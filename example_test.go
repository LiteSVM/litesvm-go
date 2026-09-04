package litesvm_test

import (
	"crypto/ed25519"
	"fmt"

	litesvm "github.com/LiteSVM/litesvm-go"
	"github.com/gagliardetto/solana-go"
)

// keypair derives a deterministic keypair for the examples below. Real
// code would use solana.NewRandomPrivateKey or load an existing key.
func keypair(b byte) ([32]byte, solana.PublicKey) {
	var seed [32]byte
	seed[0] = b
	priv := solana.PrivateKey(ed25519.NewKeyFromSeed(seed[:]))
	return seed, priv.PublicKey()
}

// Boot a fresh VM, fund an account, and read its balance. The whole
// round trip runs in-process: no validator, no RPC.
func ExampleNew() {
	svm, err := litesvm.New()
	if err != nil {
		panic(err)
	}
	defer svm.Close()

	_, wallet := keypair(1)
	if err := svm.Airdrop(wallet, 1_000_000_000); err != nil {
		panic(err)
	}

	lamports, exists, err := svm.Balance(wallet)
	if err != nil {
		panic(err)
	}
	fmt.Println(exists, lamports)
	// Output: true 1000000000
}

// Send a signed legacy transaction and inspect the outcome.
func ExampleLiteSVM_SendLegacyTransaction() {
	svm, err := litesvm.New()
	if err != nil {
		panic(err)
	}
	defer svm.Close()

	payerSeed, payer := keypair(2)
	_, recipient := keypair(3)
	if err := svm.Airdrop(payer, 1_000_000_000); err != nil {
		panic(err)
	}

	blockhash, err := svm.LatestBlockhash()
	if err != nil {
		panic(err)
	}
	txBytes, err := litesvm.BuildTransferTx(payerSeed, recipient, 1_000_000, blockhash)
	if err != nil {
		panic(err)
	}

	out, err := svm.SendLegacyTransaction(txBytes)
	if err != nil {
		panic(err)
	}
	defer out.Close()

	got, _, err := svm.Balance(recipient)
	if err != nil {
		panic(err)
	}
	fmt.Println(out.IsOk(), got)
	// Output: true 1000000
}

// Simulate a transaction to preflight it: compute units and logs are
// reported, but no state is mutated.
func ExampleLiteSVM_SimulateLegacyTransaction() {
	svm, err := litesvm.New()
	if err != nil {
		panic(err)
	}
	defer svm.Close()

	payerSeed, payer := keypair(4)
	_, recipient := keypair(5)
	if err := svm.Airdrop(payer, 1_000_000_000); err != nil {
		panic(err)
	}

	blockhash, err := svm.LatestBlockhash()
	if err != nil {
		panic(err)
	}
	txBytes, err := litesvm.BuildTransferTx(payerSeed, recipient, 1_000_000, blockhash)
	if err != nil {
		panic(err)
	}

	out, err := svm.SimulateLegacyTransaction(txBytes)
	if err != nil {
		panic(err)
	}
	defer out.Close()

	// The simulation succeeded and consumed compute units...
	fmt.Println(out.IsOk(), out.ComputeUnits() > 0)

	// ...but the recipient was never credited.
	_, exists, err := svm.Balance(recipient)
	if err != nil {
		panic(err)
	}
	fmt.Println(exists)
	// Output:
	// true true
	// false
}

// Jump the VM to an arbitrary slot — handy for testing time-locked
// logic (vesting, auctions) without waiting on a real clock.
func ExampleLiteSVM_WarpToSlot() {
	svm, err := litesvm.New()
	if err != nil {
		panic(err)
	}
	defer svm.Close()

	if err := svm.WarpToSlot(1_000); err != nil {
		panic(err)
	}

	clock, err := svm.Clock()
	if err != nil {
		panic(err)
	}
	fmt.Println(clock.Slot)
	// Output: 1000
}

// Write an arbitrary account into the VM and read it back. This is how
// you inject fixtures (e.g. accounts dumped from mainnet) into a test.
func ExampleLiteSVM_SetAccount() {
	svm, err := litesvm.New()
	if err != nil {
		panic(err)
	}
	defer svm.Close()

	_, owner := keypair(6)
	_, address := keypair(7)

	acct, err := litesvm.NewAccount(1_000_000_000, []byte("hello"), owner, false, 0)
	if err != nil {
		panic(err)
	}
	defer acct.Close()

	if err := svm.SetAccount(address, acct); err != nil {
		panic(err)
	}

	got := svm.GetAccount(address)
	if got == nil {
		panic("account not found")
	}
	defer got.Close()
	fmt.Printf("%s %d\n", got.Data(), got.Lamports())
	// Output: hello 1000000000
}
