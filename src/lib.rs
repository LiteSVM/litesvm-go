//! C ABI wrapper around LiteSVM, intended for consumption by Go via cgo.
//!
//! # ABI conventions
//!
//! - All handles are returned as opaque `*mut T`. They must be freed with the
//!   matching `*_free` function. Passing null to a `*_free` fn is allowed and
//!   is a no-op.
//! - Pubkeys and hashes are passed as 32-byte buffers (no length param);
//!   signatures as 64-byte buffers.
//! - Status returns: `0` = ok, non-zero = error. On error the thread-local
//!   error string is set and can be read with [`litesvm_last_error_copy`].
//!   Specific non-zero codes are implementation details, not a stable contract.
//! - Rust panics are caught at every FFI boundary and converted to a non-zero
//!   status plus a descriptive error message. They are never unwound across
//!   the C ABI.
//!
//! # Thread safety
//!
//! Handles are NOT safe for concurrent use from multiple threads. Callers
//! must serialize access to a handle from multiple goroutines.
//!
//! # Safety model
//!
//! Most FFI entry points are declared `unsafe extern "C"`. Unless documented
//! otherwise, callers must ensure:
//!
//! - Every handle argument is either null or was produced by the matching
//!   constructor in this crate and has not been freed.
//! - Every input pointer is either null or valid for reads of the size
//!   implied by its type plus any explicit length parameter, and points to
//!   properly initialized memory aligned for its pointee type.
//! - Every output pointer is either null or valid for writes of the size
//!   implied by its type, and properly aligned for its pointee type.
//! - `(ptr, len)` buffer pairs may be `(null, 0)`; the implementation
//!   promotes this to an empty slice without dereferencing `ptr`.
//! - No handle is used concurrently from multiple threads.
//!
//! Individual functions document additional preconditions in their own
//! `# Safety` sections when relevant.

#![deny(clippy::all)]
#![allow(clippy::result_large_err)]
#![warn(unsafe_op_in_unsafe_fn)]

mod native_mint;

use {
    bincode::deserialize,
    litesvm::{
        types::{FailedTransactionMetadata, SimulatedTransactionInfo, TransactionMetadata},
        LiteSVM,
    },
    solana_account::Account,
    solana_address::Address,
    solana_message::inner_instruction::InnerInstructionsList,
    solana_signature::Signature,
    solana_transaction::{versioned::VersionedTransaction, Transaction},
    std::{
        cell::RefCell,
        panic::{catch_unwind, AssertUnwindSafe},
        path::PathBuf,
        ptr, slice,
        str::from_utf8,
    },
};

thread_local! {
    static LAST_ERROR: RefCell<String> = const { RefCell::new(String::new()) };
}

fn set_error(msg: impl Into<String>) {
    LAST_ERROR.with(|e| *e.borrow_mut() = msg.into());
}

fn clear_error() {
    LAST_ERROR.with(|e| e.borrow_mut().clear());
}

/// Guard every extern fn body with this to prevent unwinding across the FFI
/// boundary. Returns `on_panic` if the closure panics, after setting the
/// thread-local error string.
fn guard<T>(on_panic: T, f: impl FnOnce() -> T) -> T {
    match catch_unwind(AssertUnwindSafe(f)) {
        Ok(v) => v,
        Err(payload) => {
            let msg = if let Some(s) = payload.downcast_ref::<&str>() {
                (*s).to_string()
            } else if let Some(s) = payload.downcast_ref::<String>() {
                s.clone()
            } else {
                "panic across FFI".to_string()
            };
            set_error(format!("panic: {msg}"));
            on_panic
        }
    }
}

// ---------------------------------------------------------------------------
// FFI helpers
// ---------------------------------------------------------------------------

/// Safely build a slice from an FFI `(ptr, len)` pair that may carry a null
/// pointer when `len == 0`.
///
/// - `len == 0` always returns an empty slice (any `ptr` is accepted,
///   including null). This avoids the UB of `slice::from_raw_parts(null, 0)`.
/// - `len > 0` with `ptr.is_null()` sets the thread-local error and returns
///   `None`.
/// - Otherwise defers to [`slice::from_raw_parts`].
///
/// # Safety
///
/// If `len > 0`, `ptr` must be aligned for `T`, point to `len` initialized
/// elements, and those elements must not be mutated for the returned lifetime.
unsafe fn slice_from_c<'a, T>(ptr: *const T, len: usize, label: &str) -> Option<&'a [T]> {
    if len == 0 {
        Some(&[])
    } else if ptr.is_null() {
        set_error(format!("null {label} pointer"));
        None
    } else {
        // SAFETY: ptr is non-null and caller guarantees the rest of the
        // from_raw_parts contract (see this function's `# Safety`).
        Some(unsafe { slice::from_raw_parts(ptr, len) })
    }
}

/// Copies bytes into `buf` subject to probe-then-copy semantics: always
/// returns the full byte length, copying `min(bytes.len(), buf_len)` when
/// `buf` is non-null and `buf_len > 0`.
///
/// # Safety
///
/// If `buf_len > 0` and `buf` is non-null, `buf` must be valid for writes of
/// at least `buf_len` bytes.
unsafe fn copy_probe(bytes: &[u8], buf: *mut u8, buf_len: usize) -> usize {
    if buf_len > 0 && !buf.is_null() && !bytes.is_empty() {
        let copy_len = bytes.len().min(buf_len);
        // SAFETY: caller guarantees `buf` is writable for `buf_len` bytes;
        // we copy at most `buf_len` of `bytes`.
        unsafe { ptr::copy_nonoverlapping(bytes.as_ptr(), buf, copy_len) };
    }
    bytes.len()
}

// ---------------------------------------------------------------------------
// Last-error accessor
// ---------------------------------------------------------------------------

/// Copies the last error string into `buf` (up to `buf_len` bytes, no NUL).
/// Returns the *full* length of the error. If it exceeds `buf_len`, the buffer
/// is truncated; callers can size their buffer accordingly and retry.
///
/// `buf` may be null if `buf_len` is 0 — useful for probing the required size.
///
/// # Safety
///
/// See the module-level safety model. If `buf_len > 0` and `buf` is non-null,
/// `buf` must be valid for writes of `buf_len` bytes.
#[no_mangle]
pub unsafe extern "C" fn litesvm_last_error_copy(buf: *mut u8, buf_len: usize) -> usize {
    guard(0, || {
        LAST_ERROR.with(|e| {
            let err = e.borrow();
            // SAFETY: forwarded precondition on (buf, buf_len).
            unsafe { copy_probe(err.as_bytes(), buf, buf_len) }
        })
    })
}

// ---------------------------------------------------------------------------
// LiteSVM handle
// ---------------------------------------------------------------------------

/// Opaque handle type. Layout is intentionally private.
pub struct LiteSvmHandle {
    inner: LiteSVM,
}

/// Create a new LiteSVM with default settings. Returns null on panic.
#[no_mangle]
pub extern "C" fn litesvm_new() -> *mut LiteSvmHandle {
    guard(ptr::null_mut(), || {
        clear_error();
        let h = Box::new(LiteSvmHandle {
            inner: LiteSVM::new(),
        });
        Box::into_raw(h)
    })
}

/// Free a handle. Null is a no-op.
///
/// # Safety
///
/// `handle` must be either null or a pointer previously returned from
/// [`litesvm_new`] and not yet freed. The pointer is invalidated on return.
#[no_mangle]
pub unsafe extern "C" fn litesvm_free(handle: *mut LiteSvmHandle) {
    if handle.is_null() {
        return;
    }
    guard((), || {
        // SAFETY: non-null here, and the caller's contract guarantees the
        // pointer came from `Box::into_raw` in `litesvm_new`.
        drop(unsafe { Box::from_raw(handle) });
    });
}

// ---------------------------------------------------------------------------
// Opaque handle deref helpers
// ---------------------------------------------------------------------------
//
// These return references with a caller-chosen lifetime `'a`. The FFI boundary
// makes it impossible to tie `'a` to anything tangible; each call site uses
// the returned reference only within one extern fn body, never storing it.

/// # Safety
///
/// `h` must be either null or a valid pointer to a `LiteSvmHandle`. The
/// returned reference must not outlive the pointee.
unsafe fn handle_ref<'a>(h: *const LiteSvmHandle) -> Option<&'a LiteSvmHandle> {
    if h.is_null() {
        set_error("null handle");
        None
    } else {
        // SAFETY: non-null and a valid pointee per caller's contract.
        Some(unsafe { &*h })
    }
}

/// # Safety
///
/// `h` must be either null or a valid pointer to a `LiteSvmHandle` with no
/// other live references. The returned reference must not outlive the pointee
/// and must be the only live reference to it for its lifetime.
unsafe fn handle_mut<'a>(h: *mut LiteSvmHandle) -> Option<&'a mut LiteSvmHandle> {
    if h.is_null() {
        set_error("null handle");
        None
    } else {
        // SAFETY: non-null and a valid, exclusively-owned pointee per caller.
        Some(unsafe { &mut *h })
    }
}

/// # Safety
///
/// `p` must be either null or a pointer to at least 32 readable bytes.
unsafe fn pubkey_from_ptr(p: *const u8) -> Option<Address> {
    if p.is_null() {
        set_error("null pubkey pointer");
        return None;
    }
    // SAFETY: non-null and 32 readable bytes per caller's contract.
    let bytes = unsafe { slice::from_raw_parts(p, 32) };
    Some(Address::try_from(bytes).expect("32-byte slice always fits Address"))
}

// ---------------------------------------------------------------------------
// Core API
// ---------------------------------------------------------------------------

/// Airdrop `lamports` to `pubkey`. `pubkey` points to 32 bytes.
/// Returns 0 on success, non-zero on error (see [`litesvm_last_error_copy`]).
///
/// # Safety
///
/// See module-level safety model. `handle` and `pubkey` must each be null or
/// valid per their types.
#[no_mangle]
pub unsafe extern "C" fn litesvm_airdrop(
    handle: *mut LiteSvmHandle,
    pubkey: *const u8,
    lamports: u64,
) -> i32 {
    guard(-1, || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_mut(handle) }) else {
            return 1;
        };
        // SAFETY: forwarded caller contract.
        let Some(pk) = (unsafe { pubkey_from_ptr(pubkey) }) else {
            return 2;
        };
        match h.inner.airdrop(&pk, lamports) {
            Ok(_) => 0,
            Err(e) => {
                set_error(format!("airdrop failed: {:?}", e.err));
                3
            }
        }
    })
}

/// Reads the balance of `pubkey` into `*out`.
/// Returns 0 if found, 1 if the account does not exist, non-zero otherwise.
///
/// # Safety
///
/// See module-level safety model. `out` must be null or a valid `*mut u64`.
#[no_mangle]
pub unsafe extern "C" fn litesvm_get_balance(
    handle: *const LiteSvmHandle,
    pubkey: *const u8,
    out: *mut u64,
) -> i32 {
    guard(-1, || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_ref(handle) }) else {
            return 2;
        };
        // SAFETY: forwarded caller contract.
        let Some(pk) = (unsafe { pubkey_from_ptr(pubkey) }) else {
            return 3;
        };
        if out.is_null() {
            set_error("null out pointer");
            return 4;
        }
        match h.inner.get_balance(&pk) {
            Some(v) => {
                // SAFETY: `out` is non-null and is valid for writes of `u64`
                // per caller's contract.
                unsafe { ptr::write(out, v) };
                0
            }
            None => 1,
        }
    })
}

/// Writes the 32-byte latest blockhash to `out`.
/// Returns 0 on success.
///
/// # Safety
///
/// See module-level safety model. `out` must be null or point to at least
/// 32 writable bytes.
#[no_mangle]
pub unsafe extern "C" fn litesvm_latest_blockhash(
    handle: *const LiteSvmHandle,
    out: *mut u8,
) -> i32 {
    guard(-1, || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_ref(handle) }) else {
            return 1;
        };
        if out.is_null() {
            set_error("null out pointer");
            return 2;
        }
        let hash = h.inner.latest_blockhash();
        // SAFETY: `out` is non-null and is valid for 32 writable bytes per
        // caller's contract.
        unsafe { ptr::copy_nonoverlapping(hash.to_bytes().as_ptr(), out, 32) };
        0
    })
}

/// Expires the current blockhash so the next [`litesvm_latest_blockhash`]
/// returns a new value. Returns 0 on success.
///
/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_expire_blockhash(handle: *mut LiteSvmHandle) -> i32 {
    guard(-1, || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_mut(handle) }) else {
            return 1;
        };
        h.inner.expire_blockhash();
        0
    })
}

/// Version string of this wrapper crate. Returns a static NUL-terminated
/// pointer; do not free.
#[no_mangle]
pub extern "C" fn litesvm_version() -> *const u8 {
    concat!(env!("CARGO_PKG_VERSION"), "\0").as_ptr()
}

// ---------------------------------------------------------------------------
// Transaction outcome (unified success + failure)
// ---------------------------------------------------------------------------

/// Opaque handle holding the result of a send_transaction or
/// simulate_transaction call. Always carries metadata (signature, logs,
/// CUs, fee, return_data); `error` is populated iff the tx failed;
/// `post_accounts` is populated only for successful simulations.
pub struct LiteSvmTxOutcome {
    signature: [u8; 64],
    logs: Vec<String>,
    compute_units_consumed: u64,
    fee: u64,
    error: Option<String>,
    return_data_program_id: [u8; 32],
    return_data: Vec<u8>,
    post_accounts: Vec<(Address, Account)>,
    inner_instructions: InnerInstructionsList,
}

impl LiteSvmTxOutcome {
    fn from_success(meta: TransactionMetadata) -> Self {
        Self {
            signature: meta.signature.into(),
            logs: meta.logs,
            compute_units_consumed: meta.compute_units_consumed,
            fee: meta.fee,
            error: None,
            return_data_program_id: meta.return_data.program_id.to_bytes(),
            return_data: meta.return_data.data,
            post_accounts: Vec::new(),
            inner_instructions: meta.inner_instructions,
        }
    }

    fn from_failure(failed: FailedTransactionMetadata) -> Self {
        Self {
            signature: failed.meta.signature.into(),
            logs: failed.meta.logs,
            compute_units_consumed: failed.meta.compute_units_consumed,
            fee: failed.meta.fee,
            error: Some(format!("{:?}", failed.err)),
            return_data_program_id: failed.meta.return_data.program_id.to_bytes(),
            return_data: failed.meta.return_data.data,
            post_accounts: Vec::new(),
            inner_instructions: failed.meta.inner_instructions,
        }
    }

    fn from_sim_success(info: SimulatedTransactionInfo) -> Self {
        let mut out = Self::from_success(info.meta);
        out.post_accounts = info
            .post_accounts
            .into_iter()
            .map(|(addr, asd)| (addr, Account::from(asd)))
            .collect();
        out
    }
}

/// Deserialize a bincode-encoded legacy `Transaction` and submit it.
/// Returns a heap-allocated `LiteSvmTxOutcome` (free with
/// [`litesvm_tx_outcome_free`]) on both success and failure, or null on
/// deserialization / internal error (inspect [`litesvm_last_error_copy`]).
///
/// # Safety
///
/// See module-level safety model. `(tx_bytes, tx_len)` may be `(null, 0)`
/// but will fail to decode.
#[no_mangle]
pub unsafe extern "C" fn litesvm_send_legacy_transaction(
    handle: *mut LiteSvmHandle,
    tx_bytes: *const u8,
    tx_len: usize,
) -> *mut LiteSvmTxOutcome {
    guard(ptr::null_mut(), || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_mut(handle) }) else {
            return ptr::null_mut();
        };
        // SAFETY: forwarded caller contract; helper tolerates (null, 0).
        let Some(bytes) = (unsafe { slice_from_c(tx_bytes, tx_len, "tx bytes") }) else {
            return ptr::null_mut();
        };
        let tx: Transaction = match deserialize(bytes) {
            Ok(t) => t,
            Err(e) => {
                set_error(format!("failed to decode Transaction: {e}"));
                return ptr::null_mut();
            }
        };
        let outcome = match h.inner.send_transaction(tx) {
            Ok(meta) => LiteSvmTxOutcome::from_success(meta),
            Err(failed) => LiteSvmTxOutcome::from_failure(failed),
        };
        Box::into_raw(Box::new(outcome))
    })
}

/// Free a tx-outcome handle. Null is a no-op.
///
/// # Safety
///
/// `handle` must be null or a pointer previously returned from one of the
/// transaction / simulation / `litesvm_get_transaction` entry points, not
/// yet freed. The pointer is invalidated on return.
#[no_mangle]
pub unsafe extern "C" fn litesvm_tx_outcome_free(handle: *mut LiteSvmTxOutcome) {
    if handle.is_null() {
        return;
    }
    guard((), || {
        // SAFETY: non-null and the pointer came from `Box::into_raw`.
        drop(unsafe { Box::from_raw(handle) });
    });
}

/// # Safety
///
/// `h` must be either null or a valid pointer to a `LiteSvmTxOutcome`.
unsafe fn outcome_ref<'a>(h: *const LiteSvmTxOutcome) -> Option<&'a LiteSvmTxOutcome> {
    if h.is_null() {
        set_error("null tx outcome handle");
        None
    } else {
        // SAFETY: non-null, valid pointee per caller's contract.
        Some(unsafe { &*h })
    }
}

/// Returns 1 if the transaction succeeded, 0 if it failed. -1 if handle is null.
///
/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_tx_outcome_is_ok(handle: *const LiteSvmTxOutcome) -> i32 {
    guard(-1, || {
        // SAFETY: forwarded caller contract.
        let Some(o) = (unsafe { outcome_ref(handle) }) else {
            return -1;
        };
        i32::from(o.error.is_none())
    })
}

/// Writes the 64-byte signature to `out`. Returns 0 on success.
///
/// # Safety
///
/// See module-level safety model. `out` must be null or point to at least
/// 64 writable bytes.
#[no_mangle]
pub unsafe extern "C" fn litesvm_tx_outcome_signature(
    handle: *const LiteSvmTxOutcome,
    out: *mut u8,
) -> i32 {
    guard(-1, || {
        // SAFETY: forwarded caller contract.
        let Some(o) = (unsafe { outcome_ref(handle) }) else {
            return 1;
        };
        if out.is_null() {
            set_error("null out pointer");
            return 2;
        }
        // SAFETY: `out` is non-null and is valid for 64 writable bytes per
        // caller's contract.
        unsafe { ptr::copy_nonoverlapping(o.signature.as_ptr(), out, 64) };
        0
    })
}

/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_tx_outcome_compute_units(handle: *const LiteSvmTxOutcome) -> u64 {
    // SAFETY: forwarded caller contract.
    guard(0, || {
        unsafe { outcome_ref(handle) }.map_or(0, |o| o.compute_units_consumed)
    })
}

/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_tx_outcome_fee(handle: *const LiteSvmTxOutcome) -> u64 {
    // SAFETY: forwarded caller contract.
    guard(0, || unsafe { outcome_ref(handle) }.map_or(0, |o| o.fee))
}

/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_tx_outcome_logs_count(handle: *const LiteSvmTxOutcome) -> usize {
    // SAFETY: forwarded caller contract.
    guard(0, || {
        unsafe { outcome_ref(handle) }.map_or(0, |o| o.logs.len())
    })
}

/// Copies the `idx`-th log line into `buf`. Returns the full byte length of
/// the line (may exceed `buf_len` - caller can resize and retry). Returns 0
/// with error set if `idx` is out of range.
///
/// # Safety
///
/// See module-level safety model. If `buf_len > 0` and `buf` is non-null,
/// `buf` must be valid for writes of `buf_len` bytes.
#[no_mangle]
pub unsafe extern "C" fn litesvm_tx_outcome_log_copy(
    handle: *const LiteSvmTxOutcome,
    idx: usize,
    buf: *mut u8,
    buf_len: usize,
) -> usize {
    guard(0, || {
        // SAFETY: forwarded caller contract.
        let Some(o) = (unsafe { outcome_ref(handle) }) else {
            return 0;
        };
        let Some(line) = o.logs.get(idx) else {
            set_error(format!("log index out of range: {idx}"));
            return 0;
        };
        // SAFETY: forwarded caller contract on (buf, buf_len).
        unsafe { copy_probe(line.as_bytes(), buf, buf_len) }
    })
}

/// Copies the debug-rendered transaction error into `buf`. Returns the full
/// byte length; if the tx succeeded, length is 0.
///
/// # Safety
///
/// See module-level safety model. If `buf_len > 0` and `buf` is non-null,
/// `buf` must be valid for writes of `buf_len` bytes.
#[no_mangle]
pub unsafe extern "C" fn litesvm_tx_outcome_error_copy(
    handle: *const LiteSvmTxOutcome,
    buf: *mut u8,
    buf_len: usize,
) -> usize {
    guard(0, || {
        // SAFETY: forwarded caller contract.
        let Some(o) = (unsafe { outcome_ref(handle) }) else {
            return 0;
        };
        let Some(err) = &o.error else {
            return 0;
        };
        // SAFETY: forwarded caller contract on (buf, buf_len).
        unsafe { copy_probe(err.as_bytes(), buf, buf_len) }
    })
}

/// Writes the 32-byte return-data program id to `out`. Returns 1 if the
/// transaction produced return data, 0 if it did not, negative on error.
///
/// Note: an explicit `set_return_data(&[])` call and "no return data set at
/// all" are indistinguishable here — both report 0 — mirroring upstream
/// `TransactionMetadata` semantics.
///
/// # Safety
///
/// See module-level safety model. `out` must be null or point to at least
/// 32 writable bytes.
#[no_mangle]
pub unsafe extern "C" fn litesvm_tx_outcome_return_data_program_id(
    handle: *const LiteSvmTxOutcome,
    out: *mut u8,
) -> i32 {
    guard(-1, || {
        // SAFETY: forwarded caller contract.
        let Some(o) = (unsafe { outcome_ref(handle) }) else {
            return -1;
        };
        if out.is_null() {
            set_error("null out pointer");
            return -2;
        }
        if o.return_data.is_empty() {
            return 0;
        }
        // SAFETY: `out` is non-null and valid for 32 writable bytes per caller.
        unsafe { ptr::copy_nonoverlapping(o.return_data_program_id.as_ptr(), out, 32) };
        1
    })
}

/// Returns the length of the return data (0 if absent).
///
/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_tx_outcome_return_data_len(
    handle: *const LiteSvmTxOutcome,
) -> usize {
    // SAFETY: forwarded caller contract.
    guard(0, || {
        unsafe { outcome_ref(handle) }.map_or(0, |o| o.return_data.len())
    })
}

/// # Safety
///
/// See module-level safety model. If `buf_len > 0` and `buf` is non-null,
/// `buf` must be valid for writes of `buf_len` bytes.
#[no_mangle]
pub unsafe extern "C" fn litesvm_tx_outcome_return_data_copy(
    handle: *const LiteSvmTxOutcome,
    buf: *mut u8,
    buf_len: usize,
) -> usize {
    guard(0, || {
        // SAFETY: forwarded caller contract.
        let Some(o) = (unsafe { outcome_ref(handle) }) else {
            return 0;
        };
        // SAFETY: forwarded caller contract on (buf, buf_len).
        unsafe { copy_probe(&o.return_data, buf, buf_len) }
    })
}

/// Number of (address, account) pairs returned from a successful simulation.
/// Zero for send_transaction outcomes and for failed simulations.
///
/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_tx_outcome_post_accounts_count(
    handle: *const LiteSvmTxOutcome,
) -> usize {
    // SAFETY: forwarded caller contract.
    guard(0, || {
        unsafe { outcome_ref(handle) }.map_or(0, |o| o.post_accounts.len())
    })
}

/// Writes the idx-th post-account's 32-byte address into `out_address` and its
/// encoded bytes into `out_buf` (probe-then-copy; same encoding as
/// [`litesvm_get_account`]). Returns 0 on success, negative on error or an
/// out-of-range index.
///
/// # Safety
///
/// See module-level safety model. `out_address` must point to 32 writable
/// bytes; `out_written` must be non-null; if `out_buf_len > 0`, `out_buf` must
/// be valid for `out_buf_len` writes.
#[no_mangle]
pub unsafe extern "C" fn litesvm_tx_outcome_post_account_at(
    handle: *const LiteSvmTxOutcome,
    idx: usize,
    out_address: *mut u8,
    out_buf: *mut u8,
    out_buf_len: usize,
    out_written: *mut usize,
) -> i32 {
    guard(-1, || {
        if out_written.is_null() {
            set_error("null out_written pointer");
            return -2;
        }
        // SAFETY: non-null per check.
        unsafe { ptr::write(out_written, 0) };
        // SAFETY: forwarded caller contract.
        let Some(o) = (unsafe { outcome_ref(handle) }) else {
            return -3;
        };
        if out_address.is_null() {
            set_error("null out_address pointer");
            return -4;
        }
        let Some((addr, acct)) = o.post_accounts.get(idx) else {
            set_error(format!("post_account index out of range: {idx}"));
            return -5;
        };
        // SAFETY: `out_address` is valid for 32 writable bytes per caller.
        unsafe { ptr::copy_nonoverlapping(addr.to_bytes().as_ptr(), out_address, 32) };
        let enc = encode_account(acct);
        // SAFETY: forwarded caller contract on (out_buf, out_buf_len).
        let n = unsafe { copy_probe(&enc, out_buf, out_buf_len) };
        // SAFETY: `out_written` non-null per earlier check.
        unsafe { ptr::write(out_written, n) };
        0
    })
}

// ---------------------------------------------------------------------------
// TxOutcome inner instructions (2D: Vec<Vec<InnerInstruction>>)
// ---------------------------------------------------------------------------
//
// inner_instructions[outer_idx] is the list of CPIs invoked by the
// outer_idx-th top-level instruction. Each entry is:
//
//     InnerInstruction {
//         instruction: CompiledInstruction {
//             program_id_index: u8, accounts: Vec<u8>, data: Vec<u8>,
//         },
//         stack_height: u8,
//     }

/// Number of top-level instructions that have an inner-instruction list.
/// This usually equals the number of top-level instructions in the tx.
///
/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_tx_outcome_inner_outer_count(
    handle: *const LiteSvmTxOutcome,
) -> usize {
    // SAFETY: forwarded caller contract.
    guard(0, || {
        unsafe { outcome_ref(handle) }.map_or(0, |o| o.inner_instructions.len())
    })
}

/// Number of inner instructions for the `outer_idx`-th top-level instruction.
/// Returns 0 and sets the thread-local error if `outer_idx` is out of range.
///
/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_tx_outcome_inner_inner_count(
    handle: *const LiteSvmTxOutcome,
    outer_idx: usize,
) -> usize {
    guard(0, || {
        // SAFETY: forwarded caller contract.
        let Some(o) = (unsafe { outcome_ref(handle) }) else {
            return 0;
        };
        let Some(inners) = o.inner_instructions.get(outer_idx) else {
            set_error(format!("outer_idx out of range: {outer_idx}"));
            return 0;
        };
        inners.len()
    })
}

fn inner_ix(
    o: &LiteSvmTxOutcome,
    outer: usize,
    inner: usize,
) -> Option<&solana_message::inner_instruction::InnerInstruction> {
    match o.inner_instructions.get(outer) {
        None => {
            set_error(format!("outer_idx out of range: {outer}"));
            None
        }
        Some(list) => match list.get(inner) {
            None => {
                set_error(format!("inner_idx out of range: {inner}"));
                None
            }
            Some(ix) => Some(ix),
        },
    }
}

/// Returns `program_id_index` as i32 (the underlying type is u8). -1 on error.
///
/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_tx_outcome_inner_program_id_index(
    handle: *const LiteSvmTxOutcome,
    outer_idx: usize,
    inner_idx: usize,
) -> i32 {
    guard(-1, || {
        // SAFETY: forwarded caller contract.
        let Some(o) = (unsafe { outcome_ref(handle) }) else {
            return -1;
        };
        let Some(ix) = inner_ix(o, outer_idx, inner_idx) else {
            return -1;
        };
        i32::from(ix.instruction.program_id_index)
    })
}

/// Returns `stack_height` as i32 (the underlying type is u8). -1 on error.
///
/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_tx_outcome_inner_stack_height(
    handle: *const LiteSvmTxOutcome,
    outer_idx: usize,
    inner_idx: usize,
) -> i32 {
    guard(-1, || {
        // SAFETY: forwarded caller contract.
        let Some(o) = (unsafe { outcome_ref(handle) }) else {
            return -1;
        };
        let Some(ix) = inner_ix(o, outer_idx, inner_idx) else {
            return -1;
        };
        i32::from(ix.stack_height)
    })
}

/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_tx_outcome_inner_accounts_len(
    handle: *const LiteSvmTxOutcome,
    outer_idx: usize,
    inner_idx: usize,
) -> usize {
    guard(0, || {
        // SAFETY: forwarded caller contract.
        let Some(o) = (unsafe { outcome_ref(handle) }) else {
            return 0;
        };
        inner_ix(o, outer_idx, inner_idx).map_or(0, |ix| ix.instruction.accounts.len())
    })
}

/// Probe-then-copy: returns the full accounts length; if `buf_len` < length,
/// `buf` is truncated.
///
/// # Safety
///
/// See module-level safety model. If `buf_len > 0` and `buf` is non-null,
/// `buf` must be valid for writes of `buf_len` bytes.
#[no_mangle]
pub unsafe extern "C" fn litesvm_tx_outcome_inner_accounts_copy(
    handle: *const LiteSvmTxOutcome,
    outer_idx: usize,
    inner_idx: usize,
    buf: *mut u8,
    buf_len: usize,
) -> usize {
    guard(0, || {
        // SAFETY: forwarded caller contract.
        let Some(o) = (unsafe { outcome_ref(handle) }) else {
            return 0;
        };
        let Some(ix) = inner_ix(o, outer_idx, inner_idx) else {
            return 0;
        };
        // SAFETY: forwarded caller contract on (buf, buf_len).
        unsafe { copy_probe(&ix.instruction.accounts, buf, buf_len) }
    })
}

/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_tx_outcome_inner_data_len(
    handle: *const LiteSvmTxOutcome,
    outer_idx: usize,
    inner_idx: usize,
) -> usize {
    guard(0, || {
        // SAFETY: forwarded caller contract.
        let Some(o) = (unsafe { outcome_ref(handle) }) else {
            return 0;
        };
        inner_ix(o, outer_idx, inner_idx).map_or(0, |ix| ix.instruction.data.len())
    })
}

/// # Safety
///
/// See module-level safety model. If `buf_len > 0` and `buf` is non-null,
/// `buf` must be valid for writes of `buf_len` bytes.
#[no_mangle]
pub unsafe extern "C" fn litesvm_tx_outcome_inner_data_copy(
    handle: *const LiteSvmTxOutcome,
    outer_idx: usize,
    inner_idx: usize,
    buf: *mut u8,
    buf_len: usize,
) -> usize {
    guard(0, || {
        // SAFETY: forwarded caller contract.
        let Some(o) = (unsafe { outcome_ref(handle) }) else {
            return 0;
        };
        let Some(ix) = inner_ix(o, outer_idx, inner_idx) else {
            return 0;
        };
        // SAFETY: forwarded caller contract on (buf, buf_len).
        unsafe { copy_probe(&ix.instruction.data, buf, buf_len) }
    })
}

// ---------------------------------------------------------------------------
// Versioned + simulate + transaction history + warp_to_slot
// ---------------------------------------------------------------------------

/// See [`litesvm_send_legacy_transaction`]; same semantics but decodes a
/// `VersionedTransaction`.
///
/// # Safety
///
/// See module-level safety model. `(tx_bytes, tx_len)` may be `(null, 0)`.
#[no_mangle]
pub unsafe extern "C" fn litesvm_send_versioned_transaction(
    handle: *mut LiteSvmHandle,
    tx_bytes: *const u8,
    tx_len: usize,
) -> *mut LiteSvmTxOutcome {
    guard(ptr::null_mut(), || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_mut(handle) }) else {
            return ptr::null_mut();
        };
        // SAFETY: forwarded caller contract; helper tolerates (null, 0).
        let Some(bytes) = (unsafe { slice_from_c(tx_bytes, tx_len, "tx bytes") }) else {
            return ptr::null_mut();
        };
        let tx: VersionedTransaction = match deserialize(bytes) {
            Ok(t) => t,
            Err(e) => {
                set_error(format!("failed to decode VersionedTransaction: {e}"));
                return ptr::null_mut();
            }
        };
        let outcome = match h.inner.send_transaction(tx) {
            Ok(meta) => LiteSvmTxOutcome::from_success(meta),
            Err(failed) => LiteSvmTxOutcome::from_failure(failed),
        };
        Box::into_raw(Box::new(outcome))
    })
}

/// Simulate a bincode-encoded legacy Transaction. Returns a new TxOutcome
/// (free with [`litesvm_tx_outcome_free`]); on success `post_accounts` is
/// populated. Returns null on deserialization / internal error.
///
/// # Safety
///
/// See module-level safety model. `(tx_bytes, tx_len)` may be `(null, 0)`.
#[no_mangle]
pub unsafe extern "C" fn litesvm_simulate_legacy_transaction(
    handle: *mut LiteSvmHandle,
    tx_bytes: *const u8,
    tx_len: usize,
) -> *mut LiteSvmTxOutcome {
    guard(ptr::null_mut(), || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_mut(handle) }) else {
            return ptr::null_mut();
        };
        // SAFETY: forwarded caller contract; helper tolerates (null, 0).
        let Some(bytes) = (unsafe { slice_from_c(tx_bytes, tx_len, "tx bytes") }) else {
            return ptr::null_mut();
        };
        let tx: Transaction = match deserialize(bytes) {
            Ok(t) => t,
            Err(e) => {
                set_error(format!("failed to decode Transaction: {e}"));
                return ptr::null_mut();
            }
        };
        let outcome = match h.inner.simulate_transaction(tx) {
            Ok(info) => LiteSvmTxOutcome::from_sim_success(info),
            Err(failed) => LiteSvmTxOutcome::from_failure(failed),
        };
        Box::into_raw(Box::new(outcome))
    })
}

/// See [`litesvm_simulate_legacy_transaction`]; decodes a
/// `VersionedTransaction`.
///
/// # Safety
///
/// See module-level safety model. `(tx_bytes, tx_len)` may be `(null, 0)`.
#[no_mangle]
pub unsafe extern "C" fn litesvm_simulate_versioned_transaction(
    handle: *mut LiteSvmHandle,
    tx_bytes: *const u8,
    tx_len: usize,
) -> *mut LiteSvmTxOutcome {
    guard(ptr::null_mut(), || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_mut(handle) }) else {
            return ptr::null_mut();
        };
        // SAFETY: forwarded caller contract; helper tolerates (null, 0).
        let Some(bytes) = (unsafe { slice_from_c(tx_bytes, tx_len, "tx bytes") }) else {
            return ptr::null_mut();
        };
        let tx: VersionedTransaction = match deserialize(bytes) {
            Ok(t) => t,
            Err(e) => {
                set_error(format!("failed to decode VersionedTransaction: {e}"));
                return ptr::null_mut();
            }
        };
        let outcome = match h.inner.simulate_transaction(tx) {
            Ok(info) => LiteSvmTxOutcome::from_sim_success(info),
            Err(failed) => LiteSvmTxOutcome::from_failure(failed),
        };
        Box::into_raw(Box::new(outcome))
    })
}

/// Warps the internal clock to `slot`.
///
/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_warp_to_slot(handle: *mut LiteSvmHandle, slot: u64) -> i32 {
    guard(-1, || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_mut(handle) }) else {
            return 1;
        };
        h.inner.warp_to_slot(slot);
        0
    })
}

/// Looks up a transaction by 64-byte signature in the transaction history.
/// Returns a newly-allocated TxOutcome handle (free with
/// [`litesvm_tx_outcome_free`]), or null if the signature is not in history.
///
/// # Safety
///
/// See module-level safety model. `signature` must be null or point to at
/// least 64 readable bytes.
#[no_mangle]
pub unsafe extern "C" fn litesvm_get_transaction(
    handle: *const LiteSvmHandle,
    signature: *const u8,
) -> *mut LiteSvmTxOutcome {
    guard(ptr::null_mut(), || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_ref(handle) }) else {
            return ptr::null_mut();
        };
        if signature.is_null() {
            set_error("null signature pointer");
            return ptr::null_mut();
        }
        // SAFETY: non-null and 64 readable bytes per caller's contract.
        let sig_bytes = unsafe { slice::from_raw_parts(signature, 64) };
        let sig = match Signature::try_from(sig_bytes) {
            Ok(s) => s,
            Err(e) => {
                set_error(format!("invalid signature: {e}"));
                return ptr::null_mut();
            }
        };
        let Some(res) = h.inner.get_transaction(&sig) else {
            return ptr::null_mut();
        };
        let outcome = match res.clone() {
            Ok(meta) => LiteSvmTxOutcome::from_success(meta),
            Err(failed) => LiteSvmTxOutcome::from_failure(failed),
        };
        Box::into_raw(Box::new(outcome))
    })
}

// ---------------------------------------------------------------------------
// Configuration setters
// ---------------------------------------------------------------------------

/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_set_sigverify(handle: *mut LiteSvmHandle, enabled: bool) -> i32 {
    guard(-1, || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_mut(handle) }) else {
            return 1;
        };
        h.inner.set_sigverify(enabled);
        0
    })
}

/// Returns 1 if sigverify is enabled, 0 if not, -1 on error.
///
/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_get_sigverify(handle: *const LiteSvmHandle) -> i32 {
    guard(-1, || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_ref(handle) }) else {
            return -1;
        };
        i32::from(h.inner.get_sigverify())
    })
}

/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_set_blockhash_check(
    handle: *mut LiteSvmHandle,
    enabled: bool,
) -> i32 {
    guard(-1, || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_mut(handle) }) else {
            return 1;
        };
        h.inner.set_blockhash_check(enabled);
        0
    })
}

/// Set transaction-history capacity. Pass 0 to disable dedup and allow
/// resubmission of identical transactions.
///
/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_set_transaction_history(
    handle: *mut LiteSvmHandle,
    capacity: usize,
) -> i32 {
    guard(-1, || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_mut(handle) }) else {
            return 1;
        };
        h.inner.set_transaction_history(capacity);
        0
    })
}

/// Set the per-transaction log byte limit. `has_limit == false` means unlimited.
///
/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_set_log_bytes_limit(
    handle: *mut LiteSvmHandle,
    has_limit: bool,
    limit: usize,
) -> i32 {
    guard(-1, || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_mut(handle) }) else {
            return 1;
        };
        h.inner
            .set_log_bytes_limit(if has_limit { Some(limit) } else { None });
        0
    })
}

/// Set the balance of the internal airdrop pool account.
///
/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_set_lamports(handle: *mut LiteSvmHandle, lamports: u64) -> i32 {
    guard(-1, || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_mut(handle) }) else {
            return 1;
        };
        h.inner.set_lamports(lamports);
        0
    })
}

/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_set_sysvars(handle: *mut LiteSvmHandle) -> i32 {
    guard(-1, || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_mut(handle) }) else {
            return 1;
        };
        h.inner.set_sysvars();
        0
    })
}

/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_set_builtins(handle: *mut LiteSvmHandle) -> i32 {
    guard(-1, || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_mut(handle) }) else {
            return 1;
        };
        h.inner.set_builtins();
        0
    })
}

/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_set_default_programs(handle: *mut LiteSvmHandle) -> i32 {
    guard(-1, || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_mut(handle) }) else {
            return 1;
        };
        h.inner.set_default_programs();
        0
    })
}

/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_set_precompiles(handle: *mut LiteSvmHandle) -> i32 {
    guard(-1, || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_mut(handle) }) else {
            return 1;
        };
        h.inner.set_precompiles();
        0
    })
}

/// Seeds the SPL Token / Token-2022 native-mint accounts, if the matching
/// program is present in the accounts DB. No-op for programs that aren't loaded.
///
/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_with_native_mints(handle: *mut LiteSvmHandle) -> i32 {
    guard(-1, || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_mut(handle) }) else {
            return 1;
        };
        if h.inner
            .accounts_db()
            .inner
            .contains_key(&native_mint::inline_spl::SPL_TOKEN_PROGRAM_ID)
        {
            native_mint::create_native_mint(&mut h.inner);
        }
        if h.inner
            .accounts_db()
            .inner
            .contains_key(&native_mint::inline_spl::SPL_TOKEN_2022_PROGRAM_ID)
        {
            native_mint::create_native_mint_2022(&mut h.inner);
        }
        0
    })
}

/// # Safety
///
/// See module-level safety model. `(bytes, bytes_len)` may be `(null, 0)` but
/// that will fail validation.
#[no_mangle]
pub unsafe extern "C" fn litesvm_add_program_with_loader(
    handle: *mut LiteSvmHandle,
    program_id: *const u8,
    bytes: *const u8,
    bytes_len: usize,
    loader_id: *const u8,
) -> i32 {
    guard(-1, || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_mut(handle) }) else {
            return 1;
        };
        // SAFETY: forwarded caller contract.
        let Some(pid) = (unsafe { pubkey_from_ptr(program_id) }) else {
            return 2;
        };
        // SAFETY: forwarded caller contract.
        let Some(lid) = (unsafe { pubkey_from_ptr(loader_id) }) else {
            return 3;
        };
        // SAFETY: forwarded caller contract; helper tolerates (null, 0).
        let Some(program_bytes) = (unsafe { slice_from_c(bytes, bytes_len, "program bytes") })
        else {
            return 4;
        };
        if program_bytes.is_empty() {
            set_error("empty program bytes");
            return 4;
        }
        match h.inner.add_program_with_loader(pid, program_bytes, lid) {
            Ok(()) => 0,
            Err(e) => {
                set_error(format!("add_program_with_loader failed: {e}"));
                5
            }
        }
    })
}

// ---------------------------------------------------------------------------
// Account wire codec
//
// Accounts cross the FFI as a fixed-header byte blob; the typed Account value
// lives on the Go side. Layout (little-endian):
//   [lamports: u64][rent_epoch: u64][executable: u8][owner: 32][data: rest]
// ---------------------------------------------------------------------------

const ACCOUNT_HEADER_LEN: usize = 8 + 8 + 1 + 32;

fn encode_account(a: &Account) -> Vec<u8> {
    let mut out = Vec::with_capacity(ACCOUNT_HEADER_LEN + a.data.len());
    out.extend_from_slice(&a.lamports.to_le_bytes());
    out.extend_from_slice(&a.rent_epoch.to_le_bytes());
    out.push(u8::from(a.executable));
    out.extend_from_slice(a.owner.to_bytes().as_ref());
    out.extend_from_slice(&a.data);
    out
}

fn decode_account(b: &[u8]) -> Result<Account, String> {
    if b.len() < ACCOUNT_HEADER_LEN {
        return Err(format!(
            "account: short buffer ({} bytes, need >= {ACCOUNT_HEADER_LEN})",
            b.len()
        ));
    }
    let lamports = u64::from_le_bytes(b[0..8].try_into().expect("8 bytes"));
    let rent_epoch = u64::from_le_bytes(b[8..16].try_into().expect("8 bytes"));
    let executable = b[16] != 0;
    let owner = Address::try_from(&b[17..ACCOUNT_HEADER_LEN])
        .map_err(|_| "account: bad owner".to_string())?;
    Ok(Account {
        lamports,
        data: b[ACCOUNT_HEADER_LEN..].to_vec(),
        owner,
        executable,
        rent_epoch,
    })
}

// ---------------------------------------------------------------------------
// SVM account + program operations
// ---------------------------------------------------------------------------

/// Writes the encoded account at `pubkey` into `out_buf` (probe-then-copy: pass
/// `(null, 0)` to learn the length via `*out_written`, then call again with a
/// buffer of at least that size). Returns 0 if the account exists, 1 if it does
/// not, negative on error.
///
/// # Safety
///
/// See module-level safety model. `out_written` must be non-null; if
/// `out_buf_len > 0`, `out_buf` must be valid for `out_buf_len` writes.
#[no_mangle]
pub unsafe extern "C" fn litesvm_get_account(
    handle: *const LiteSvmHandle,
    pubkey: *const u8,
    out_buf: *mut u8,
    out_buf_len: usize,
    out_written: *mut usize,
) -> i32 {
    guard(-1, || {
        clear_error();
        if out_written.is_null() {
            set_error("null out_written pointer");
            return -2;
        }
        // SAFETY: non-null per check.
        unsafe { ptr::write(out_written, 0) };
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_ref(handle) }) else {
            return -3;
        };
        // SAFETY: forwarded caller contract.
        let Some(pk) = (unsafe { pubkey_from_ptr(pubkey) }) else {
            return -4;
        };
        match h.inner.get_account(&pk) {
            Some(acct) => {
                let enc = encode_account(&acct);
                // SAFETY: forwarded caller contract on (out_buf, out_buf_len).
                let n = unsafe { copy_probe(&enc, out_buf, out_buf_len) };
                // SAFETY: `out_written` non-null per earlier check.
                unsafe { ptr::write(out_written, n) };
                0
            }
            None => 1,
        }
    })
}

/// Stores the account encoded in `(data, data_len)` at `pubkey`. The encoding
/// matches [`litesvm_get_account`]'s output.
///
/// # Safety
///
/// See module-level safety model. `(data, data_len)` must describe a valid
/// slice (at least the fixed account header).
#[no_mangle]
pub unsafe extern "C" fn litesvm_set_account(
    handle: *mut LiteSvmHandle,
    pubkey: *const u8,
    data: *const u8,
    data_len: usize,
) -> i32 {
    guard(-1, || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_mut(handle) }) else {
            return 1;
        };
        // SAFETY: forwarded caller contract.
        let Some(pk) = (unsafe { pubkey_from_ptr(pubkey) }) else {
            return 2;
        };
        // SAFETY: forwarded caller contract; tolerates (null, 0).
        let Some(bytes) = (unsafe { slice_from_c(data, data_len, "account data") }) else {
            return 3;
        };
        let acct = match decode_account(bytes) {
            Ok(a) => a,
            Err(e) => {
                set_error(e);
                return 4;
            }
        };
        match h.inner.set_account(pk, acct) {
            Ok(()) => 0,
            Err(e) => {
                set_error(format!("set_account failed: {e}"));
                5
            }
        }
    })
}

/// Adds an SBF program to the environment from an in-memory byte buffer.
///
/// # Safety
///
/// See module-level safety model. `(bytes, bytes_len)` may be `(null, 0)`
/// but that will fail validation.
#[no_mangle]
pub unsafe extern "C" fn litesvm_add_program(
    handle: *mut LiteSvmHandle,
    program_id: *const u8,
    bytes: *const u8,
    bytes_len: usize,
) -> i32 {
    guard(-1, || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_mut(handle) }) else {
            return 1;
        };
        // SAFETY: forwarded caller contract.
        let Some(pid) = (unsafe { pubkey_from_ptr(program_id) }) else {
            return 2;
        };
        // SAFETY: forwarded caller contract; helper tolerates (null, 0).
        let Some(program_bytes) = (unsafe { slice_from_c(bytes, bytes_len, "program bytes") })
        else {
            return 3;
        };
        if program_bytes.is_empty() {
            set_error("empty program bytes");
            return 3;
        }
        match h.inner.add_program(pid, program_bytes) {
            Ok(()) => 0,
            Err(e) => {
                set_error(format!("add_program failed: {e}"));
                4
            }
        }
    })
}

/// Adds an SBF program loaded from the file at `path_utf8` (length `path_len`,
/// UTF-8, no trailing NUL required).
///
/// # Safety
///
/// See module-level safety model. `(path_utf8, path_len)` may be `(null, 0)`
/// but an empty path will fail validation.
#[no_mangle]
pub unsafe extern "C" fn litesvm_add_program_from_file(
    handle: *mut LiteSvmHandle,
    program_id: *const u8,
    path_utf8: *const u8,
    path_len: usize,
) -> i32 {
    guard(-1, || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_mut(handle) }) else {
            return 1;
        };
        // SAFETY: forwarded caller contract.
        let Some(pid) = (unsafe { pubkey_from_ptr(program_id) }) else {
            return 2;
        };
        // SAFETY: forwarded caller contract; helper tolerates (null, 0).
        let Some(path_bytes) = (unsafe { slice_from_c(path_utf8, path_len, "path") }) else {
            return 3;
        };
        if path_bytes.is_empty() {
            set_error("empty path");
            return 3;
        }
        let path_str = match from_utf8(path_bytes) {
            Ok(s) => s,
            Err(e) => {
                set_error(format!("path is not valid UTF-8: {e}"));
                return 4;
            }
        };
        match h.inner.add_program_from_file(pid, PathBuf::from(path_str)) {
            Ok(()) => 0,
            Err(e) => {
                set_error(format!("add_program_from_file failed: {e}"));
                5
            }
        }
    })
}

// ---------------------------------------------------------------------------
// Sysvars (generic bincode byte-exchange, dispatched by account id)
//
// The typed sysvar structs live on the Go side (solana-go's `sysvar` package);
// the FFI only shuttles the bincode-encoded account data, which is identical to
// the on-chain sysvar account layout. `get` serializes the bank's current
// sysvar; `set` deserializes and installs it (refreshing the sysvar cache via
// `LiteSVM::set_sysvar`). Supported ids: Clock, Rent, EpochSchedule,
// EpochRewards, LastRestartSlot, SlotHashes, StakeHistory, SlotHistory.
// ---------------------------------------------------------------------------

/// Serializes the sysvar identified by the 32-byte `id` (its account pubkey)
/// into `out_buf` as bincode. Probe-then-copy: pass `(null, 0)` to learn the
/// required length via `*out_written`, then call again with a buffer of at
/// least that size. Returns 0 on success, non-zero on error (including an
/// unrecognized sysvar id).
///
/// # Safety
///
/// See module-level safety model. `id` must point to 32 readable bytes;
/// `out_written` must be non-null; if `out_buf_len > 0` and `out_buf` is
/// non-null, `out_buf` must be valid for `out_buf_len` writes.
#[no_mangle]
pub unsafe extern "C" fn litesvm_get_sysvar(
    handle: *const LiteSvmHandle,
    id: *const u8,
    out_buf: *mut u8,
    out_buf_len: usize,
    out_written: *mut usize,
) -> i32 {
    use solana_sdk_ids::sysvar as ids;
    guard(-1, || {
        clear_error();
        if out_written.is_null() {
            set_error("null out_written pointer");
            return 1;
        }
        // SAFETY: `out_written` non-null per check.
        unsafe { ptr::write(out_written, 0) };
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_ref(handle) }) else {
            return 2;
        };
        if id.is_null() {
            set_error("null sysvar id pointer");
            return 3;
        }
        // SAFETY: 32 readable bytes per caller's contract.
        let id: [u8; 32] = unsafe { slice::from_raw_parts(id, 32) }
            .try_into()
            .expect("slice is length 32");

        macro_rules! ser {
            ($t:ty) => {
                bincode::serialize(&h.inner.get_sysvar::<$t>())
            };
        }
        let encoded = if id == ids::clock::id().to_bytes() {
            ser!(solana_clock::Clock)
        } else if id == ids::rent::id().to_bytes() {
            ser!(solana_rent::Rent)
        } else if id == ids::epoch_schedule::id().to_bytes() {
            ser!(solana_epoch_schedule::EpochSchedule)
        } else if id == ids::epoch_rewards::id().to_bytes() {
            ser!(solana_epoch_rewards::EpochRewards)
        } else if id == ids::last_restart_slot::id().to_bytes() {
            ser!(solana_last_restart_slot::LastRestartSlot)
        } else if id == ids::slot_hashes::id().to_bytes() {
            ser!(solana_slot_hashes::SlotHashes)
        } else if id == ids::stake_history::id().to_bytes() {
            ser!(solana_stake_interface::stake_history::StakeHistory)
        } else if id == ids::slot_history::id().to_bytes() {
            ser!(solana_slot_history::SlotHistory)
        } else {
            set_error("unknown sysvar id");
            return 4;
        };
        let encoded = match encoded {
            Ok(b) => b,
            Err(e) => {
                set_error(format!("bincode encode failed: {e}"));
                return 5;
            }
        };
        // SAFETY: `out_written` non-null per earlier check.
        unsafe { ptr::write(out_written, encoded.len()) };
        if encoded.len() > out_buf_len {
            return 0; // caller resizes and retries; buf untouched
        }
        if !out_buf.is_null() && !encoded.is_empty() {
            // SAFETY: `out_buf` valid for `out_buf_len >= encoded.len()` writes.
            unsafe { ptr::copy_nonoverlapping(encoded.as_ptr(), out_buf, encoded.len()) };
        }
        0
    })
}

/// Installs `(data, data_len)` (bincode-encoded sysvar account data) as the
/// sysvar identified by the 32-byte `id`, refreshing the bank's sysvar cache.
/// Returns 0 on success, non-zero on error (including an unrecognized sysvar
/// id or malformed data).
///
/// # Safety
///
/// See module-level safety model. `id` must point to 32 readable bytes;
/// `(data, data_len)` must describe a valid slice or be `(null, 0)`.
#[no_mangle]
pub unsafe extern "C" fn litesvm_set_sysvar(
    handle: *mut LiteSvmHandle,
    id: *const u8,
    data: *const u8,
    data_len: usize,
) -> i32 {
    use solana_sdk_ids::sysvar as ids;
    guard(-1, || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_mut(handle) }) else {
            return 1;
        };
        if id.is_null() {
            set_error("null sysvar id pointer");
            return 2;
        }
        // SAFETY: 32 readable bytes per caller's contract.
        let id: [u8; 32] = unsafe { slice::from_raw_parts(id, 32) }
            .try_into()
            .expect("slice is length 32");
        // SAFETY: valid slice or (null, 0) per caller's contract.
        let bytes: &[u8] = if data.is_null() {
            &[]
        } else {
            unsafe { slice::from_raw_parts(data, data_len) }
        };

        macro_rules! de_set {
            ($t:ty) => {
                match bincode::deserialize::<$t>(bytes) {
                    Ok(v) => {
                        h.inner.set_sysvar(&v);
                        0
                    }
                    Err(e) => {
                        set_error(format!("bincode decode failed: {e}"));
                        4
                    }
                }
            };
        }
        if id == ids::clock::id().to_bytes() {
            de_set!(solana_clock::Clock)
        } else if id == ids::rent::id().to_bytes() {
            de_set!(solana_rent::Rent)
        } else if id == ids::epoch_schedule::id().to_bytes() {
            de_set!(solana_epoch_schedule::EpochSchedule)
        } else if id == ids::epoch_rewards::id().to_bytes() {
            de_set!(solana_epoch_rewards::EpochRewards)
        } else if id == ids::last_restart_slot::id().to_bytes() {
            de_set!(solana_last_restart_slot::LastRestartSlot)
        } else if id == ids::slot_hashes::id().to_bytes() {
            de_set!(solana_slot_hashes::SlotHashes)
        } else if id == ids::stake_history::id().to_bytes() {
            de_set!(solana_stake_interface::stake_history::StakeHistory)
        } else if id == ids::slot_history::id().to_bytes() {
            de_set!(solana_slot_history::SlotHistory)
        } else {
            set_error("unknown sysvar id");
            3
        }
    })
}

// ---------------------------------------------------------------------------
// ComputeBudget (fixed layout, ~44 numeric fields)
// ---------------------------------------------------------------------------

/// Mirror of `solana_compute_budget::compute_budget::ComputeBudget`.
/// Fields typed as `usize` on the Rust side are surfaced as `u64` for ABI
/// stability; `heap_size` stays `u32` as in the source.
#[repr(C)]
pub struct LiteSvmComputeBudget {
    pub compute_unit_limit: u64,
    pub log_64_units: u64,
    pub create_program_address_units: u64,
    pub invoke_units: u64,
    pub max_instruction_stack_depth: u64,
    pub max_instruction_trace_length: u64,
    pub sha256_base_cost: u64,
    pub sha256_byte_cost: u64,
    pub sha256_max_slices: u64,
    pub max_call_depth: u64,
    pub stack_frame_size: u64,
    pub log_pubkey_units: u64,
    pub cpi_bytes_per_unit: u64,
    pub sysvar_base_cost: u64,
    pub secp256k1_recover_cost: u64,
    pub syscall_base_cost: u64,
    pub curve25519_edwards_validate_point_cost: u64,
    pub curve25519_edwards_add_cost: u64,
    pub curve25519_edwards_subtract_cost: u64,
    pub curve25519_edwards_multiply_cost: u64,
    pub curve25519_edwards_msm_base_cost: u64,
    pub curve25519_edwards_msm_incremental_cost: u64,
    pub curve25519_ristretto_validate_point_cost: u64,
    pub curve25519_ristretto_add_cost: u64,
    pub curve25519_ristretto_subtract_cost: u64,
    pub curve25519_ristretto_multiply_cost: u64,
    pub curve25519_ristretto_msm_base_cost: u64,
    pub curve25519_ristretto_msm_incremental_cost: u64,
    pub heap_size: u32,
    pub heap_cost: u64,
    pub mem_op_base_cost: u64,
    pub alt_bn128_addition_cost: u64,
    pub alt_bn128_multiplication_cost: u64,
    pub alt_bn128_pairing_one_pair_cost_first: u64,
    pub alt_bn128_pairing_one_pair_cost_other: u64,
    pub big_modular_exponentiation_base_cost: u64,
    pub big_modular_exponentiation_cost_divisor: u64,
    pub poseidon_cost_coefficient_a: u64,
    pub poseidon_cost_coefficient_c: u64,
    pub get_remaining_compute_units_cost: u64,
    pub alt_bn128_g1_compress: u64,
    pub alt_bn128_g1_decompress: u64,
    pub alt_bn128_g2_compress: u64,
    pub alt_bn128_g2_decompress: u64,
}

fn compute_budget_to_ffi(
    b: &solana_compute_budget::compute_budget::ComputeBudget,
) -> LiteSvmComputeBudget {
    // usize -> u64 is lossless on every platform we target (usize is u32 or
    // u64). `as u64` is deliberate here: it stays infallible even on future
    // hypothetical 128-bit targets because compute-budget values are well
    // below u64::MAX.
    LiteSvmComputeBudget {
        compute_unit_limit: b.compute_unit_limit,
        log_64_units: b.log_64_units,
        create_program_address_units: b.create_program_address_units,
        invoke_units: b.invoke_units,
        max_instruction_stack_depth: b.max_instruction_stack_depth as u64,
        max_instruction_trace_length: b.max_instruction_trace_length as u64,
        sha256_base_cost: b.sha256_base_cost,
        sha256_byte_cost: b.sha256_byte_cost,
        sha256_max_slices: b.sha256_max_slices,
        max_call_depth: b.max_call_depth as u64,
        stack_frame_size: b.stack_frame_size as u64,
        log_pubkey_units: b.log_pubkey_units,
        cpi_bytes_per_unit: b.cpi_bytes_per_unit,
        sysvar_base_cost: b.sysvar_base_cost,
        secp256k1_recover_cost: b.secp256k1_recover_cost,
        syscall_base_cost: b.syscall_base_cost,
        curve25519_edwards_validate_point_cost: b.curve25519_edwards_validate_point_cost,
        curve25519_edwards_add_cost: b.curve25519_edwards_add_cost,
        curve25519_edwards_subtract_cost: b.curve25519_edwards_subtract_cost,
        curve25519_edwards_multiply_cost: b.curve25519_edwards_multiply_cost,
        curve25519_edwards_msm_base_cost: b.curve25519_edwards_msm_base_cost,
        curve25519_edwards_msm_incremental_cost: b.curve25519_edwards_msm_incremental_cost,
        curve25519_ristretto_validate_point_cost: b.curve25519_ristretto_validate_point_cost,
        curve25519_ristretto_add_cost: b.curve25519_ristretto_add_cost,
        curve25519_ristretto_subtract_cost: b.curve25519_ristretto_subtract_cost,
        curve25519_ristretto_multiply_cost: b.curve25519_ristretto_multiply_cost,
        curve25519_ristretto_msm_base_cost: b.curve25519_ristretto_msm_base_cost,
        curve25519_ristretto_msm_incremental_cost: b.curve25519_ristretto_msm_incremental_cost,
        heap_size: b.heap_size,
        heap_cost: b.heap_cost,
        mem_op_base_cost: b.mem_op_base_cost,
        // Agave 4.0 split the single alt_bn128 add/mul costs into G1/G2
        // variants; the FFI struct keeps its original (G1) field names for
        // ABI stability and mirrors the G1 cost, the original semantic.
        alt_bn128_addition_cost: b.alt_bn128_g1_addition_cost,
        alt_bn128_multiplication_cost: b.alt_bn128_g1_multiplication_cost,
        alt_bn128_pairing_one_pair_cost_first: b.alt_bn128_pairing_one_pair_cost_first,
        alt_bn128_pairing_one_pair_cost_other: b.alt_bn128_pairing_one_pair_cost_other,
        big_modular_exponentiation_base_cost: b.big_modular_exponentiation_base_cost,
        big_modular_exponentiation_cost_divisor: b.big_modular_exponentiation_cost_divisor,
        poseidon_cost_coefficient_a: b.poseidon_cost_coefficient_a,
        poseidon_cost_coefficient_c: b.poseidon_cost_coefficient_c,
        get_remaining_compute_units_cost: b.get_remaining_compute_units_cost,
        alt_bn128_g1_compress: b.alt_bn128_g1_compress,
        alt_bn128_g1_decompress: b.alt_bn128_g1_decompress,
        alt_bn128_g2_compress: b.alt_bn128_g2_compress,
        alt_bn128_g2_decompress: b.alt_bn128_g2_decompress,
    }
}

fn compute_budget_from_ffi(
    f: &LiteSvmComputeBudget,
) -> solana_compute_budget::compute_budget::ComputeBudget {
    // u64 -> usize can truncate on 32-bit. Compute-budget values are all
    // small (hundreds to thousands), so `try_into().expect(...)` documents
    // the invariant while catching any future regression.
    let u64_to_usize = |v: u64, name: &'static str| -> usize {
        usize::try_from(v).unwrap_or_else(|_| panic!("compute budget field {name} exceeds usize"))
    };
    let mut b =
        solana_compute_budget::compute_budget::ComputeBudget::new_with_defaults(false, false);
    b.compute_unit_limit = f.compute_unit_limit;
    b.log_64_units = f.log_64_units;
    b.create_program_address_units = f.create_program_address_units;
    b.invoke_units = f.invoke_units;
    b.max_instruction_stack_depth =
        u64_to_usize(f.max_instruction_stack_depth, "max_instruction_stack_depth");
    b.max_instruction_trace_length = u64_to_usize(
        f.max_instruction_trace_length,
        "max_instruction_trace_length",
    );
    b.sha256_base_cost = f.sha256_base_cost;
    b.sha256_byte_cost = f.sha256_byte_cost;
    b.sha256_max_slices = f.sha256_max_slices;
    b.max_call_depth = u64_to_usize(f.max_call_depth, "max_call_depth");
    b.stack_frame_size = u64_to_usize(f.stack_frame_size, "stack_frame_size");
    b.log_pubkey_units = f.log_pubkey_units;
    b.cpi_bytes_per_unit = f.cpi_bytes_per_unit;
    b.sysvar_base_cost = f.sysvar_base_cost;
    b.secp256k1_recover_cost = f.secp256k1_recover_cost;
    b.syscall_base_cost = f.syscall_base_cost;
    b.curve25519_edwards_validate_point_cost = f.curve25519_edwards_validate_point_cost;
    b.curve25519_edwards_add_cost = f.curve25519_edwards_add_cost;
    b.curve25519_edwards_subtract_cost = f.curve25519_edwards_subtract_cost;
    b.curve25519_edwards_multiply_cost = f.curve25519_edwards_multiply_cost;
    b.curve25519_edwards_msm_base_cost = f.curve25519_edwards_msm_base_cost;
    b.curve25519_edwards_msm_incremental_cost = f.curve25519_edwards_msm_incremental_cost;
    b.curve25519_ristretto_validate_point_cost = f.curve25519_ristretto_validate_point_cost;
    b.curve25519_ristretto_add_cost = f.curve25519_ristretto_add_cost;
    b.curve25519_ristretto_subtract_cost = f.curve25519_ristretto_subtract_cost;
    b.curve25519_ristretto_multiply_cost = f.curve25519_ristretto_multiply_cost;
    b.curve25519_ristretto_msm_base_cost = f.curve25519_ristretto_msm_base_cost;
    b.curve25519_ristretto_msm_incremental_cost = f.curve25519_ristretto_msm_incremental_cost;
    b.heap_size = f.heap_size;
    b.heap_cost = f.heap_cost;
    b.mem_op_base_cost = f.mem_op_base_cost;
    b.alt_bn128_g1_addition_cost = f.alt_bn128_addition_cost;
    b.alt_bn128_g1_multiplication_cost = f.alt_bn128_multiplication_cost;
    b.alt_bn128_pairing_one_pair_cost_first = f.alt_bn128_pairing_one_pair_cost_first;
    b.alt_bn128_pairing_one_pair_cost_other = f.alt_bn128_pairing_one_pair_cost_other;
    b.big_modular_exponentiation_base_cost = f.big_modular_exponentiation_base_cost;
    b.big_modular_exponentiation_cost_divisor = f.big_modular_exponentiation_cost_divisor;
    b.poseidon_cost_coefficient_a = f.poseidon_cost_coefficient_a;
    b.poseidon_cost_coefficient_c = f.poseidon_cost_coefficient_c;
    b.get_remaining_compute_units_cost = f.get_remaining_compute_units_cost;
    b.alt_bn128_g1_compress = f.alt_bn128_g1_compress;
    b.alt_bn128_g1_decompress = f.alt_bn128_g1_decompress;
    b.alt_bn128_g2_compress = f.alt_bn128_g2_compress;
    b.alt_bn128_g2_decompress = f.alt_bn128_g2_decompress;
    b
}

/// Writes the current compute budget to `*out`.
/// Returns 0 if a custom budget is set, 1 if using the runtime default (no
/// custom budget has been configured), negative on error.
///
/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_get_compute_budget(
    handle: *const LiteSvmHandle,
    out: *mut LiteSvmComputeBudget,
) -> i32 {
    guard(-1, || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_ref(handle) }) else {
            return -1;
        };
        if out.is_null() {
            set_error("null compute_budget out pointer");
            return -2;
        }
        match h.inner.get_compute_budget() {
            Some(b) => {
                let value = compute_budget_to_ffi(&b);
                // SAFETY: `out` non-null and valid per caller's contract.
                unsafe { ptr::write_unaligned(out, value) };
                0
            }
            None => 1,
        }
    })
}

/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_set_compute_budget(
    handle: *mut LiteSvmHandle,
    budget: *const LiteSvmComputeBudget,
) -> i32 {
    guard(-1, || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_mut(handle) }) else {
            return 1;
        };
        if budget.is_null() {
            set_error("null compute_budget pointer");
            return 2;
        }
        // SAFETY: non-null and valid pointee per caller's contract.
        let src = unsafe { ptr::read_unaligned(budget) };
        h.inner.set_compute_budget(compute_budget_from_ffi(&src));
        0
    })
}

// ---------------------------------------------------------------------------
// FeatureSet (opaque handle)
// ---------------------------------------------------------------------------

pub struct LiteSvmFeatureSetHandle {
    inner: agave_feature_set::FeatureSet,
}

/// # Safety
///
/// `h` must be null or a valid pointer to a `LiteSvmFeatureSetHandle`.
unsafe fn feature_set_ref<'a>(
    h: *const LiteSvmFeatureSetHandle,
) -> Option<&'a LiteSvmFeatureSetHandle> {
    if h.is_null() {
        set_error("null feature_set handle");
        None
    } else {
        // SAFETY: non-null, valid pointee per caller's contract.
        Some(unsafe { &*h })
    }
}

/// # Safety
///
/// `h` must be null or a valid pointer to a `LiteSvmFeatureSetHandle` with
/// no other live references.
unsafe fn feature_set_mut<'a>(
    h: *mut LiteSvmFeatureSetHandle,
) -> Option<&'a mut LiteSvmFeatureSetHandle> {
    if h.is_null() {
        set_error("null feature_set handle");
        None
    } else {
        // SAFETY: non-null, exclusively owned pointee per caller's contract.
        Some(unsafe { &mut *h })
    }
}

#[no_mangle]
pub extern "C" fn litesvm_feature_set_new_default() -> *mut LiteSvmFeatureSetHandle {
    guard(ptr::null_mut(), || {
        clear_error();
        Box::into_raw(Box::new(LiteSvmFeatureSetHandle {
            inner: agave_feature_set::FeatureSet::default(),
        }))
    })
}

#[no_mangle]
pub extern "C" fn litesvm_feature_set_new_all_enabled() -> *mut LiteSvmFeatureSetHandle {
    guard(ptr::null_mut(), || {
        clear_error();
        Box::into_raw(Box::new(LiteSvmFeatureSetHandle {
            inner: agave_feature_set::FeatureSet::all_enabled(),
        }))
    })
}

/// # Safety
///
/// `handle` must be null or a pointer previously returned from one of the
/// `litesvm_feature_set_new_*` constructors, not yet freed.
#[no_mangle]
pub unsafe extern "C" fn litesvm_feature_set_free(handle: *mut LiteSvmFeatureSetHandle) {
    if handle.is_null() {
        return;
    }
    guard((), || {
        // SAFETY: non-null and the pointer came from `Box::into_raw`.
        drop(unsafe { Box::from_raw(handle) });
    });
}

/// Returns 1 if the feature is active, 0 if not, -1 on error.
///
/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_feature_set_is_active(
    handle: *const LiteSvmFeatureSetHandle,
    feature_id: *const u8,
) -> i32 {
    guard(-1, || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { feature_set_ref(handle) }) else {
            return -1;
        };
        // SAFETY: forwarded caller contract.
        let Some(id) = (unsafe { pubkey_from_ptr(feature_id) }) else {
            return -2;
        };
        i32::from(h.inner.is_active(&id))
    })
}

/// Returns 1 (and writes the activation slot to `*out_slot`) if the feature
/// has an activation slot. Returns 0 if inactive. Negative on error.
///
/// # Safety
///
/// See module-level safety model. `out_slot` must be null or a valid `*mut u64`.
#[no_mangle]
pub unsafe extern "C" fn litesvm_feature_set_activated_slot(
    handle: *const LiteSvmFeatureSetHandle,
    feature_id: *const u8,
    out_slot: *mut u64,
) -> i32 {
    guard(-1, || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { feature_set_ref(handle) }) else {
            return -1;
        };
        // SAFETY: forwarded caller contract.
        let Some(id) = (unsafe { pubkey_from_ptr(feature_id) }) else {
            return -2;
        };
        if out_slot.is_null() {
            set_error("null out_slot pointer");
            return -3;
        }
        match h.inner.activated_slot(&id) {
            Some(slot) => {
                // SAFETY: `out_slot` non-null and valid per caller's contract.
                unsafe { ptr::write_unaligned(out_slot, slot) };
                1
            }
            None => 0,
        }
    })
}

/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_feature_set_activate(
    handle: *mut LiteSvmFeatureSetHandle,
    feature_id: *const u8,
    slot: u64,
) -> i32 {
    guard(-1, || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { feature_set_mut(handle) }) else {
            return 1;
        };
        // SAFETY: forwarded caller contract.
        let Some(id) = (unsafe { pubkey_from_ptr(feature_id) }) else {
            return 2;
        };
        h.inner.activate(&id, slot);
        0
    })
}

/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_feature_set_deactivate(
    handle: *mut LiteSvmFeatureSetHandle,
    feature_id: *const u8,
) -> i32 {
    guard(-1, || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { feature_set_mut(handle) }) else {
            return 1;
        };
        // SAFETY: forwarded caller contract.
        let Some(id) = (unsafe { pubkey_from_ptr(feature_id) }) else {
            return 2;
        };
        h.inner.deactivate(&id);
        0
    })
}

/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_feature_set_active_count(
    handle: *const LiteSvmFeatureSetHandle,
) -> usize {
    guard(0, || {
        // SAFETY: forwarded caller contract.
        unsafe { feature_set_ref(handle) }.map_or(0, |h| h.inner.active().len())
    })
}

/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_feature_set_inactive_count(
    handle: *const LiteSvmFeatureSetHandle,
) -> usize {
    guard(0, || {
        // SAFETY: forwarded caller contract.
        unsafe { feature_set_ref(handle) }.map_or(0, |h| h.inner.inactive().len())
    })
}

/// Copies up to `out_count` active feature pubkeys (32 bytes each) into
/// `out_buf`. Output is sorted by pubkey for deterministic ordering. Returns
/// the total number of active features.
///
/// # Safety
///
/// See module-level safety model. If `out_count > 0` and `out_buf` is
/// non-null, `out_buf` must be valid for writes of `out_count * 32` bytes.
#[no_mangle]
pub unsafe extern "C" fn litesvm_feature_set_active_copy(
    handle: *const LiteSvmFeatureSetHandle,
    out_buf: *mut u8,
    out_count: usize,
) -> usize {
    guard(0, || {
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { feature_set_ref(handle) }) else {
            return 0;
        };
        let mut active: Vec<_> = h.inner.active().keys().map(|k| k.to_bytes()).collect();
        active.sort_unstable();
        let n = active.len();
        if out_count > 0 && !out_buf.is_null() {
            let copy_n = n.min(out_count);
            for (i, bytes) in active.iter().take(copy_n).enumerate() {
                // SAFETY: `out_buf.add(i * 32)` is within the caller-supplied
                // buffer of `out_count * 32` bytes (i < copy_n <= out_count).
                unsafe { ptr::copy_nonoverlapping(bytes.as_ptr(), out_buf.add(i * 32), 32) };
            }
        }
        n
    })
}

/// Copies up to `out_count` inactive feature pubkeys (32 bytes each) into
/// `out_buf`. Output is sorted by pubkey for deterministic ordering. Returns
/// the total number of inactive features.
///
/// # Safety
///
/// See module-level safety model. If `out_count > 0` and `out_buf` is
/// non-null, `out_buf` must be valid for writes of `out_count * 32` bytes.
#[no_mangle]
pub unsafe extern "C" fn litesvm_feature_set_inactive_copy(
    handle: *const LiteSvmFeatureSetHandle,
    out_buf: *mut u8,
    out_count: usize,
) -> usize {
    guard(0, || {
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { feature_set_ref(handle) }) else {
            return 0;
        };
        let mut inactive: Vec<_> = h.inner.inactive().iter().map(|k| k.to_bytes()).collect();
        inactive.sort_unstable();
        let n = inactive.len();
        if out_count > 0 && !out_buf.is_null() {
            let copy_n = n.min(out_count);
            for (i, bytes) in inactive.iter().take(copy_n).enumerate() {
                // SAFETY: `out_buf.add(i * 32)` is within the caller-supplied
                // buffer of `out_count * 32` bytes (i < copy_n <= out_count).
                unsafe { ptr::copy_nonoverlapping(bytes.as_ptr(), out_buf.add(i * 32), 32) };
            }
        }
        n
    })
}

/// Installs `handle` as the LiteSVM's active feature set. Caller retains
/// ownership of the feature-set handle.
///
/// # Safety
///
/// See module-level safety model.
#[no_mangle]
pub unsafe extern "C" fn litesvm_set_feature_set(
    handle: *mut LiteSvmHandle,
    features: *const LiteSvmFeatureSetHandle,
) -> i32 {
    guard(-1, || {
        clear_error();
        // SAFETY: forwarded caller contract.
        let Some(h) = (unsafe { handle_mut(handle) }) else {
            return 1;
        };
        // SAFETY: forwarded caller contract.
        let Some(fs) = (unsafe { feature_set_ref(features) }) else {
            return 2;
        };
        h.inner.set_feature_set(fs.inner.clone());
        0
    })
}

// Note: `get_feature_set` is gated behind `internal-test` in the litesvm
// crate (same treatment as node-litesvm, which does not expose it).
// Callers should keep their FeatureSet handle around after `set_feature_set`
// if they need to read it back.
