package mithrilsvm

import (
	"crypto/ed25519"
	"fmt"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
)

// BuildTransferTx produces a signed, wire-encoded legacy Transaction that
// transfers `lamports` from the keypair derived from `payerSeed` to `to`,
// using `blockhash`. It is a convenience for tests; application code can
// build the same transaction directly with solana-go.
func BuildTransferTx(payerSeed [32]byte, to solana.PublicKey, lamports uint64, blockhash solana.Hash) ([]byte, error) {
	// ed25519.NewKeyFromSeed performs the RFC 8032 seed expansion, identical
	// to Rust's Keypair::new_from_array, so the derived payer pubkey (and
	// thus the signature) is deterministic for a given seed.
	priv := solana.PrivateKey(ed25519.NewKeyFromSeed(payerSeed[:]))
	payer := priv.PublicKey()

	ix := system.NewTransferInstruction(lamports, payer, to).Build()
	tx, err := solana.NewTransaction(
		[]solana.Instruction{ix},
		blockhash,
		solana.TransactionPayer(payer),
	)
	if err != nil {
		return nil, fmt.Errorf("BuildTransferTx: build: %w", err)
	}
	if _, err := tx.Sign(func(k solana.PublicKey) *solana.PrivateKey {
		if k.Equals(payer) {
			return &priv
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("BuildTransferTx: sign: %w", err)
	}
	out, err := tx.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("BuildTransferTx: marshal: %w", err)
	}
	return out, nil
}
