package mithrilsvm

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/gagliardetto/solana-go"
	addresslookuptable "github.com/gagliardetto/solana-go/programs/address-lookup-table"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/sysvar"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// altAccountData hand-crafts an initialized address-lookup-table account:
// 56-byte meta (per mithril's UnmarshalAddressLookupTable /
// LookupTableMeta.UnmarshalWithDecoder, AddressLookupTableMetaSize = 56)
// followed by the raw 32-byte addresses.
//
// Layout:
//
//	offset  0: u32  state discriminant (1 = LookupTable)
//	offset  4: u64  deactivation slot (MaxUint64 = active)
//	offset 12: u64  last extended slot
//	offset 20: u8   last extended slot start index
//	offset 21: u8   authority option tag (1 = Some)
//	offset 22: [32] authority
//	offset 54: u16  padding
//	offset 56: raw addresses, 32 bytes each
func altAccountData(authority solana.PublicKey, addrs ...solana.PublicKey) []byte {
	return altAccountDataMeta(authority, math.MaxUint64, 0, 0, addrs...)
}

// altAccountDataMeta is altAccountData with explicit deactivation slot,
// last-extended slot, and last-extended-slot start index.
func altAccountDataMeta(authority solana.PublicKey, deactivationSlot, lastExtendedSlot uint64, lastExtendedStartIndex byte, addrs ...solana.PublicKey) []byte {
	data := make([]byte, 56+32*len(addrs))
	binary.LittleEndian.PutUint32(data[0:4], 1)                  // state: LookupTable
	binary.LittleEndian.PutUint64(data[4:12], deactivationSlot)  // deactivation slot
	binary.LittleEndian.PutUint64(data[12:20], lastExtendedSlot) // last extended slot
	data[20] = lastExtendedStartIndex                            // last extended slot start index
	data[21] = 1                                                 // Some(authority)
	copy(data[22:54], authority[:])                              // authority
	binary.LittleEndian.PutUint16(data[54:56], 0)                // padding
	for i, addr := range addrs {
		copy(data[56+32*i:], addr[:])
	}
	return data
}

// TestV0TransactionLookupResolution sends a v0 transaction whose transfer
// recipient is reachable only through an address lookup table account: the
// recipient must not appear in the static keys and must resolve through the
// MessageAddressTableLookup entry.
func TestV0TransactionLookupResolution(t *testing.T) {
	svm := mustNew(t)
	payer := solana.NewWallet()
	require.NoError(t, svm.Airdrop(payer.PublicKey(), 2*lamportsPerSol))

	recipient := solana.NewWallet().PublicKey()
	authority := solana.NewWallet().PublicKey()
	tableAddr := solana.NewWallet().PublicKey()

	tableData := altAccountData(authority, recipient)
	rent, err := svm.MinimumBalanceForRentExemption(len(tableData))
	require.NoError(t, err)
	require.NoError(t, svm.SetAccount(tableAddr, &solana.Account{
		Lamports: rent,
		Data:     tableData,
		Owner:    solana.AddressLookupTableProgramID,
	}))

	// The hand-crafted table carries LastExtendedSlot = 0, and addresses
	// appended in the current slot are not visible (AddressLookupTable::
	// lookup caps at LastExtendedSlotStartIndex when last_extended_slot ==
	// clock.slot); move past the genesis slot so the address resolves,
	// exactly as required on the Rust engine.
	require.NoError(t, svm.WarpToSlot(1))

	const amount = uint64(123_456_789)
	blockhash, err := svm.LatestBlockhash()
	require.NoError(t, err)
	ix := system.NewTransferInstruction(amount, payer.PublicKey(), recipient).Build()
	tx, err := solana.NewTransaction(
		[]solana.Instruction{ix},
		blockhash,
		solana.TransactionPayer(payer.PublicKey()),
		solana.TransactionAddressTables(map[solana.PublicKey]solana.PublicKeySlice{
			tableAddr: {recipient},
		}),
	)
	require.NoError(t, err)

	// The recipient must be referenced via the table, not statically.
	require.Equal(t, 1, tx.Message.GetAddressTableLookups().NumLookups())
	for _, k := range tx.Message.AccountKeys {
		require.False(t, k.Equals(recipient), "recipient leaked into static keys")
	}

	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(payer.PublicKey()) {
			return &payer.PrivateKey
		}
		return nil
	})
	require.NoError(t, err)
	txBytes, err := tx.MarshalBinary()
	require.NoError(t, err)

	// The wire format must be a v0 versioned transaction: the first message
	// byte (after the compact-u16 signature count and the signatures) has the
	// version prefix bit set.
	numSigs := int(txBytes[0]) // < 128 signatures, so compact-u16 is 1 byte
	require.Equal(t, 1, numSigs)
	assert.Equal(t, byte(0x80), txBytes[1+64*numSigs]&0x80, "expected v0 wire prefix")

	// Round-trip decode sees the lookups.
	decoded, err := solana.TransactionFromBytes(txBytes)
	require.NoError(t, err)
	require.Equal(t, 1, decoded.Message.GetAddressTableLookups().NumLookups())

	outcome, err := svm.SendVersionedTransaction(txBytes)
	require.NoError(t, err)
	require.True(t, outcome.IsOk(), "v0 transfer failed: %s logs=%v", outcome.Error(), outcome.Logs())

	bal, exists, err := svm.Balance(recipient)
	require.NoError(t, err)
	require.True(t, exists, "recipient account missing after v0 transfer")
	assert.Equal(t, amount, bal)
}

// TestV0TransactionMissingTable asserts the engine reports the Agave error
// for a lookup against a table account that does not exist.
func TestV0TransactionMissingTable(t *testing.T) {
	svm := mustNew(t)
	payer := solana.NewWallet()
	require.NoError(t, svm.Airdrop(payer.PublicKey(), 2*lamportsPerSol))

	recipient := solana.NewWallet().PublicKey()
	tableAddr := solana.NewWallet().PublicKey() // never created

	blockhash, err := svm.LatestBlockhash()
	require.NoError(t, err)
	ix := system.NewTransferInstruction(1, payer.PublicKey(), recipient).Build()
	tx, err := solana.NewTransaction(
		[]solana.Instruction{ix},
		blockhash,
		solana.TransactionPayer(payer.PublicKey()),
		solana.TransactionAddressTables(map[solana.PublicKey]solana.PublicKeySlice{
			tableAddr: {recipient},
		}),
	)
	require.NoError(t, err)
	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(payer.PublicKey()) {
			return &payer.PrivateKey
		}
		return nil
	})
	require.NoError(t, err)
	txBytes, err := tx.MarshalBinary()
	require.NoError(t, err)

	outcome, err := svm.SendVersionedTransaction(txBytes)
	require.NoError(t, err)
	require.False(t, outcome.IsOk())
	assert.Contains(t, outcome.Error(), "AddressLookupTableNotFound")
}

// TestCreateLookupTableProgram executes the vendored core-BPF address lookup
// table program (installed by SetDefaultPrograms under the upgradeable
// loader) with a CreateLookupTable instruction, seeding SlotHashes so the
// recent-slot check passes, and asserts the table account is created.
func TestCreateLookupTableProgram(t *testing.T) {
	svm := mustNew(t)
	require.NoError(t, svm.SetDefaultPrograms())

	payer := solana.NewWallet()
	require.NoError(t, svm.Airdrop(payer.PublicKey(), 2*lamportsPerSol))

	// The program validates the recent slot against SlotHashes; make slot
	// 100 a real, hashed slot and move the clock past it.
	const recentSlot = uint64(100)
	require.NoError(t, svm.WarpToSlot(recentSlot+1))
	require.NoError(t, svm.SetSlotHashes(sysvar.NewSlotHashes(
		sysvar.SlotHash{Slot: recentSlot, Hash: solana.Hash{0x01}},
	)))

	builder, tableAddr, err := addresslookuptable.NewCreateLookupTableInstruction(
		payer.PublicKey(), payer.PublicKey(), recentSlot)
	require.NoError(t, err)
	ix, err := builder.ValidateAndBuild()
	require.NoError(t, err)

	outcome := signAndSend(t, svm, payer.PrivateKey, []solana.Instruction{ix})
	require.True(t, outcome.IsOk(), "CreateLookupTable failed: %s logs=%v", outcome.Error(), outcome.Logs())
	assert.Greater(t, outcome.ComputeUnits(), uint64(0))

	table := svm.GetAccount(tableAddr)
	require.NotNil(t, table, "lookup table account was not created")
	assert.Equal(t, solana.AddressLookupTableProgramID, table.Owner)
	require.GreaterOrEqual(t, len(table.Data), 56)
	assert.Equal(t, uint32(1), binary.LittleEndian.Uint32(table.Data[0:4]), "state: LookupTable")
	assert.Equal(t, uint64(math.MaxUint64), binary.LittleEndian.Uint64(table.Data[4:12]), "deactivation slot")
	assert.Equal(t, uint8(1), table.Data[21], "authority Some")
	assert.Equal(t, payer.PublicKey().Bytes(), table.Data[22:54], "authority")
	assert.Greater(t, table.Lamports, uint64(0))

	// The created table is immediately usable by the engine's v0 lookup
	// resolution once extended; here it has zero addresses, so resolving an
	// index against it must fail cleanly rather than panic.
	recipient := solana.NewWallet().PublicKey()
	blockhash, err := svm.LatestBlockhash()
	require.NoError(t, err)
	transferIx := system.NewTransferInstruction(1, payer.PublicKey(), recipient).Build()
	tx, err := solana.NewTransaction(
		[]solana.Instruction{transferIx},
		blockhash,
		solana.TransactionPayer(payer.PublicKey()),
		solana.TransactionAddressTables(map[solana.PublicKey]solana.PublicKeySlice{
			tableAddr: {recipient}, // pretend index 0 exists
		}),
	)
	require.NoError(t, err)
	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(payer.PublicKey()) {
			return &payer.PrivateKey
		}
		return nil
	})
	require.NoError(t, err)
	txBytes, err := tx.MarshalBinary()
	require.NoError(t, err)
	badOutcome, err := svm.SendVersionedTransaction(txBytes)
	require.NoError(t, err)
	require.False(t, badOutcome.IsOk())
	assert.Contains(t, badOutcome.Error(), "InvalidAddressLookupTableIndex")
}

// buildV0Transfer builds and signs a v0 transfer whose recipient resolves
// through tableAddr using the given builder-side table contents.
func buildV0Transfer(t *testing.T, svm *LiteSVM, payer *solana.Wallet, recipient solana.PublicKey, tableAddr solana.PublicKey, builderTable solana.PublicKeySlice, amount uint64) []byte {
	t.Helper()
	blockhash, err := svm.LatestBlockhash()
	require.NoError(t, err)
	ix := system.NewTransferInstruction(amount, payer.PublicKey(), recipient).Build()
	tx, err := solana.NewTransaction(
		[]solana.Instruction{ix},
		blockhash,
		solana.TransactionPayer(payer.PublicKey()),
		solana.TransactionAddressTables(map[solana.PublicKey]solana.PublicKeySlice{
			tableAddr: builderTable,
		}),
	)
	require.NoError(t, err)
	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(payer.PublicKey()) {
			return &payer.PrivateKey
		}
		return nil
	})
	require.NoError(t, err)
	txBytes, err := tx.MarshalBinary()
	require.NoError(t, err)
	return txBytes
}

// TestDeactivatedLookupTableNotFound pins the AddressLookupTable::lookup
// deactivation rule: a table whose deactivation slot is no longer in
// SlotHashes (and is not the current slot) fails resolution with
// AddressLookupTableNotFound, exactly like the Rust engine.
func TestDeactivatedLookupTableNotFound(t *testing.T) {
	svm := mustNew(t)
	payer := solana.NewWallet()
	require.NoError(t, svm.Airdrop(payer.PublicKey(), 2*lamportsPerSol))

	recipient := solana.NewWallet().PublicKey()
	authority := solana.NewWallet().PublicKey()
	tableAddr := solana.NewWallet().PublicKey()

	// Deactivated at slot 10: slot 10 is not in the default SlotHashes
	// (which only carries slot 0) and is not the current slot, so the
	// cooldown has fully elapsed -> Deactivated.
	tableData := altAccountDataMeta(authority, 10, 0, 0, recipient)
	rent, err := svm.MinimumBalanceForRentExemption(len(tableData))
	require.NoError(t, err)
	require.NoError(t, svm.SetAccount(tableAddr, &solana.Account{
		Lamports: rent,
		Data:     tableData,
		Owner:    solana.AddressLookupTableProgramID,
	}))
	require.NoError(t, svm.WarpToSlot(600))

	txBytes := buildV0Transfer(t, svm, payer, recipient, tableAddr, solana.PublicKeySlice{recipient}, lamportsPerSol)
	outcome, err := svm.SendVersionedTransaction(txBytes)
	require.NoError(t, err)
	require.False(t, outcome.IsOk(), "deactivated table must not resolve")
	assert.Contains(t, outcome.Error(), "AddressLookupTableNotFound")
}

// TestSameSlotExtendedVisibility pins the last-extended-slot rule: addresses
// appended in the current slot are capped at LastExtendedSlotStartIndex and
// become visible one slot later.
func TestSameSlotExtendedVisibility(t *testing.T) {
	svm := mustNew(t)
	payer := solana.NewWallet()
	require.NoError(t, svm.Airdrop(payer.PublicKey(), 5*lamportsPerSol))

	recipient1 := solana.NewWallet().PublicKey()
	recipient2 := solana.NewWallet().PublicKey()
	authority := solana.NewWallet().PublicKey()
	tableAddr := solana.NewWallet().PublicKey()

	// Extended at slot 5 starting from index 1: at slot 5 only index 0 is
	// visible; from slot 6 both are.
	tableData := altAccountDataMeta(authority, math.MaxUint64, 5, 1, recipient1, recipient2)
	rent, err := svm.MinimumBalanceForRentExemption(len(tableData))
	require.NoError(t, err)
	require.NoError(t, svm.SetAccount(tableAddr, &solana.Account{
		Lamports: rent,
		Data:     tableData,
		Owner:    solana.AddressLookupTableProgramID,
	}))
	builderTable := solana.PublicKeySlice{recipient1, recipient2}

	require.NoError(t, svm.WarpToSlot(5))
	// Index 1 was appended in the current slot: not yet visible.
	outcome, err := svm.SendVersionedTransaction(
		buildV0Transfer(t, svm, payer, recipient2, tableAddr, builderTable, lamportsPerSol))
	require.NoError(t, err)
	require.False(t, outcome.IsOk(), "same-slot-extended address must be hidden")
	assert.Contains(t, outcome.Error(), "InvalidAddressLookupTableIndex")

	// Index 0 predates the extension and resolves.
	outcome, err = svm.SendVersionedTransaction(
		buildV0Transfer(t, svm, payer, recipient1, tableAddr, builderTable, lamportsPerSol))
	require.NoError(t, err)
	require.True(t, outcome.IsOk(), "pre-extension address failed: %s", outcome.Error())

	// One slot later the appended address becomes visible.
	require.NoError(t, svm.WarpToSlot(6))
	outcome, err = svm.SendVersionedTransaction(
		buildV0Transfer(t, svm, payer, recipient2, tableAddr, builderTable, lamportsPerSol))
	require.NoError(t, err)
	require.True(t, outcome.IsOk(), "post-extension address failed: %s", outcome.Error())

	bal, _, err := svm.Balance(recipient2)
	require.NoError(t, err)
	assert.Equal(t, uint64(lamportsPerSol), bal)
}
