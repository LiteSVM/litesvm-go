package litesvm

import (
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/addresses"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"

	"github.com/LiteSVM/litesvm-go/internal/gates"
)

const (
	lamportsPerSol = 1_000_000_000
	// defaultAirdropLamports matches Rust litesvm's default airdrop pool
	// (1_000_000 * LAMPORTS_PER_SOL).
	defaultAirdropLamports = 1_000_000 * lamportsPerSol
	// defaultLamportsPerSignature matches Agave/litesvm defaults.
	defaultLamportsPerSignature = 5000
	// maxRecentBlockhashes mirrors Agave's blockhash queue depth.
	maxRecentBlockhashes = 150
	// defaultHistoryCapacity mirrors litesvm's default transaction history:
	// TransactionHistory::new() is IndexMap::with_capacity(32) and
	// add_new_transaction evicts the oldest entry once len reaches that
	// capacity (litesvm-0.13.0 src/history.rs).
	defaultHistoryCapacity = 32
	// featureAccountSize is bincode Feature{activated_at: Some(slot)}:
	// 1-byte Option tag + 8-byte slot (solana-feature-gate-interface
	// Feature::size_of()).
	featureAccountSize = 9
)

// latestEvictedSentinel initializes SlotCtx.LatestEvictedBlockhash: mithril's
// age check treats a tx blockhash equal to that field as valid (Agave's
// 151-entry blockhash-queue quirk), but this engine's queue never overflows,
// so nothing is ever genuinely evicted. A fixed non-zero constant keeps the
// zero-value hole closed: an all-zero (unset) recent blockhash must fail age
// validation with BlockhashNotFound, exactly like Rust litesvm.
var latestEvictedSentinel = sha256.Sum256([]byte("mithrilsvm:latest-evicted-blockhash-sentinel"))

// ErrLiteSVM is the sentinel wrapped by every error this package
// returns. It is the same value as the root package's ErrLiteSVM, and its
// message is "litesvm" so wrapped errors read "litesvm: ...".
var ErrLiteSVM = errors.New("litesvm")

// LiteSVM is a pure-Go, in-process Solana VM for testing. Instances are
// fully isolated from each other (per-instance accounts, sysvar cache,
// feature set, blockhash queue) and are NOT safe for concurrent use by
// multiple goroutines.
type LiteSVM struct {
	mem   accounts.MemAccounts
	cache *sealevel.SysvarCacheData
	feats *features.Features
	// stubDb is a disk-free accountsdb used only for the program cache;
	// it opens no database and touches no disk.
	stubDb *accountsdb.AccountsDb

	slot        uint64
	blockhashes []solana.Hash // newest first, capped at maxRecentBlockhashes
	feeGov      *sealevel.FeeRateGovernor
	// rbhLamportsPerSignature is the fee-calculator value written into the
	// RecentBlockhashes sysvar entries. Rust litesvm's genesis entry carries
	// Fees::default().lamports_per_signature == 0 (lib.rs set_sysvars) and
	// expire_blockhash rewrites it with fee_structure.lamports_per_signature
	// == 5000. Actual fee charging is independent of this value.
	rbhLamportsPerSignature uint64

	sigverify      bool
	blockhashCheck bool
	logBytesLimit  *uint64
	// computeBudget, when non-nil, is the custom per-instance compute
	// budget installed by SetComputeBudget; see computebudget.go.
	computeBudget *ComputeBudget

	historyCap   int
	history      map[solana.Signature]*TxOutcome
	historyOrder []solana.Signature

	airdropKey solana.PrivateKey
}

// New creates a fresh LiteSVM with default settings, mirroring Rust
// LiteSVM::new(): default sysvars (including the deprecated Fees sysvar and
// stake-config accounts), builtin programs, precompile accounts, the default
// SPL/core-BPF program set (with_default_programs: SPL Token as p-token,
// Token-2022, Memo v1+v3, ATA, Address Lookup Table, Stake), on-chain
// feature-gate accounts for every mainnet-active feature, an airdrop pool of
// 1M SOL, and the deterministic genesis blockhash sha256("genesis").
// Native mints stay opt-in via WithNativeMints, as in Rust.
func New() (*LiteSVM, error) {
	s := &LiteSVM{
		mem:            accounts.NewMemAccounts(),
		cache:          &sealevel.SysvarCacheData{},
		stubDb:         &accountsdb.AccountsDb{},
		feeGov:         defaultFeeRateGovernor(),
		sigverify:      true,
		blockhashCheck: true,
		historyCap:     defaultHistoryCapacity,
		history:        make(map[solana.Signature]*TxOutcome),
	}
	s.stubDb.InitCaches()

	s.feats = defaultFeatures()

	// Rust: latest_blockhash = create_blockhash(b"genesis") (lib.rs:476).
	// Seed the queue before installSysvarDefaults so SlotHashes can carry
	// the (slot 0, genesis blockhash) entry, then sync the RecentBlockhashes
	// sysvar once Rent is installed.
	s.blockhashes = []solana.Hash{createBlockhash([]byte("genesis"))}

	if err := s.installSysvarDefaults(); err != nil {
		return nil, err
	}
	if err := s.syncRecentBlockhashes(); err != nil {
		return nil, err
	}
	s.installBuiltins()
	s.installPrecompiles()
	if err := s.installFeatureAccounts(); err != nil {
		return nil, err
	}
	if err := s.SetDefaultPrograms(); err != nil {
		return nil, err
	}

	airdropKey, err := solana.NewRandomPrivateKey()
	if err != nil {
		return nil, fmt.Errorf("%w: airdrop key: %v", ErrLiteSVM, err)
	}
	s.airdropKey = airdropKey
	s.setPlainAccount(airdropKey.PublicKey(), defaultAirdropLamports)

	return s, nil
}

// Close is a no-op: the engine holds no resources that need releasing.
// It exists so `defer svm.Close()` remains valid.
func (s *LiteSVM) Close() {}

// Version reports the engine backing this package.
func Version() string {
	return "litesvm-go (mithril svm, pure go)"
}

func defaultFeeRateGovernor() *sealevel.FeeRateGovernor {
	return &sealevel.FeeRateGovernor{
		TargetLamportsPerSignature: defaultLamportsPerSignature / 2,
		TargetSignaturesPerSlot:    20000,
		MinLamportsPerSignature:    defaultLamportsPerSignature,
		MaxLamportsPerSignature:    defaultLamportsPerSignature,
		BurnPercent:                50,
		LamportsPerSignature:       defaultLamportsPerSignature,
		PrevLamportsPerSignature:   defaultLamportsPerSignature,
	}
}

// defaultFeatures enables exactly the mainnet-active feature gates from the
// gates catalog, matching Rust litesvm's mainnet snapshot. Mithril keys its
// Features map on {Name, Address} with its own gate names, so activation is
// mapped by address onto mithril's gate list; catalog gates mithril does
// not implement are ignored, and mithril-only gates absent from the catalog
// (e.g. its TestFeature placeholders) stay inactive.
//
// FormalizeLoadedTransactionDataSize, which the loader requires for
// empty-account defaulting, is mainnet-active in the catalog and therefore
// enabled by this mapping.
func defaultFeatures() *features.Features {
	mainnetActive := make(map[[32]byte]bool, len(gates.All))
	for _, g := range gates.All {
		if g.MainnetActive {
			mainnetActive[[32]byte(g.Address)] = true
		}
	}
	f := features.NewFeaturesDefault()
	for _, gate := range features.AllFeatureGates {
		if mainnetActive[gate.Address] {
			f.EnableFeature(gate, 0)
		}
	}
	return f
}

// createBlockhash mirrors Rust litesvm's create_blockhash (utils/mod.rs):
// a single sha256 over the seed bytes. New() seeds with "genesis" and
// ExpireBlockhash chains from the previous hash, so blockhash sequences are
// identical across engines and across runs.
func createBlockhash(seed []byte) solana.Hash {
	return solana.Hash(sha256.Sum256(seed))
}

// installFeatureAccounts writes the on-chain feature-gate account for every
// active feature, mirroring Rust's with_feature_accounts (lib.rs:672-687):
// owner Feature111.., bincode Feature{activated_at: Some(0)} (9 bytes),
// rent-exempt lamports. Only active features get accounts; inactive gates
// have none. Rust's set_feature_set never rewrites these accounts, so
// SetFeatureSet leaves them untouched too.
func (s *LiteSVM) installFeatureAccounts() error {
	lamports, err := s.MinimumBalanceForRentExemption(featureAccountSize)
	if err != nil {
		return err
	}
	for _, g := range gates.All {
		if !g.MainnetActive {
			continue
		}
		data := make([]byte, featureAccountSize)
		data[0] = 1 // Some(activated_at); slot 0 is the 8 zero bytes after it
		pk := [32]byte(g.Address)
		acct := &accounts.Account{
			Key:      g.Address,
			Lamports: lamports,
			Data:     data,
			Owner:    addresses.FeatureAddr,
		}
		if err := s.mem.SetAccount(&pk, acct); err != nil {
			return fmt.Errorf("%w: write feature account: %v", ErrLiteSVM, err)
		}
	}
	return nil
}

// setPlainAccount installs a system-owned account holding lamports.
func (s *LiteSVM) setPlainAccount(pubkey solana.PublicKey, lamports uint64) {
	acct := &accounts.Account{
		Key:      pubkey,
		Lamports: lamports,
		Owner:    addresses.SystemProgramAddr,
	}
	pk := [32]byte(pubkey)
	_ = s.mem.SetAccount(&pk, acct)
}

// installBuiltins creates the native-loader-owned builtin program accounts
// that mithril's dispatch table serves natively. Stake, Config, and Address
// Lookup Table are core-BPF programs and are installed from vendored ELFs
// by SetDefaultPrograms instead; precompile accounts are installed by
// installPrecompiles.
func (s *LiteSVM) installBuiltins() {
	s.installNativeAccounts([]nativeAccount{
		{addresses.SystemProgramAddr, "system_program"},
		{addresses.VoteProgramAddr, "vote_program"},
		{addresses.ComputeBudgetProgramAddr, "compute_budget_program"},
		{addresses.BpfLoaderDeprecatedAddr, "solana_bpf_loader_deprecated_program"},
		{addresses.BpfLoader2Addr, "solana_bpf_loader_program"},
		{addresses.BpfLoaderUpgradeableAddr, "solana_bpf_loader_upgradeable_program"},
		{addresses.ZkElgamalProofProgramAddr, "zk_elgamal_proof_program"},
	})
}

// installPrecompiles creates the precompile accounts: signature
// verification is native code keyed off the account's presence.
func (s *LiteSVM) installPrecompiles() {
	s.installNativeAccounts([]nativeAccount{
		{addresses.Secp256kPrecompileAddr, "secp256k1_program"},
		{addresses.Ed25519PrecompileAddr, "ed25519_program"},
		{addresses.Secp256r1PrecompileAddr, "secp256r1_program"},
	})
}

type nativeAccount struct {
	addr [32]byte
	name string
}

func (s *LiteSVM) installNativeAccounts(natives []nativeAccount) {
	for _, b := range natives {
		addr := b.addr
		acct := &accounts.Account{
			Key:        solana.PublicKey(addr),
			Lamports:   1,
			Data:       []byte(b.name),
			Owner:      accounts.NativeLoaderAddr,
			Executable: true,
		}
		_ = s.mem.SetAccount(&addr, acct)
	}
}

// SetSysvars re-initializes the built-in sysvars to their defaults. The
// recent-blockhash queue is left untouched.
func (s *LiteSVM) SetSysvars() error {
	if err := s.installSysvarDefaults(); err != nil {
		return err
	}
	// The Clock sysvar was reset; keep the internal slot counter coherent.
	s.slot = 0
	return nil
}

// SetBuiltins (re-)installs the default set of built-in program accounts.
func (s *LiteSVM) SetBuiltins() error {
	s.installBuiltins()
	return nil
}

// SetPrecompiles (re-)installs the standard precompile accounts.
func (s *LiteSVM) SetPrecompiles() error {
	s.installPrecompiles()
	return nil
}

// registerBlockhash pushes a new latest blockhash into the queue and
// refreshes the RecentBlockhashes sysvar (cache + account).
func (s *LiteSVM) registerBlockhash(h solana.Hash) error {
	s.blockhashes = append([]solana.Hash{h}, s.blockhashes...)
	if len(s.blockhashes) > maxRecentBlockhashes {
		s.blockhashes = s.blockhashes[:maxRecentBlockhashes]
	}
	return s.syncRecentBlockhashes()
}

func (s *LiteSVM) syncRecentBlockhashes() error {
	rbh := make(sealevel.SysvarRecentBlockhashes, 0, len(s.blockhashes))
	for _, h := range s.blockhashes {
		rbh = append(rbh, sealevel.RecentBlockHashesEntry{
			Blockhash:     h,
			FeeCalculator: sealevel.FeeCalculator{LamportsPerSignature: s.rbhLamportsPerSignature},
		})
	}
	s.cache.RecentBlockHashes.Sysvar = &rbh
	return s.writeSysvarAccount(sealevel.SysvarRecentBlockHashesAddr, rbh.MustMarshal())
}

// LatestBlockhash returns the most recent registered blockhash.
func (s *LiteSVM) LatestBlockhash() (solana.Hash, error) {
	if len(s.blockhashes) == 0 {
		return solana.Hash{}, fmt.Errorf("%w: no blockhash registered", ErrLiteSVM)
	}
	return s.blockhashes[0], nil
}

// ExpireBlockhash invalidates all current blockhashes and installs a fresh
// latest blockhash, mirroring litesvm's expire_blockhash: the new hash is
// create_blockhash(previous hash bytes) and the rewritten RecentBlockhashes
// entry carries fee_structure.lamports_per_signature (5000).
func (s *LiteSVM) ExpireBlockhash() error {
	prev, err := s.LatestBlockhash()
	if err != nil {
		return err
	}
	s.blockhashes = nil
	s.rbhLamportsPerSignature = s.feeGov.LamportsPerSignature
	return s.registerBlockhash(createBlockhash(prev[:]))
}

// WarpToSlot advances the bank to the given slot: it updates the Clock
// sysvar's Slot field (cache + account) and the internal slot counter.
func (s *LiteSVM) WarpToSlot(slot uint64) error {
	clock := sealevel.SysvarClock{}
	if s.cache.Clock.Sysvar != nil {
		clock = *s.cache.Clock.Sysvar
	}
	clock.Slot = slot
	s.slot = slot
	s.cache.Clock.Sysvar = &clock
	return s.writeSysvarAccount(sealevel.SysvarClockAddr, clock.MustMarshal())
}

// Airdrop credits lamports to pubkey by executing a real transfer
// transaction from the internal airdrop account, mirroring litesvm.
func (s *LiteSVM) Airdrop(pubkey solana.PublicKey, lamports uint64) error {
	blockhash, err := s.LatestBlockhash()
	if err != nil {
		return err
	}
	ix := system.NewTransferInstruction(lamports, s.airdropKey.PublicKey(), pubkey).Build()
	tx, err := solana.NewTransaction(
		[]solana.Instruction{ix},
		blockhash,
		solana.TransactionPayer(s.airdropKey.PublicKey()),
	)
	if err != nil {
		return fmt.Errorf("%w: airdrop build: %v", ErrLiteSVM, err)
	}
	if _, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(s.airdropKey.PublicKey()) {
			return &s.airdropKey
		}
		return nil
	}); err != nil {
		return fmt.Errorf("%w: airdrop sign: %v", ErrLiteSVM, err)
	}
	outcome, err := s.execute(tx, false)
	if err != nil {
		return err
	}
	if !outcome.IsOk() {
		return fmt.Errorf("%w: airdrop failed: %s", ErrLiteSVM, outcome.Error())
	}
	return nil
}

// Balance returns the lamport balance of pubkey. The bool reports whether
// the account exists.
func (s *LiteSVM) Balance(pubkey solana.PublicKey) (uint64, bool, error) {
	acct, err := s.mem.GetAccountWithoutLock(pubkey)
	if err != nil || acct == nil {
		return 0, false, nil
	}
	return acct.Lamports, true, nil
}

// MinimumBalanceForRentExemption computes the rent-exempt minimum for an
// account of the given data length using the current Rent sysvar.
func (s *LiteSVM) MinimumBalanceForRentExemption(dataLen int) (uint64, error) {
	if s.cache.Rent.Sysvar == nil {
		return 0, fmt.Errorf("%w: rent sysvar not installed", ErrLiteSVM)
	}
	if dataLen < 0 {
		return 0, fmt.Errorf("%w: negative data length", ErrLiteSVM)
	}
	return s.cache.Rent.Sysvar.MinimumBalance(uint64(dataLen)), nil
}

// SetSigverify toggles signature verification for submitted transactions.
func (s *LiteSVM) SetSigverify(enabled bool) error {
	s.sigverify = enabled
	return nil
}

// Sigverify reports whether signature verification is enabled.
func (s *LiteSVM) Sigverify() (bool, error) {
	return s.sigverify, nil
}

// SetBlockhashCheck toggles transaction blockhash-age validation.
func (s *LiteSVM) SetBlockhashCheck(enabled bool) error {
	s.blockhashCheck = enabled
	return nil
}

// SetTransactionHistory sets the transaction history capacity; 0 disables
// history and with it duplicate-transaction detection.
func (s *LiteSVM) SetTransactionHistory(capacity int) error {
	if capacity < 0 {
		return fmt.Errorf("%w: negative history capacity", ErrLiteSVM)
	}
	s.historyCap = capacity
	if capacity == 0 {
		s.history = make(map[solana.Signature]*TxOutcome)
		s.historyOrder = nil
	}
	s.trimHistory()
	return nil
}

// SetLogBytesLimit caps the log bytes recorded per transaction. A negative
// limit removes the cap.
func (s *LiteSVM) SetLogBytesLimit(limit int) error {
	if limit < 0 {
		s.logBytesLimit = nil
		return nil
	}
	l := uint64(limit)
	s.logBytesLimit = &l
	return nil
}

// SetLamports resets the airdrop pool balance.
func (s *LiteSVM) SetLamports(lamports uint64) error {
	s.setPlainAccount(s.airdropKey.PublicKey(), lamports)
	return nil
}

func (s *LiteSVM) trimHistory() {
	for s.historyCap > 0 && len(s.historyOrder) > s.historyCap {
		oldest := s.historyOrder[0]
		s.historyOrder = s.historyOrder[1:]
		delete(s.history, oldest)
	}
}

// GetTransaction returns the recorded outcome for signature, or nil.
// Mirroring Rust litesvm, both successful and executed-but-failed (fee
// charged) transactions are recorded; pre-execution rejections are not.
func (s *LiteSVM) GetTransaction(signature solana.Signature) *TxOutcome {
	return s.history[signature]
}
