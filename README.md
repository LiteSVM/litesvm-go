<div align="center">
    <img src="https://raw.githubusercontent.com/litesvm/litesvm/master/logo.jpeg" width="50%" height="50%">
</div>

---

# litesvm-go

[![Go Reference](https://pkg.go.dev/badge/github.com/LiteSVM/litesvm-go.svg)](https://pkg.go.dev/github.com/LiteSVM/litesvm-go)
[![License: Apache-2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

Go bindings for [LiteSVM](https://github.com/LiteSVM/litesvm) — a fast,
in-process Solana VM purpose-built for testing programs and clients.

If you've been writing Solana tests in Go against `solana-test-validator`
(slow, heavyweight) or hand-rolled mocks (incomplete), `litesvm-go` gives
you a real SVM with sysvars, programs, compute budget, and feature gates,
that boots in milliseconds and runs in the same process as your tests.

Core types (`PublicKey`, `Hash`, `Signature`) come straight from
[gagliardetto/solana-go](https://github.com/gagliardetto/solana-go), so
values flow naturally between `litesvm-go` and the rest of the Go Solana
ecosystem.

## :sparkles: Highlights

- **Fast**: in-process VM, no validator, no RPC.
- **Familiar**: reuses `solana-go`'s `PublicKey`, `Hash`, `Signature`, and
  bincode-encoded transactions, so existing Go Solana code drops in.
- **Honest about errors**: every entry point returns `(value, error)`;
  panics on the Rust side are caught and converted to errors.
- **Batteries included**: airdrops, transactions (legacy + v0), simulation,
  account read/write, sysvars, compute budget, feature gates, time travel,
  custom programs, transaction history.

## :package: Install

```sh
go get github.com/LiteSVM/litesvm-go
```

That's it. `go build` / `go test` will pick the right prebuilt static
archive automatically based on `GOOS` / `GOARCH`. **No Rust toolchain
required** — archives are vendored in [litesvm_vendor/](./litesvm_vendor)
and statically linked at build time.

Supported platforms: macOS (amd64, arm64), Linux (amd64, arm64; glibc or
musl), Windows (amd64). Go 1.24+.

> :penguin: **Alpine / musl users**: add `-tags musl` to your `go build`
> / `go test` invocation.

## :rocket: Quick start

A minimal transfer test, mirroring the [LiteSVM
README](https://github.com/LiteSVM/litesvm#-minimal-example):

```go
package mytest

import (
    "testing"

    litesvm "github.com/LiteSVM/litesvm-go"
    solana "github.com/gagliardetto/solana-go"
    "github.com/gagliardetto/solana-go/programs/system"
)

func TestTransfer(t *testing.T) {
    svm, err := litesvm.New()
    if err != nil {
        t.Fatal(err)
    }
    defer svm.Close()

    // Fund a payer.
    priv, _ := solana.NewRandomPrivateKey()
    payer := priv.PublicKey()
    recipient := solana.NewWallet().PublicKey()

    if err := svm.Airdrop(payer, 2_000_000_000); err != nil {
        t.Fatal(err)
    }

    // Build, sign, and encode a legacy transfer with solana-go.
    blockhash, _ := svm.LatestBlockhash()
    ix := system.NewTransferInstruction(1_000_000_000, payer, recipient).Build()
    tx, _ := solana.NewTransaction(
        []solana.Instruction{ix},
        blockhash,
        solana.TransactionPayer(payer),
    )
    tx.Sign(func(k solana.PublicKey) *solana.PrivateKey {
        if k.Equals(payer) {
            return &priv
        }
        return nil
    })
    txBytes, _ := tx.MarshalBinary()

    // Execute.
    out, err := svm.SendLegacyTransaction(txBytes)
    if err != nil {
        t.Fatal(err)
    }
    defer out.Close()

    if !out.IsOk() {
        t.Fatalf("tx failed: %s\nlogs: %v", out.Error(), out.Logs())
    }

    // Inspect resulting balances.
    if lamports, _, _ := svm.Balance(recipient); lamports != 1_000_000_000 {
        t.Fatalf("recipient balance = %d, want 1_000_000_000", lamports)
    }
}
```

A fresh `LiteSVM` ships with the core Solana programs (System Program,
SPL Token, etc.) preloaded.

## :outbox_tray: Sending and simulating transactions

Both legacy and versioned transactions are supported. The methods accept
the bincode-encoded bytes produced by `(*solana.Transaction).MarshalBinary`.

```go
out, err := svm.SendLegacyTransaction(txBytes)     // commits on success
out, err := svm.SendVersionedTransaction(txBytes)  // same, for v0 messages

sim, err := svm.SimulateLegacyTransaction(txBytes)    // never commits
sim, err := svm.SimulateVersionedTransaction(txBytes) // same, for v0 messages
```

Every entry point returns a `*TxOutcome`. The same handle carries metadata
whether the transaction succeeded or failed, so call `IsOk` first:

```go
out, _ := svm.SendLegacyTransaction(txBytes)
defer out.Close()

if !out.IsOk() {
    t.Fatalf("error: %s", out.Error())
}

_ = out.Signature()      // solana.Signature
_ = out.ComputeUnits()   // uint64
_ = out.Fee()            // uint64
_ = out.Logs()           // []string
_ = out.InnerInstructions()

// Programs that call set_return_data expose it here.
if pid, data, ok := out.ReturnData(); ok {
    _ = pid
    _ = data
}

// Simulate also exposes the would-be post-execution account state.
for _, p := range sim.PostAccounts() {
    defer p.Account.Close()
    _ = p.Address
    _ = p.Account.Lamports()
}
```

Look up historical transactions by signature:

```go
prior := svm.GetTransaction(signature) // nil if unknown
if prior != nil {
    defer prior.Close()
}
```

## :card_file_box: Accounts

Accounts are opaque handles. Always `Close` them (or rely on the finalizer).

```go
// Read
if acct := svm.GetAccount(addr); acct != nil {
    defer acct.Close()
    _ = acct.Lamports()
    _ = acct.Owner()
    _ = acct.Executable()
    _ = acct.Data() // copy
}

// Write
rent, _ := svm.MinimumBalanceForRentExemption(len(payload))
acct, _ := litesvm.NewAccount(rent, payload, ownerProgram, false, 0)
defer acct.Close()
_ = svm.SetAccount(targetAddr, acct)
```

## :jigsaw: Loading programs

Load any compiled SBF program so transactions can invoke it.

```go
// From a .so file on disk.
_ = svm.AddProgramFromFile(programID, "./target/deploy/my_program.so")

// From bytes you already have in memory.
_ = svm.AddProgram(programID, bytes)

// Under a specific loader (advanced).
_ = svm.AddProgramWithLoader(programID, bytes, loaderID)
```

## :clock3: Time travel and sysvars

Forward the internal clock:

```go
_ = svm.WarpToSlot(10_000_000)
```

Read and overwrite sysvars directly:

```go
c, _ := svm.Clock()
c.UnixTimestamp += 3600
_ = svm.SetClock(c)

r, _ := svm.Rent()
_ = svm.SetRent(r)

es, _ := svm.EpochSchedule()
_ = svm.SetEpochSchedule(es)

er, _ := svm.EpochRewards()
_ = svm.SetEpochRewards(er)

slot, _ := svm.LastRestartSlot()
_ = svm.SetLastRestartSlot(slot)

hashes, _ := svm.SlotHashes()
_ = svm.SetSlotHashes(hashes)

hist, _ := svm.StakeHistory()
_ = svm.SetStakeHistory(hist)
```

`SlotHistory` is a ~128 KB bitvec and uses a handle rather than a slice:

```go
sh := litesvm.NewSlotHistory()
defer sh.Close()
sh.Add(42)
_ = sh.Check(42) // SlotHistoryFound / SlotHistoryNotFound / SlotHistoryTooOld
_ = svm.SetSlotHistory(sh)
```

## :gear: Compute budget

```go
budget, set, _ := svm.ComputeBudget()
if !set {
    budget.ComputeUnitLimit = 1_400_000
}
_ = svm.SetComputeBudget(budget)
```

`ComputeBudget` is a plain struct mirroring the Rust `ComputeBudget`.
`usize` fields are normalized to `uint64`; `HeapSize` stays `uint32`.

## :triangular_flag_on_post: Feature gating

```go
// Start with all features off, then activate what you care about.
fs := litesvm.NewFeatureSet()
defer fs.Close()
_ = fs.Activate(featureID, 0)

// Or start from "everything enabled" and flip specific features off.
fs = litesvm.NewFeatureSetAllEnabled()
defer fs.Close()
_ = fs.Deactivate(featureID)

_ = svm.SetFeatureSet(fs)
```

Inspect with `IsActive`, `ActivatedSlot`, `ActiveCount`, `InactiveCount`,
`ActiveFeatures`, `InactiveFeatures`.

## :wrench: Configuration

Each setter mirrors the builder method on the Rust `LiteSVM` type:

```go
_ = svm.SetSigverify(false)            // accept unsigned / badly-signed txs
_ = svm.SetBlockhashCheck(false)       // skip recent-blockhash enforcement
_ = svm.SetTransactionHistory(0)       // 0 disables dedup; any N caps history
_ = svm.SetLogBytesLimit(-1)           // negative = unlimited
_ = svm.SetLamports(1 << 40)           // default lamports for new accounts
_ = svm.SetSysvars()                   // reset sysvars to defaults
_ = svm.SetBuiltins()                  // reload built-in programs
_ = svm.SetDefaultPrograms()           // reload SPL Token, Memo, etc.
_ = svm.SetPrecompiles()               // enable ed25519/secp256k1 precompiles
_ = svm.WithNativeMints()              // seed wrapped-SOL mint
```

For the complete method list, see the
[GoDoc](https://pkg.go.dev/github.com/LiteSVM/litesvm-go).

## :memo: Notes

**Thread safety.** A `LiteSVM` handle is not safe for concurrent use from
multiple goroutines (the underlying Rust type is not `Sync`, and most
methods mutate internal state). Confine a handle to a single goroutine,
or wrap it in a `sync.Mutex`.

**Panic behavior.** Vendored release archives are built with
`immediate-abort`: any panic inside Rust aborts the host process directly,
without unwinding. This is a deliberate trade for ~50% smaller archives.
If you ever hit one in practice, please open an issue with a reproducer.

## :sos: Troubleshooting

**`undefined reference to ...` on Linux** — usually means you're on a
musl-based distro (Alpine) but linking the glibc archive. Add `-tags musl`
to your `go build` / `go test` invocation.

**`go: cannot find module providing package`** — confirm `go get`
succeeded and your module is on Go 1.24+.

---

# Contributing

The sections below are for contributors working on `litesvm-go` itself.

## Building from source

You only need to do this if you are modifying [src/lib.rs](./src/lib.rs)
or refreshing the vendored archives for a release.

```sh
git clone https://github.com/LiteSVM/litesvm-go.git
cd litesvm-go

# Iterating: build the local debug archive and run tests against it.
make dev
make test-dev

# Release: rebuild every vendored archive committed to git.
# Requires:
#   - rustup targets for darwin/linux/windows
#   - cargo-zigbuild + zig (used for the linux cross-compiles)
#   - mingw-w64 (used for the windows cross-compile; do not substitute
#     zigbuild — zig's lld produces a staticlib whose TLS/unwinder ABI
#     does not link cleanly against cgo's MinGW gcc on Windows)
#   - nightly toolchain with rust-src (-Z build-std + immediate-abort
#     are unstable, so vendor builds run on `cargo +nightly`)
#   - llvm-strip (ships with LLVM; used to drop debug info from each
#     archive, ~3x size reduction on linux/windows)
make vendor
```

Run `make` for the full list of targets.

### Build modes

| Mode                   | Tag           | When to use                                                                |
| ---------------------- | ------------- | -------------------------------------------------------------------------- |
| Vendored static        | _(default)_   | Normal use — the `go get` path                                             |
| Local cargo build      | `litesvm_dev` | Iterating on `src/lib.rs` (see above)                                      |
| System dynamic library | `dynamic`     | You have `liblitesvm_go.{so,dylib}` installed somewhere on the loader path |

```sh
go test ./...                       # default: vendored static
go test -tags litesvm_dev ./...     # link against ./target/debug/
go build -tags dynamic ./...        # link against system liblitesvm_go
```

## Repository layout

```
Cargo.toml                  Rust cdylib + staticlib (release builds only)
src/lib.rs                  C ABI implementation
include/litesvm.h           C header, kept in sync with lib.rs
litesvm_vendor/             Prebuilt static archives + header (committed)
go.mod
litesvm.go                  Idiomatic Go wrapper
cgo_<plat>_<arch>.go        Per-platform cgo LDFLAGS (one per target)
cgo_dev.go                  -tags litesvm_dev: links target/{debug,release}
cgo_dynamic.go              -tags dynamic: links system liblitesvm_go
select_litesvm.h            Header shim: vendored vs. include/ vs. system
litesvm_test.go             End-to-end tests
Makefile                    `make vendor` to refresh archives
```

The Rust crate and Go module share a single directory. Go ignores
`Cargo.toml` / `src/`; Cargo ignores `go.mod` / `*.go`.

## Extending

To wire a new LiteSVM method through to Go, follow the pattern already
used throughout `src/lib.rs` and `litesvm.go`:

1. Add an `extern "C" fn` in `src/lib.rs`, wrapped in `guard(...)`, using
   the `handle_ref` / `handle_mut` / `pubkey_from_ptr` helpers. Set
   thread-local error strings on failure.
2. Declare the function in `include/litesvm.h`.
3. Add the Go method in `litesvm.go`. Translate non-zero return codes via
   `lastError`.
4. Add a test to `litesvm_test.go`.

For result-carrying operations, prefer extending the existing `TxOutcome`
shape rather than reproducing Rust `Result` / enum discriminants across
the FFI.

## License

Apache-2.0. See [LICENSE](./LICENSE).
