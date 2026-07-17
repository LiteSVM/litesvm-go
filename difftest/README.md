# difftest

Differential and benchmark harness for the engine, used during
development. It runs identical scenarios against the root package
(`github.com/LiteSVM/litesvm-go`) and the `mithrilsvm` package it
re-exports, compares named observations pairwise (hard observations fail
the test on drift, soft ones are only reported), and carries paired
benchmarks for the common workloads.

Historical records, kept as-is:

- `DRIFT.md` is the final parity table recorded against the previous
  cgo-backed engine (Rust litesvm via FFI) before it was removed: 106
  observations, 0 hard drifts, 0 soft drifts (last run 2026-07-17).
- `BENCH.md` is the corresponding performance comparison between those
  two engines.

The test suite does not rewrite either file.
