<div align="center">
    <img src="https://raw.githubusercontent.com/litesvm/litesvm/master/logo.jpeg" width="50%" height="50%">
</div>

---

# litesvm-go

[![Go Reference](https://pkg.go.dev/badge/github.com/LiteSVM/litesvm-go.svg)](https://pkg.go.dev/github.com/LiteSVM/litesvm-go)
[![License: Apache-2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

The Go take on [LiteSVM](https://github.com/LiteSVM/litesvm): a fast,
in-process Solana VM for testing programs and clients. Pure Go, no cgo;
a fresh VM boots in milliseconds and runs in the same process as your
tests. Core types (`PublicKey`, `Hash`, `Signature`) come from
[gagliardetto/solana-go](https://github.com/gagliardetto/solana-go), so
values flow naturally to and from the rest of the Go Solana ecosystem.

## Install

```sh
go get github.com/LiteSVM/litesvm-go
```

Requires Go 1.25.7 or newer; cross-compilation works with plain
`GOOS`/`GOARCH`. Note: until the patched `mithril` and `solana-go`
dependencies are published, `go.mod` carries two local-path `replace`
directives that you must point at your own checkouts.

## Quick start

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

    priv, _ := solana.NewRandomPrivateKey()
    payer := priv.PublicKey()
    recipient := solana.NewWallet().PublicKey()

    if err := svm.Airdrop(payer, 2_000_000_000); err != nil {
        t.Fatal(err)
    }

    // Build, sign, and encode a transfer with solana-go.
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

    out, err := svm.SendLegacyTransaction(txBytes)
    if err != nil {
        t.Fatal(err)
    }
    if !out.IsOk() {
        t.Fatalf("tx failed: %s\nlogs: %v", out.Error(), out.Logs())
    }

    if lamports, _, _ := svm.Balance(recipient); lamports != 1_000_000_000 {
        t.Fatalf("recipient balance = %d, want 1_000_000_000", lamports)
    }
}
```

## Features

- Send and simulate transactions, legacy and versioned (v0, including
  address-table lookups). Every entry point returns `(value, error)`,
  wrapped under a single sentinel detectable with `errors.Is`.
- Accounts: airdrops, read/write any account, rent-exempt minimums.
- Sysvars: read and overwrite Clock, Rent, EpochSchedule, EpochRewards,
  SlotHashes, StakeHistory, and more; `WarpToSlot` for time travel.
- Feature gates: activate or deactivate any runtime feature per instance.
- Compute budget configuration per instance.
- Program loading from bytes or file, under any loader; a fresh instance
  ships the default program set (SPL Token, Token-2022, Memo, ATA,
  Address Lookup Table, Stake) preloaded.
- Transaction history with duplicate detection (`GetTransaction`).
- Instances are fully isolated from each other.

## Notes

- The engine is [mithril](https://github.com/Overclock-Validator/mithril)'s
  SVM, running as pure Go inside your test process. It is consumed from the
  [sonicfromnewyoke/mithril](https://github.com/sonicfromnewyoke/mithril)
  fork, which carries the patches this package needs.
- Transaction error strings (`TxOutcome.Error`) are Agave wire-format
  JSON, e.g. `{"InstructionError":[0,{"Custom":1}]}`.
- `SetComputeBudget` honors `ComputeUnitLimit` and `HeapSize` only; every
  other field is a VM cost constant baked into the engine and must keep
  its default value.
- A `LiteSVM` handle is not safe for concurrent use from multiple
  goroutines; confine it to one goroutine or guard it with a mutex.

## Contributing

Everything is plain Go: clone and run `go test -count=1 ./...`. The root
package is the entire public API; the engine internals live alongside it
and the vendored program ELFs and feature-gate table live under
`internal/`. New API goes in the root package with tests in
`litesvm_test.go`. Benchmarks for the common workloads are in
`bench_test.go` (`go test -run '^$' -bench .`).

## License

Apache-2.0. See [LICENSE](./LICENSE).
