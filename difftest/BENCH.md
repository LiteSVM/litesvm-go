# Engine benchmarks: cgo (Rust litesvm, JIT) vs pure Go (mithrilsvm, interpreter)

Command:

    go test -bench . -benchtime 2s -count 3 -run '^$' -benchmem

Machine: Apple M4 Max, darwin/arm64, go1.26.2. Date: 2026-07-17.
Median of 3 samples per benchmark. Every send benchmark builds and signs the
transaction inside the loop, so both engines pay an identical Go-side cost of
~67.4us/op (measured by BenchmarkBuildSignTransfer, dominated by ed25519
signing); the "engine-only" columns subtract that baseline.

| workload            | cgo ns/op | pure ns/op | pure/cgo | engine-only cgo | engine-only pure | engine ratio |
|---------------------|-----------|------------|----------|-----------------|------------------|--------------|
| transfer (send)     | 99263     | 100799     | 1.02x    | ~31.9us         | ~33.4us          | 1.05x        |
| transfer (simulate) | 97950     | 99663      | 1.02x    | ~30.6us         | ~32.2us          | 1.05x        |
| memo (BPF)          | 129723    | 146719     | 1.13x    | ~62.3us         | ~79.3us          | 1.27x        |
| token transfer (BPF)| 230411    | 237397     | 1.03x    | ~163.0us        | ~170.0us         | 1.04x        |

Allocations per op (whole loop): transfer 2576 B / 44 allocs (cgo) vs
11523 B / 135 allocs (pure); memo 2497 B / 30 vs ~99.5 KB / 120; token
3800 B / 52 vs ~52.6 KB / 198.

## Interpretation

- On native-program work (system transfers, send or simulate) the engines
  are within ~2% wall-clock of each other; the pure engine's extra
  allocations are absorbed by the Go GC without measurable latency cost at
  this scale.
- The interpreter-vs-JIT gap only appears on BPF execution and is modest:
  memo is 13% slower end-to-end (27% on engine-only time). The p-token
  transfer, a heavier BPF workload, is only 3-4% slower overall because
  account loading and commit dominate over raw instruction dispatch.
- The pure engine allocates roughly 10-40x more bytes per transaction
  (interpreter frames, per-tx contexts). For very hot suites this is GC
  pressure to watch, but at ~100us/tx both engines execute roughly 10k
  transactions per second single-threaded.
- Conclusion for the migration: dropping the JIT costs low-single-digit
  percent on realistic test workloads and ~25% in the worst BPF-bound case
  measured here. Compute-heavy programs (crypto-dense instructions) will
  widen the gap; re-measure if a suite becomes noticeably slower.
