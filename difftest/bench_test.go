package difftest

import (
	"fmt"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/programs/token"
)

// Paired benchmarks: each workload runs once against the root package and
// once against the mithrilsvm package directly. Transaction construction
// (build + ed25519 sign + LatestBlockhash fetch) stays INSIDE the timed
// loop for the send/simulate benchmarks so both engine views pay identical
// Go-side costs; BenchmarkBuildSignTransfer measures that shared cost in
// isolation so the engine-only delta can be read off.

const (
	// 500k SOL: covers fees + transfers for any b.N while staying under the
	// engines' 1M-SOL faucet balance.
	benchAirdrop = 500_000 * lamportsPerSol

	benchMintLen         = 82
	benchTokenAccountLen = 165
)

func newBenchEngine(b *testing.B, kind string) Engine {
	b.Helper()
	var (
		e   Engine
		err error
	)
	switch kind {
	case "root":
		e, err = newRootEngine()
	case "pure":
		e, err = newPureEngine()
	default:
		b.Fatalf("unknown engine kind %q", kind)
	}
	if err != nil {
		b.Fatalf("new %s engine: %v", kind, err)
	}
	b.Cleanup(e.Close)
	return e
}

// withNativeMints reaches through the adapter; the Engine interface does not
// carry it because no differential scenario needs it yet.
func withNativeMints(b *testing.B, e Engine) {
	b.Helper()
	var err error
	switch x := e.(type) {
	case *rootEngine:
		err = x.svm.WithNativeMints()
	case *pureEngine:
		err = x.svm.WithNativeMints()
	default:
		b.Fatalf("unknown engine type %T", e)
	}
	if err != nil {
		b.Fatalf("with native mints: %v", err)
	}
}

func benchAirdropPayer(b *testing.B, e Engine, payer solana.PrivateKey) {
	b.Helper()
	if err := e.Airdrop(payer.PublicKey(), benchAirdrop); err != nil {
		b.Fatalf("airdrop: %v", err)
	}
}

func mustSendOK(b *testing.B, e Engine, txBytes []byte) {
	b.Helper()
	r, err := e.SendTx(txBytes)
	if err != nil {
		b.Fatalf("send: %v", err)
	}
	if !r.Ok {
		b.Fatalf("tx failed: %s logs=%v", r.Err, r.Logs)
	}
}

// ---------------------------------------------------------------------------
// System transfer (native program, no BPF)
// ---------------------------------------------------------------------------

// benchTransfer sends b.N system transfers from one funded payer to a
// rotating set of 256 pre-generated recipients. The lamport amount is the
// rent-exempt minimum plus the iteration index so every transaction is
// byte-unique (the blockhash never advances, and both engines reject
// duplicate signatures via transaction history).
func benchTransfer(b *testing.B, e Engine) {
	payer := seededKey("bench-transfer-payer")
	benchAirdropPayer(b, e, payer)

	base, err := e.MinimumBalanceForRentExemption(0)
	if err != nil {
		b.Fatalf("rent min: %v", err)
	}
	recipients := make([]solana.PublicKey, 256)
	for i := range recipients {
		recipients[i] = seededKey(fmt.Sprintf("bench-transfer-recipient-%d", i)).PublicKey()
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		txBytes, err := buildTransfer(e, payer, recipients[i%len(recipients)], base+uint64(i))
		if err != nil {
			b.Fatalf("build: %v", err)
		}
		mustSendOK(b, e, txBytes)
	}
}

func BenchmarkTransferRoot(b *testing.B) { benchTransfer(b, newBenchEngine(b, "root")) }
func BenchmarkTransferPure(b *testing.B) { benchTransfer(b, newBenchEngine(b, "pure")) }

// ---------------------------------------------------------------------------
// Memo (real BPF execution: interpreter vs JIT)
// ---------------------------------------------------------------------------

// benchMemo sends b.N memo-v3 transactions. The memo body carries the
// iteration index for signature uniqueness; memo is a real BPF program (with
// UTF-8 validation and logging), so this is the interpreter-vs-JIT number.
func benchMemo(b *testing.B, e Engine) {
	payer := seededKey("bench-memo-payer")
	benchAirdropPayer(b, e, payer)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		txBytes, err := buildMemo(e, payer, fmt.Sprintf("litesvm-go bench memo %d", i))
		if err != nil {
			b.Fatalf("build: %v", err)
		}
		mustSendOK(b, e, txBytes)
	}
}

func BenchmarkMemoRoot(b *testing.B) { benchMemo(b, newBenchEngine(b, "root")) }
func BenchmarkMemoPure(b *testing.B) { benchMemo(b, newBenchEngine(b, "pure")) }

// ---------------------------------------------------------------------------
// SPL token transfer (p-token BPF program)
// ---------------------------------------------------------------------------

// benchTokenTransfer initializes a mint and two token accounts outside the
// timer, seeds both with a large supply, then sends b.N p-token transfers
// alternating direction so balances never drain. Both legs of pair p move
// 1+p base units, so consecutive pairs net to zero while every transaction
// stays byte-unique.
func benchTokenTransfer(b *testing.B, e Engine) {
	if err := e.SetDefaultPrograms(); err != nil {
		b.Fatalf("set default programs: %v", err)
	}
	withNativeMints(b, e)

	payer := seededKey("bench-token-payer")
	alice := seededKey("bench-token-alice")
	bob := seededKey("bench-token-bob")
	mint := seededKey("bench-token-mint")
	acctA := seededKey("bench-token-acct-a")
	acctB := seededKey("bench-token-acct-b")
	benchAirdropPayer(b, e, payer)

	mintRent, err := e.MinimumBalanceForRentExemption(benchMintLen)
	if err != nil {
		b.Fatalf("mint rent: %v", err)
	}
	tokenRent, err := e.MinimumBalanceForRentExemption(benchTokenAccountLen)
	if err != nil {
		b.Fatalf("token rent: %v", err)
	}

	initMint, err := token.NewInitializeMint2InstructionBuilder().
		SetDecimals(6).
		SetMintAuthority(payer.PublicKey()).
		SetMintAccount(mint.PublicKey()).
		ValidateAndBuild()
	if err != nil {
		b.Fatalf("init mint ix: %v", err)
	}
	initA, err := token.NewInitializeAccount3Instruction(
		alice.PublicKey(), acctA.PublicKey(), mint.PublicKey()).ValidateAndBuild()
	if err != nil {
		b.Fatalf("init acct A ix: %v", err)
	}
	initB, err := token.NewInitializeAccount3Instruction(
		bob.PublicKey(), acctB.PublicKey(), mint.PublicKey()).ValidateAndBuild()
	if err != nil {
		b.Fatalf("init acct B ix: %v", err)
	}

	blockhash, err := e.LatestBlockhash()
	if err != nil {
		b.Fatalf("blockhash: %v", err)
	}
	setupTx, err := buildSignedTx(e, []solana.Instruction{
		system.NewCreateAccountInstruction(mintRent, benchMintLen, solana.TokenProgramID,
			payer.PublicKey(), mint.PublicKey()).Build(),
		initMint,
		system.NewCreateAccountInstruction(tokenRent, benchTokenAccountLen, solana.TokenProgramID,
			payer.PublicKey(), acctA.PublicKey()).Build(),
		initA,
		system.NewCreateAccountInstruction(tokenRent, benchTokenAccountLen, solana.TokenProgramID,
			payer.PublicKey(), acctB.PublicKey()).Build(),
		initB,
	}, blockhash, payer, mint, acctA, acctB)
	if err != nil {
		b.Fatalf("build setup tx: %v", err)
	}
	mustSendOK(b, e, setupTx)

	// Seed both sides so alternating transfers never drain either account.
	const supplyPerSide = uint64(1) << 40
	for _, dst := range []solana.PublicKey{acctA.PublicKey(), acctB.PublicKey()} {
		mintTo, err := token.NewMintToInstruction(supplyPerSide, mint.PublicKey(),
			dst, payer.PublicKey(), nil).ValidateAndBuild()
		if err != nil {
			b.Fatalf("mint-to ix: %v", err)
		}
		blockhash, err = e.LatestBlockhash()
		if err != nil {
			b.Fatalf("blockhash: %v", err)
		}
		txBytes, err := buildSignedTx(e, []solana.Instruction{mintTo}, blockhash, payer)
		if err != nil {
			b.Fatalf("build mint-to: %v", err)
		}
		mustSendOK(b, e, txBytes)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		src, dst, owner := acctA.PublicKey(), acctB.PublicKey(), alice
		if i%2 == 1 {
			src, dst, owner = acctB.PublicKey(), acctA.PublicKey(), bob
		}
		amount := 1 + uint64(i/2)
		ix, err := token.NewTransferInstruction(amount, src, dst, owner.PublicKey(), nil).ValidateAndBuild()
		if err != nil {
			b.Fatalf("transfer ix: %v", err)
		}
		blockhash, err := e.LatestBlockhash()
		if err != nil {
			b.Fatalf("blockhash: %v", err)
		}
		txBytes, err := buildSignedTx(e, []solana.Instruction{ix}, blockhash, payer, owner)
		if err != nil {
			b.Fatalf("build transfer: %v", err)
		}
		mustSendOK(b, e, txBytes)
	}
}

func BenchmarkTokenTransferRoot(b *testing.B) { benchTokenTransfer(b, newBenchEngine(b, "root")) }
func BenchmarkTokenTransferPure(b *testing.B) { benchTokenTransfer(b, newBenchEngine(b, "pure")) }

// ---------------------------------------------------------------------------
// Simulation (no-commit path)
// ---------------------------------------------------------------------------

// benchSimulateTransfer simulates b.N identical system transfers. Nothing is
// committed and simulations leave no signature history, so the transaction
// can repeat verbatim; construction still runs inside the loop for parity
// with the send benchmarks.
func benchSimulateTransfer(b *testing.B, e Engine) {
	payer := seededKey("bench-sim-payer")
	benchAirdropPayer(b, e, payer)

	base, err := e.MinimumBalanceForRentExemption(0)
	if err != nil {
		b.Fatalf("rent min: %v", err)
	}
	recipient := seededKey("bench-sim-recipient").PublicKey()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		txBytes, err := buildTransfer(e, payer, recipient, base)
		if err != nil {
			b.Fatalf("build: %v", err)
		}
		r, err := e.SimulateTx(txBytes)
		if err != nil {
			b.Fatalf("simulate: %v", err)
		}
		if !r.Ok {
			b.Fatalf("simulation failed: %s logs=%v", r.Err, r.Logs)
		}
	}
}

func BenchmarkSimulateTransferRoot(b *testing.B) { benchSimulateTransfer(b, newBenchEngine(b, "root")) }
func BenchmarkSimulateTransferPure(b *testing.B) { benchSimulateTransfer(b, newBenchEngine(b, "pure")) }

// ---------------------------------------------------------------------------
// Shared Go-side construction cost
// ---------------------------------------------------------------------------

// BenchmarkBuildSignTransfer measures only the Go-side cost both engines pay
// inside every timed loop above (minus the LatestBlockhash call): build a
// one-instruction legacy transfer, ed25519-sign it, and marshal it.
func BenchmarkBuildSignTransfer(b *testing.B) {
	payer := seededKey("bench-build-payer")
	recipient := seededKey("bench-build-recipient").PublicKey()
	var blockhash solana.Hash

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := buildTransferAt(nil, payer, recipient, 1_000_000+uint64(i), blockhash); err != nil {
			b.Fatalf("build: %v", err)
		}
	}
}
