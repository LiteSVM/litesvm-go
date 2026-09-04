package litesvm

import (
	"crypto/ed25519"
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/programs/token"
)

// Benchmarks for the common workloads. Transaction construction (build +
// ed25519 sign + LatestBlockhash fetch) stays INSIDE the timed loop for the
// send/simulate benchmarks so the numbers include the Go-side cost;
// BenchmarkBuildSignTransfer measures that cost in isolation so the
// engine-only delta can be read off.

const (
	// 500k SOL: covers fees + transfers for any b.N while staying under the
	// 1M-SOL faucet balance.
	benchAirdrop = 500_000 * lamportsPerSol

	benchMintLen         = 82
	benchTokenAccountLen = 165
)

// benchMemoV3ProgramID is SPL Memo v3, part of the default program set.
var benchMemoV3ProgramID = solana.MustPublicKeyFromBase58("MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr")

// benchSVM returns a VM closed when the benchmark ends.
func benchSVM(b *testing.B) *LiteSVM {
	b.Helper()
	svm, err := New()
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	b.Cleanup(svm.Close)
	return svm
}

// benchKey derives a deterministic keypair so runs are comparable.
func benchKey(name string) solana.PrivateKey {
	sum := sha256.Sum256([]byte("litesvm-go:bench:" + name))
	return solana.PrivateKey(ed25519.NewKeyFromSeed(sum[:]))
}

func benchAirdropPayer(b *testing.B, svm *LiteSVM, payer solana.PrivateKey) {
	b.Helper()
	if err := svm.Airdrop(payer.PublicKey(), benchAirdrop); err != nil {
		b.Fatalf("airdrop: %v", err)
	}
}

func benchSendOK(b *testing.B, svm *LiteSVM, txBytes []byte) {
	b.Helper()
	out, err := svm.SendLegacyTransaction(txBytes)
	if err != nil {
		b.Fatalf("send: %v", err)
	}
	if !out.IsOk() {
		b.Fatalf("tx failed: %s logs=%v", out.Error(), out.Logs())
	}
}

// benchSign builds, signs, and marshals a legacy transaction.
func benchSign(ixs []solana.Instruction, blockhash solana.Hash, payer solana.PrivateKey, extraSigners ...solana.PrivateKey) ([]byte, error) {
	tx, err := solana.NewTransaction(ixs, blockhash, solana.TransactionPayer(payer.PublicKey()))
	if err != nil {
		return nil, fmt.Errorf("build tx: %w", err)
	}
	signers := append([]solana.PrivateKey{payer}, extraSigners...)
	if _, err := tx.Sign(func(k solana.PublicKey) *solana.PrivateKey {
		for i := range signers {
			if k.Equals(signers[i].PublicKey()) {
				return &signers[i]
			}
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("sign tx: %w", err)
	}
	return tx.MarshalBinary()
}

func benchTransferAt(from solana.PrivateKey, to solana.PublicKey, lamports uint64, blockhash solana.Hash) ([]byte, error) {
	ix := system.NewTransferInstruction(lamports, from.PublicKey(), to).Build()
	return benchSign([]solana.Instruction{ix}, blockhash, from)
}

func benchTransferTx(svm *LiteSVM, from solana.PrivateKey, to solana.PublicKey, lamports uint64) ([]byte, error) {
	blockhash, err := svm.LatestBlockhash()
	if err != nil {
		return nil, err
	}
	return benchTransferAt(from, to, lamports, blockhash)
}

// ---------------------------------------------------------------------------
// System transfer (native program, no BPF)
// ---------------------------------------------------------------------------

// BenchmarkTransfer sends b.N system transfers from one funded payer to a
// rotating set of 256 pre-generated recipients. The lamport amount is the
// rent-exempt minimum plus the iteration index so every transaction is
// byte-unique (the blockhash never advances, and duplicate signatures are
// rejected via transaction history).
func BenchmarkTransfer(b *testing.B) {
	svm := benchSVM(b)
	payer := benchKey("transfer-payer")
	benchAirdropPayer(b, svm, payer)

	base, err := svm.MinimumBalanceForRentExemption(0)
	if err != nil {
		b.Fatalf("rent min: %v", err)
	}
	recipients := make([]solana.PublicKey, 256)
	for i := range recipients {
		recipients[i] = benchKey(fmt.Sprintf("transfer-recipient-%d", i)).PublicKey()
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		txBytes, err := benchTransferTx(svm, payer, recipients[i%len(recipients)], base+uint64(i))
		if err != nil {
			b.Fatalf("build: %v", err)
		}
		benchSendOK(b, svm, txBytes)
	}
}

// ---------------------------------------------------------------------------
// Memo (real BPF execution)
// ---------------------------------------------------------------------------

// BenchmarkMemo sends b.N memo-v3 transactions. The memo body carries the
// iteration index for signature uniqueness; memo is a real BPF program (with
// UTF-8 validation and logging), so this exercises the sbpf interpreter.
func BenchmarkMemo(b *testing.B) {
	svm := benchSVM(b)
	payer := benchKey("memo-payer")
	benchAirdropPayer(b, svm, payer)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		blockhash, err := svm.LatestBlockhash()
		if err != nil {
			b.Fatalf("blockhash: %v", err)
		}
		ix := solana.NewInstruction(benchMemoV3ProgramID, solana.AccountMetaSlice{},
			[]byte(fmt.Sprintf("litesvm-go bench memo %d", i)))
		txBytes, err := benchSign([]solana.Instruction{ix}, blockhash, payer)
		if err != nil {
			b.Fatalf("build: %v", err)
		}
		benchSendOK(b, svm, txBytes)
	}
}

// ---------------------------------------------------------------------------
// SPL token transfer (p-token BPF program)
// ---------------------------------------------------------------------------

// BenchmarkTokenTransfer initializes a mint and two token accounts outside
// the timer, seeds both with a large supply, then sends b.N p-token
// transfers alternating direction so balances never drain. Both legs of pair
// p move 1+p base units, so consecutive pairs net to zero while every
// transaction stays byte-unique.
func BenchmarkTokenTransfer(b *testing.B) {
	svm := benchSVM(b)
	if err := svm.SetDefaultPrograms(); err != nil {
		b.Fatalf("set default programs: %v", err)
	}
	if err := svm.WithNativeMints(); err != nil {
		b.Fatalf("with native mints: %v", err)
	}

	payer := benchKey("token-payer")
	alice := benchKey("token-alice")
	bob := benchKey("token-bob")
	mint := benchKey("token-mint")
	acctA := benchKey("token-acct-a")
	acctB := benchKey("token-acct-b")
	benchAirdropPayer(b, svm, payer)

	mintRent, err := svm.MinimumBalanceForRentExemption(benchMintLen)
	if err != nil {
		b.Fatalf("mint rent: %v", err)
	}
	tokenRent, err := svm.MinimumBalanceForRentExemption(benchTokenAccountLen)
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

	blockhash, err := svm.LatestBlockhash()
	if err != nil {
		b.Fatalf("blockhash: %v", err)
	}
	setupTx, err := benchSign([]solana.Instruction{
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
	benchSendOK(b, svm, setupTx)

	// Seed both sides so alternating transfers never drain either account.
	const supplyPerSide = uint64(1) << 40
	for _, dst := range []solana.PublicKey{acctA.PublicKey(), acctB.PublicKey()} {
		mintTo, err := token.NewMintToInstruction(supplyPerSide, mint.PublicKey(),
			dst, payer.PublicKey(), nil).ValidateAndBuild()
		if err != nil {
			b.Fatalf("mint-to ix: %v", err)
		}
		blockhash, err = svm.LatestBlockhash()
		if err != nil {
			b.Fatalf("blockhash: %v", err)
		}
		txBytes, err := benchSign([]solana.Instruction{mintTo}, blockhash, payer)
		if err != nil {
			b.Fatalf("build mint-to: %v", err)
		}
		benchSendOK(b, svm, txBytes)
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
		blockhash, err := svm.LatestBlockhash()
		if err != nil {
			b.Fatalf("blockhash: %v", err)
		}
		txBytes, err := benchSign([]solana.Instruction{ix}, blockhash, payer, owner)
		if err != nil {
			b.Fatalf("build transfer: %v", err)
		}
		benchSendOK(b, svm, txBytes)
	}
}

// ---------------------------------------------------------------------------
// Simulation (no-commit path)
// ---------------------------------------------------------------------------

// BenchmarkSimulateTransfer simulates b.N identical system transfers.
// Nothing is committed and simulations leave no signature history, so the
// transaction can repeat verbatim; construction still runs inside the loop
// for parity with the send benchmarks.
func BenchmarkSimulateTransfer(b *testing.B) {
	svm := benchSVM(b)
	payer := benchKey("sim-payer")
	benchAirdropPayer(b, svm, payer)

	base, err := svm.MinimumBalanceForRentExemption(0)
	if err != nil {
		b.Fatalf("rent min: %v", err)
	}
	recipient := benchKey("sim-recipient").PublicKey()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		txBytes, err := benchTransferTx(svm, payer, recipient, base)
		if err != nil {
			b.Fatalf("build: %v", err)
		}
		out, err := svm.SimulateLegacyTransaction(txBytes)
		if err != nil {
			b.Fatalf("simulate: %v", err)
		}
		if !out.IsOk() {
			b.Fatalf("simulation failed: %s logs=%v", out.Error(), out.Logs())
		}
	}
}

// ---------------------------------------------------------------------------
// Shared Go-side construction cost
// ---------------------------------------------------------------------------

// BenchmarkBuildSignTransfer measures only the Go-side cost paid inside
// every timed loop above (minus the LatestBlockhash call): build a
// one-instruction legacy transfer, ed25519-sign it, and marshal it.
func BenchmarkBuildSignTransfer(b *testing.B) {
	payer := benchKey("build-payer")
	recipient := benchKey("build-recipient").PublicKey()
	var blockhash solana.Hash

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := benchTransferAt(payer, recipient, 1_000_000+uint64(i), blockhash); err != nil {
			b.Fatalf("build: %v", err)
		}
	}
}
