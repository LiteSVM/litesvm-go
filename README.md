<div align="center">
    <img src="https://raw.githubusercontent.com/litesvm/litesvm/master/logo.jpeg" width="50%" height="50%">
</div>

---

# litesvm-go

[![Go Reference](https://pkg.go.dev/badge/github.com/LiteSVM/litesvm-go.svg)](https://pkg.go.dev/github.com/LiteSVM/litesvm-go)
[![License: Apache-2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

Go bindings for [LiteSVM](https://github.com/LiteSVM/litesvm) — an in-process
Solana VM optimized for fast, ergonomic program testing. `litesvm-go`
exposes the LiteSVM Rust crate through a thin C ABI and calls into it from
Go via cgo.

If you've been writing Solana tests in Go against `solana-test-validator`
(slow, heavyweight) or hand-rolled mocks (incomplete), `litesvm-go` gives
you a real SVM — with sysvars, programs, compute budget, and feature gates
— that boots in milliseconds and runs in the same process as your tests.

Core Solana types (`PublicKey`, `Hash`, `Signature`) are reused from
[gagliardetto/solana-go](https://github.com/gagliardetto/solana-go) so
values flow naturally between `litesvm-go` and the rest of the Go Solana
ecosystem.

> Status: experimental. The API is stable enough to write real tests
> against but may grow as it catches up with the Rust crate.

## Highlights

- Fast: an in-process VM, no validator, no RPC.
- Familiar: re-uses `solana-go`'s `PublicKey`, `Hash`, `Signature`, and
  bincode-encoded transactions, so existing Go Solana code drops in.
- Honest about errors: every entry point returns `(value, error)`; panics
  on the Rust side are caught and converted to errors.
- Full surface area: airdrops, transactions (legacy + v0), simulation,
  account reads/writes, sysvars, compute budget, feature gates, time
  travel, custom programs, transaction history.

## Requirements

- Go 1.24 or newer with cgo enabled (the default)
- A C toolchain that cgo can drive (Xcode Command Line Tools on macOS;
  `gcc`/`clang` on Linux; `mingw-w64` on Windows)
- macOS (Intel or Apple Silicon), Linux (glibc or musl, amd64 or arm64),
  or Windows (amd64)

You do **not** need Rust to use `litesvm-go`. Prebuilt static archives
for every supported platform are vendored in
[litesvm_vendor/](./litesvm_vendor) and statically linked into your
binary by cgo at build time.

## Installation

```sh
go get github.com/LiteSVM/litesvm-go
```

That's it. `go build` / `go test` in your project will pick the right
archive automatically based on `GOOS` / `GOARCH`:

| OS      | Arch  | C library | Vendored archive                    |
| ------- | ----- | --------- | ----------------------------------- |
| macOS   | amd64 | -         | `liblitesvm_go_darwin_amd64.a`      |
| macOS   | arm64 | -         | `liblitesvm_go_darwin_arm64.a`      |
| Linux   | amd64 | glibc     | `liblitesvm_go_glibc_linux_amd64.a` |
| Linux   | arm64 | glibc     | `liblitesvm_go_glibc_linux_arm64.a` |
| Linux   | amd64 | musl      | `liblitesvm_go_musl_linux_amd64.a`  |
| Linux   | arm64 | musl      | `liblitesvm_go_musl_linux_arm64.a`  |
| Windows | amd64 | -         | `liblitesvm_go_windows_amd64.a`     |

### Linux: glibc vs musl

The default Linux archives target glibc (Debian, Ubuntu, RHEL, etc.). If
you're building on Alpine or any other musl-based distro, opt into the
musl archive with a build tag:

```sh
go build -tags musl ./...
go test -tags musl ./...
```

### Build modes

| Mode                   | Tag           | When to use                                                                |
| ---------------------- | ------------- | -------------------------------------------------------------------------- |
| Vendored static        | _(default)_   | Normal use — the `go get` path                                             |
| Local cargo build      | `litesvm_dev` | Iterating on `src/lib.rs` (see below)                                      |
| System dynamic library | `dynamic`     | You have `liblitesvm_go.{so,dylib}` installed somewhere on the loader path |

```sh
go test ./...                       # default: vendored static
go test -tags litesvm_dev ./...     # link against ./target/debug/
go build -tags dynamic ./...        # link against system liblitesvm_go
```

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

## Quick start

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

## Sending and simulating transactions

Both legacy and versioned transactions are supported. The methods accept
the bincode-encoded bytes produced by `(*solana.Transaction).MarshalBinary`.

```go
out, err := svm.SendLegacyTransaction(txBytes)     // commit on success
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

## Accounts

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

## Loading programs

Load any compiled SBF program so transactions can invoke it.

```go
// From a .so file on disk.
_ = svm.AddProgramFromFile(programID, "./target/deploy/my_program.so")

// From bytes you already have in memory.
_ = svm.AddProgram(programID, bytes)

// Under a specific loader (advanced).
_ = svm.AddProgramWithLoader(programID, bytes, loaderID)
```

## Time travel and sysvars

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

## Compute budget

```go
budget, set, _ := svm.ComputeBudget()
if !set {
    budget.ComputeUnitLimit = 1_400_000
}
_ = svm.SetComputeBudget(budget)
```

`ComputeBudget` is a plain struct mirroring the 44 fields of the Rust
`ComputeBudget`. `usize` fields are normalized to `uint64`; `HeapSize`
stays `uint32`.

## Feature gating

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

Inspecting a feature set: `IsActive`, `ActivatedSlot`, `ActiveCount`,
`InactiveCount`, `ActiveFeatures`, `InactiveFeatures`.

## Configuration

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

## API surface

- Lifecycle: `New`, `Close`, `Version`
- Funding and balances: `Airdrop`, `Balance`, `MinimumBalanceForRentExemption`
- Blockhash: `LatestBlockhash`, `ExpireBlockhash`
- Transactions: `SendLegacyTransaction`, `SendVersionedTransaction`,
  `SimulateLegacyTransaction`, `SimulateVersionedTransaction`,
  `GetTransaction`
- `TxOutcome`: `IsOk`, `Signature`, `ComputeUnits`, `Fee`, `Logs`, `Error`,
  `ReturnData`, `InnerInstructions`, `PostAccounts`
- Accounts: `GetAccount`, `SetAccount`, `NewAccount` (`Lamports`, `Owner`,
  `Executable`, `RentEpoch`, `Data`)
- Programs: `AddProgram`, `AddProgramFromFile`, `AddProgramWithLoader`
- Time: `WarpToSlot`
- Sysvars: `Clock`, `Rent`, `EpochSchedule`, `EpochRewards`,
  `LastRestartSlot`, `SlotHashes`, `StakeHistory`, `SlotHistory`
- Compute budget: `ComputeBudget`, `SetComputeBudget`
- Feature set: `NewFeatureSet`, `NewFeatureSetAllEnabled`, `IsActive`,
  `ActivatedSlot`, `Activate`, `Deactivate`, `ActiveCount`, `InactiveCount`,
  `ActiveFeatures`, `InactiveFeatures`, `SetFeatureSet`
- Configuration: `SetSigverify`, `Sigverify`, `SetBlockhashCheck`,
  `SetTransactionHistory`, `SetLogBytesLimit`, `SetLamports`, `SetSysvars`,
  `SetBuiltins`, `SetDefaultPrograms`, `SetPrecompiles`, `WithNativeMints`

## Thread safety

`LiteSVM` handles are not safe for concurrent use from multiple goroutines.
The underlying Rust type is not `Sync`, and most method calls mutate
internal state. Either confine a handle to a single goroutine or wrap it
in a `sync.Mutex`.

## Panic safety

The vendored release archives are built with the `immediate-abort` panic
strategy: any panic inside Rust code aborts the host process directly,
without unwinding and without printing a panic message. This is a
deliberate trade for ~50% smaller archives — most of std's
panic-formatting code is dropped at compile time. If you ever hit a
panic-driven abort in practice, please open an issue with a reproducer.

The `litesvm_dev` build path (`cargo build` + `-tags litesvm_dev`) uses
the default debug profile with `panic = "unwind"`. In that mode every
`extern "C"` entry point catches panics with `std::panic::catch_unwind`
and converts them to non-zero return codes plus a descriptive error
string — useful while actively debugging the Rust side.

## Troubleshooting

**`undefined reference to ...` on Linux** — usually means you're on a
musl-based distro (Alpine) but linking the glibc archive. Add `-tags musl`
to your `go build` / `go test` invocation.

**`ld: library 'litesvm_go' not found`** — happens in `litesvm_dev` mode
when you forgot to run `cargo build` first. The `target/debug/` archive
must exist for that build tag to link.

**`go: cannot find module providing package`** — confirm `go get` succeeded
and your module is on Go 1.24+.

**Cross-compilation** — cgo packages can't be cross-compiled with the
default Go toolchain. To build for a different OS or libc you need a C
toolchain that matches the target. The vendored archives themselves are
target-specific; the build constraint on each `cgo_*.go` file picks the
right one based on `GOOS`, `GOARCH`, and the `musl` build tag.

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
