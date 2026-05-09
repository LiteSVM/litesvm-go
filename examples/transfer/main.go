// Command transfer is a minimal end-to-end example: spin up a fresh
// LiteSVM, fund a payer, send a system-program transfer, and print the
// resulting balances. Run with:
//
//	go run ./examples/transfer
package main

import (
	"fmt"
	"log"

	litesvm "github.com/LiteSVM/litesvm-go"
	solana "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
)

func main() {
	svm, err := litesvm.New()
	if err != nil {
		log.Fatalf("litesvm.New: %v", err)
	}
	defer svm.Close()

	priv, err := solana.NewRandomPrivateKey()
	if err != nil {
		log.Fatalf("NewRandomPrivateKey: %v", err)
	}
	payer := priv.PublicKey()
	recipient := solana.NewWallet().PublicKey()

	if err := svm.Airdrop(payer, 2_000_000_000); err != nil {
		log.Fatalf("Airdrop: %v", err)
	}

	blockhash, err := svm.LatestBlockhash()
	if err != nil {
		log.Fatalf("LatestBlockhash: %v", err)
	}

	ix := system.NewTransferInstruction(1_000_000_000, payer, recipient).Build()
	tx, err := solana.NewTransaction(
		[]solana.Instruction{ix},
		blockhash,
		solana.TransactionPayer(payer),
	)
	if err != nil {
		log.Fatalf("NewTransaction: %v", err)
	}
	if _, err := tx.Sign(func(k solana.PublicKey) *solana.PrivateKey {
		if k.Equals(payer) {
			return &priv
		}
		return nil
	}); err != nil {
		log.Fatalf("Sign: %v", err)
	}
	txBytes, err := tx.MarshalBinary()
	if err != nil {
		log.Fatalf("MarshalBinary: %v", err)
	}

	out, err := svm.SendLegacyTransaction(txBytes)
	if err != nil {
		log.Fatalf("SendLegacyTransaction: %v", err)
	}
	defer out.Close()

	if !out.IsOk() {
		log.Fatalf("tx failed: %s\nlogs: %v", out.Error(), out.Logs())
	}

	payerLamports, _, _ := svm.Balance(payer)
	recipientLamports, _, _ := svm.Balance(recipient)
	fmt.Printf("payer:     %d lamports\n", payerLamports)
	fmt.Printf("recipient: %d lamports\n", recipientLamports)
	fmt.Printf("compute:   %d CU\n", out.ComputeUnits())
	fmt.Printf("fee:       %d lamports\n", out.Fee())
}
