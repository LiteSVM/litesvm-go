package litesvm

import (
	"encoding/binary"
	"fmt"
	"os"

	"github.com/gagliardetto/solana-go"
	"github.com/sonicfromnewyoke/mithril/pkg/accounts"
	"github.com/sonicfromnewyoke/mithril/pkg/addresses"
	"github.com/sonicfromnewyoke/mithril/pkg/sealevel"

	"github.com/LiteSVM/litesvm-go/internal/elf"
)

// toSolanaAccount converts a mithril account to the solana-go shape. Data is
// copied so callers cannot mutate live engine state through the returned
// slice: the accounts API has value semantics.
func toSolanaAccount(a *accounts.Account) *solana.Account {
	if a == nil {
		return nil
	}
	return &solana.Account{
		Lamports:   a.Lamports,
		Data:       append([]byte(nil), a.Data...),
		Owner:      solana.PublicKey(a.Owner),
		Executable: a.Executable,
		RentEpoch:  a.RentEpoch,
	}
}

// GetAccount returns the account at pubkey, or nil if it does not exist.
func (s *LiteSVM) GetAccount(pubkey solana.PublicKey) *solana.Account {
	acct, err := s.mem.GetAccountWithoutLock(pubkey)
	if err != nil || acct == nil {
		return nil
	}
	return toSolanaAccount(acct)
}

// SetAccount installs acct at pubkey, overwriting any existing account,
// mirroring Rust litesvm's set_account (accounts_db.rs add_account):
//
//   - the compiled-program cache entry for pubkey is evicted, so overwriting
//     a program (or programdata) account never serves stale program bytes;
//   - writing a known sysvar ADDRESS (regardless of the account's owner)
//     refreshes the typed sysvar cache, and undecodable sysvar data is
//     rejected without storing anything, keeping both views coherent;
//   - a zero-lamport account deletes the entry instead of storing it.
//
// The account data is copied, so later mutation of acct.Data by the caller
// does not affect engine state.
func (s *LiteSVM) SetAccount(pubkey solana.PublicKey, acct *solana.Account) error {
	if acct == nil {
		return fmt.Errorf("%w: SetAccount: nil account", ErrLiteSVM)
	}
	m := &accounts.Account{
		Key:        pubkey,
		Lamports:   acct.Lamports,
		Data:       append([]byte(nil), acct.Data...),
		Owner:      [32]byte(acct.Owner),
		Executable: acct.Executable,
		RentEpoch:  acct.RentEpoch,
	}
	pk := [32]byte(pubkey)

	// Keep the sysvar cache coherent when a sysvar account is overwritten
	// directly. Rust keys this on the address (maybe_handle_sysvar_account)
	// and propagates decode failures before storing the account.
	if isKnownSysvarID(pk) {
		if err := s.refreshCache(pubkey, m.Data); err != nil {
			return err
		}
		s.setCacheAcct(pk, m)
	}

	// Rust reloads the program cache from the new bytes on every set_account
	// of an executable account; mithril's cache serves compiled entries in
	// preference to account data, so evict the key and let the next
	// execution recompile from the freshly written bytes.
	s.stubDb.RemoveProgramFromCache(pubkey)

	if m.Lamports == 0 {
		// Rust removes zero-lamport accounts (accounts_db.rs add_account).
		delete(s.mem.Map, pk)
		return nil
	}
	if err := s.mem.SetAccount(&pk, m); err != nil {
		return fmt.Errorf("%w: SetAccount: %v", ErrLiteSVM, err)
	}
	return nil
}

// NewAccount builds an account value for SetAccount. Accounts are plain
// solana.Account structs, so a composite literal does the same job and is
// usually clearer:
//
//	&solana.Account{Lamports: 1e9, Data: data, Owner: owner}
//
// This constructor exists so the documented API surface can be used
// positionally. The error result is always nil; it is retained so callers
// written against it keep compiling.
func NewAccount(lamports uint64, data []byte, owner solana.PublicKey, executable bool, rentEpoch uint64) (*solana.Account, error) {
	return &solana.Account{
		Lamports:   lamports,
		Data:       append([]byte(nil), data...),
		Owner:      owner,
		Executable: executable,
		RentEpoch:  rentEpoch,
	}, nil
}

// AddProgram installs an SBF program under the upgradeable loader,
// mirroring Rust litesvm's add_program default. Like the Rust engine
// (which loads and verifies the ELF while populating its program cache),
// invalid program bytes are rejected up front.
func (s *LiteSVM) AddProgram(programID solana.PublicKey, programBytes []byte) error {
	if err := sealevel.ValidateUpgradeableLoaderProgram(programBytes, s.feats); err != nil {
		return fmt.Errorf("%w: add_program: invalid program bytes: %v", ErrLiteSVM, err)
	}
	return s.addProgramV3(programID, programBytes)
}

// AddProgramFromFile loads an SBF program ELF from disk and installs it.
func (s *LiteSVM) AddProgramFromFile(programID solana.PublicKey, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%w: AddProgramFromFile: %v", ErrLiteSVM, err)
	}
	return s.AddProgram(programID, b)
}

// AddProgramWithLoader installs an SBF program under a specific loader:
// the deprecated loader (v1), BPF loader v2, or the upgradeable loader
// (v3). Unknown loader ids are an error.
func (s *LiteSVM) AddProgramWithLoader(programID solana.PublicKey, programBytes []byte, loader solana.PublicKey) error {
	if len(programBytes) == 0 {
		return fmt.Errorf("%w: AddProgramWithLoader: empty program bytes", ErrLiteSVM)
	}
	switch [32]byte(loader) {
	case addresses.BpfLoaderDeprecatedAddr:
		return s.addProgramV1V2(programID, programBytes, addresses.BpfLoaderDeprecatedAddr)
	case addresses.BpfLoader2Addr:
		return s.addProgramV1V2(programID, programBytes, addresses.BpfLoader2Addr)
	case addresses.BpfLoaderUpgradeableAddr:
		// Route through AddProgram so upgradeable-loader installs get the
		// same ELF verification as Rust litesvm's add_program.
		return s.AddProgram(programID, programBytes)
	default:
		return fmt.Errorf("%w: AddProgramWithLoader: unknown loader %s", ErrLiteSVM, loader)
	}
}

// addProgramV1V2 installs a program as a single loader-owned account
// (deprecated loader or BPF loader v2 semantics). The compiled-program
// cache entry for the program id is evicted so re-installing under the same
// id executes the new bytes (mithril caches v1/v2 programs keyed on the
// program account, bpf_loader.go executeProgramFromBytes).
func (s *LiteSVM) addProgramV1V2(programID solana.PublicKey, programBytes []byte, loader [32]byte) error {
	rentMin, err := s.MinimumBalanceForRentExemption(len(programBytes))
	if err != nil {
		return err
	}
	acct := &accounts.Account{
		Key:        programID,
		Lamports:   rentMin,
		Data:       programBytes,
		Owner:      loader,
		Executable: true,
	}
	pk := [32]byte(programID)
	if err := s.mem.SetAccount(&pk, acct); err != nil {
		return fmt.Errorf("%w: add program: %v", ErrLiteSVM, err)
	}
	s.stubDb.RemoveProgramFromCache(programID)
	return nil
}

// addProgramV3 installs a program under the upgradeable loader: a program
// account pointing at a canonical programdata account holding the ELF.
func (s *LiteSVM) addProgramV3(programID solana.PublicKey, programBytes []byte) error {
	upgradeable := solana.PublicKey(addresses.BpfLoaderUpgradeableAddr)
	programData, _, err := solana.FindProgramAddress([][]byte{programID[:]}, upgradeable)
	if err != nil {
		return fmt.Errorf("%w: derive programdata address: %v", ErrLiteSVM, err)
	}

	// Program account: enum Program = 2, then the programdata address.
	progAcctData := make([]byte, 4+32)
	binary.LittleEndian.PutUint32(progAcctData[0:4], 2)
	copy(progAcctData[4:], programData[:])

	// ProgramData account: enum ProgramData = 3, slot u64, Option<authority>
	// = Some(zeroed) to keep the program upgradable-shaped, then the ELF.
	pdData := make([]byte, 4+8+1+32+len(programBytes))
	binary.LittleEndian.PutUint32(pdData[0:4], 3)
	binary.LittleEndian.PutUint64(pdData[4:12], 0) // deployed at slot 0
	pdData[12] = 1                                 // Some(authority)
	copy(pdData[13+32:], programBytes)

	rentProg, err := s.MinimumBalanceForRentExemption(len(progAcctData))
	if err != nil {
		return err
	}
	rentPD, err := s.MinimumBalanceForRentExemption(len(pdData))
	if err != nil {
		return err
	}

	pdPk := [32]byte(programData)
	if err := s.mem.SetAccount(&pdPk, &accounts.Account{
		Key:      programData,
		Lamports: rentPD,
		Data:     pdData,
		Owner:    addresses.BpfLoaderUpgradeableAddr,
	}); err != nil {
		return fmt.Errorf("%w: write programdata account: %v", ErrLiteSVM, err)
	}

	pk := [32]byte(programID)
	if err := s.mem.SetAccount(&pk, &accounts.Account{
		Key:        programID,
		Lamports:   rentProg,
		Data:       progAcctData,
		Owner:      addresses.BpfLoaderUpgradeableAddr,
		Executable: true,
	}); err != nil {
		return fmt.Errorf("%w: write program account: %v", ErrLiteSVM, err)
	}
	// Mithril caches upgradeable-loader programs keyed on the canonical
	// programdata address (bpf_loader.go); evict both keys so re-installing
	// under the same program id executes the new ELF instead of stale
	// compiled bytes.
	s.stubDb.RemoveProgramFromCache(programData)
	s.stubDb.RemoveProgramFromCache(programID)
	return nil
}

// SetDefaultPrograms installs the standard program set the Rust litesvm
// crate ships: SPL Token (p-token), Token-2022, Memo v1+v3, Associated
// Token Account, and the core-BPF Address Lookup Table and Stake programs.
func (s *LiteSVM) SetDefaultPrograms() error {
	for _, p := range elf.Defaults {
		var err error
		switch p.Loader {
		case elf.LoaderV1:
			err = s.addProgramV1V2(p.ProgramID, p.Elf, addresses.BpfLoaderDeprecatedAddr)
		case elf.LoaderV2:
			err = s.addProgramV1V2(p.ProgramID, p.Elf, addresses.BpfLoader2Addr)
		case elf.LoaderV3:
			err = s.addProgramV3(p.ProgramID, p.Elf)
		default:
			err = fmt.Errorf("%w: unknown loader for %s", ErrLiteSVM, p.Name)
		}
		if err != nil {
			return fmt.Errorf("install %s: %w", p.Name, err)
		}
	}
	return nil
}

// WithNativeMints seeds the wrapped-SOL mint (and the Token-2022 native
// mint) when the corresponding token program account is installed.
func (s *LiteSVM) WithNativeMints() error {
	pairs := []struct {
		program solana.PublicKey
		mint    solana.PublicKey
	}{
		{elf.Defaults[0].ProgramID, elf.NativeMintSPL},
		{elf.Defaults[1].ProgramID, elf.NativeMintToken2022},
	}
	for _, p := range pairs {
		if prog, err := s.mem.GetAccountWithoutLock(p.program); err != nil || prog == nil {
			continue
		}
		acct := &accounts.Account{
			Key:      p.mint,
			Lamports: elf.NativeMintLamports,
			Data:     elf.NativeMintData(),
			Owner:    [32]byte(p.program),
		}
		pk := [32]byte(p.mint)
		if err := s.mem.SetAccount(&pk, acct); err != nil {
			return fmt.Errorf("%w: seed native mint: %v", ErrLiteSVM, err)
		}
	}
	return nil
}
