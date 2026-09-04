// Package elf vendors the Solana program ELF blobs that the Rust
// litesvm 0.13.0 crate ships and installs by default (mainnet feature set),
// mirroring litesvm-0.13.0/src/programs/mod.rs.
//
// Because the mainnet feature set activates replace_spl_token_with_p_token,
// the spl-token program id is backed by the p-token (pinocchio) ELF under the
// upgradeable loader, not the legacy spl_token-3.5.0.so.
package elf

import (
	_ "embed"

	"github.com/gagliardetto/solana-go"
)

// Loader identifies which BPF loader owns a program account.
type Loader int

const (
	// LoaderV1 is the deprecated loader,
	// BPFLoader1111111111111111111111111111111111.
	LoaderV1 Loader = iota + 1
	// LoaderV2 is BPFLoader2111111111111111111111111111111111.
	LoaderV2
	// LoaderV3 is the upgradeable loader,
	// BPFLoaderUpgradeab1e11111111111111111111111.
	LoaderV3
)

// Program describes one default program: its id, owning loader, and ELF bytes.
type Program struct {
	ProgramID solana.PublicKey
	Loader    Loader
	Elf       []byte
	Name      string // human-readable, e.g. "spl-token (p-token)"
}

//go:embed pinocchio_token_program.so
var pinocchioTokenElf []byte

//go:embed spl_token_2022-11.0.0.so
var token2022Elf []byte

//go:embed spl_memo-1.0.0.so
var memoV1Elf []byte

//go:embed spl_memo-3.0.0.so
var memoV3Elf []byte

//go:embed spl_associated_token_account-1.1.1.so
var associatedTokenElf []byte

//go:embed address_lookup_table.so
var addressLookupTableElf []byte

//go:embed core_bpf_stake-5.0.0.so
var coreBpfStakeElf []byte

// Defaults lists the seven programs installed by litesvm 0.13.0's
// load_default_programs, in mod.rs order.
var Defaults = []Program{
	{
		ProgramID: solana.MustPublicKeyFromBase58("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"),
		Loader:    LoaderV3,
		Elf:       pinocchioTokenElf,
		Name:      "spl-token (p-token)",
	},
	{
		ProgramID: solana.MustPublicKeyFromBase58("TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"),
		Loader:    LoaderV3,
		Elf:       token2022Elf,
		Name:      "spl-token-2022",
	},
	{
		ProgramID: solana.MustPublicKeyFromBase58("Memo1UhkJRfHyvLMcVucJwxXeuD728EqVDDwQDxFMNo"),
		Loader:    LoaderV1,
		Elf:       memoV1Elf,
		Name:      "spl-memo v1",
	},
	{
		ProgramID: solana.MustPublicKeyFromBase58("MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr"),
		Loader:    LoaderV2,
		Elf:       memoV3Elf,
		Name:      "spl-memo v3",
	},
	{
		ProgramID: solana.MustPublicKeyFromBase58("ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL"),
		Loader:    LoaderV2,
		Elf:       associatedTokenElf,
		Name:      "spl-associated-token-account",
	},
	{
		ProgramID: solana.MustPublicKeyFromBase58("AddressLookupTab1e1111111111111111111111111"),
		Loader:    LoaderV3,
		Elf:       addressLookupTableElf,
		Name:      "address-lookup-table (core BPF)",
	},
	{
		ProgramID: solana.MustPublicKeyFromBase58("Stake11111111111111111111111111111111111111"),
		Loader:    LoaderV3,
		Elf:       coreBpfStakeElf,
		Name:      "stake (core BPF)",
	},
}

// Native mint addresses seeded alongside the token programs.
var (
	// NativeMintSPL is the wrapped SOL mint owned by spl-token.
	NativeMintSPL = solana.MustPublicKeyFromBase58("So11111111111111111111111111111111111111112")
	// NativeMintToken2022 is the native mint owned by spl-token-2022.
	NativeMintToken2022 = solana.MustPublicKeyFromBase58("9pan9bMn5HatX4EJdBwg9VgCa7Uz5HL8N1m5D3NdXejP")
)

// NativeMintLamports is the rent-exempt minimum for an 82-byte account under
// the default Rent sysvar: (128 + 82) * 6960 * 1.0 (identical to the classic
// 3480 * 2.0 product).
const NativeMintLamports = 1461600

// NativeMintData returns the 82-byte SPL Mint account layout for the native
// mint: no mint authority, zero supply, decimals = 9 (offset 44),
// is_initialized = 1 (offset 45), no freeze authority.
func NativeMintData() []byte {
	data := make([]byte, 82)
	data[44] = 9 // decimals
	data[45] = 1 // is_initialized
	return data
}
