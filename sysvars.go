package litesvm

import (
	"encoding/binary"
	"fmt"
	"math"

	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/sysvar"
	"github.com/sonicfromnewyoke/mithril/pkg/accounts"
	"github.com/sonicfromnewyoke/mithril/pkg/addresses"
	"github.com/sonicfromnewyoke/mithril/pkg/sealevel"
)

// installSysvarDefaults seeds the default sysvars into both the account map
// (bincode account data, so programs reading sysvar accounts see them) and
// the per-instance sysvar cache (what the runtime consults), mirroring
// litesvm's set_sysvars.
func (s *LiteSVM) installSysvarDefaults() error {
	clock := sealevel.SysvarClock{}
	s.cache.Clock.Sysvar = &clock
	if err := s.writeSysvarAccount(sealevel.SysvarClockAddr, clock.MustMarshal()); err != nil {
		return err
	}

	// Rust's mainnet feature set activates deprecate_rent_exemption_threshold,
	// so set_sysvars installs {lamports_per_byte_year: 6960 (DEFAULT_LAMPORTS
	// _PER_BYTE), exemption_threshold: 1.0} (lib.rs:585-597) instead of the
	// classic 3480/2.0. minimum_balance results are identical.
	rent := sealevel.NewDefaultRentSysvar()
	rent.LamportsPerUint8Year = 6960
	rent.ExemptionThreshold = 1.0
	s.cache.Rent.Sysvar = &rent
	if err := s.writeSysvarAccount(sealevel.SysvarRentAddr, rent.MustMarshal()); err != nil {
		return err
	}

	// Deprecated Fees sysvar: Rust installs Fees::default(), whose
	// FeeCalculator carries lamports_per_signature 0 (lib.rs:573-575).
	// Actual fee charging comes from the FeeRateGovernor, not this account.
	fees := sealevel.SysvarFees{}
	s.cache.Fees.Sysvar = &fees
	feesData := make([]byte, sealevel.SysvarFeesStructLen)
	if err := s.writeSysvarAccount(sealevel.SysvarFeesAddr, feesData); err != nil {
		return err
	}

	// Agave EpochSchedule::default(): 432_000 slots per epoch, no warmup.
	epochSchedule := sealevel.SysvarEpochSchedule{
		SlotsPerEpoch:            432_000,
		LeaderScheduleSlotOffset: 432_000,
		Warmup:                   false,
		FirstNormalEpoch:         0,
		FirstNormalSlot:          0,
	}
	s.cache.EpochSchedule.Sysvar = &epochSchedule
	epochScheduleData, err := (&sysvar.EpochSchedule{
		SlotsPerEpoch:            epochSchedule.SlotsPerEpoch,
		LeaderScheduleSlotOffset: epochSchedule.LeaderScheduleSlotOffset,
		Warmup:                   epochSchedule.Warmup,
		FirstNormalEpoch:         epochSchedule.FirstNormalEpoch,
		FirstNormalSlot:          epochSchedule.FirstNormalSlot,
	}).MarshalBinary()
	if err != nil {
		return fmt.Errorf("%w: marshal epoch schedule: %v", ErrLiteSVM, err)
	}
	if err := s.writeSysvarAccount(sealevel.SysvarEpochScheduleAddr, epochScheduleData); err != nil {
		return err
	}

	// Rust seeds SlotHashes with a single (clock.slot, latest blockhash)
	// entry (lib.rs:598-601); the Clock was just reset above, so the slot is
	// 0. New() seeds the blockhash queue before calling this, so the latest
	// hash is available even at construction time.
	slotHashes := sealevel.SysvarSlotHashes{}
	if len(s.blockhashes) > 0 {
		slotHashes = sealevel.SysvarSlotHashes{{Slot: clock.Slot, Hash: [32]byte(s.blockhashes[0])}}
	}
	s.cache.SlotHashes.Sysvar = &slotHashes
	if err := s.writeSysvarAccount(sealevel.SysvarSlotHashesAddr, slotHashes.MustMarshal()); err != nil {
		return err
	}

	stakeHistory := sealevel.SysvarStakeHistory{}
	s.cache.StakeHistory.Sysvar = &stakeHistory
	if err := s.writeSysvarAccount(sealevel.SysvarStakeHistoryAddr, mustEncode(stakeHistory.MustMarshalWithEncoder)); err != nil {
		return err
	}

	lastRestartSlot := sealevel.SysvarLastRestartSlot{}
	s.cache.LastRestartSlot.Sysvar = &lastRestartSlot
	lastRestartSlotData, err := (&sysvar.LastRestartSlot{}).MarshalBinary()
	if err != nil {
		return fmt.Errorf("%w: marshal last restart slot: %v", ErrLiteSVM, err)
	}
	if err := s.writeSysvarAccount(sealevel.SysvarLastRestartSlotAddr, lastRestartSlotData); err != nil {
		return err
	}

	epochRewards := sealevel.SysvarEpochRewards{}
	s.cache.EpochRewards.Sysvar = &epochRewards
	if err := s.writeSysvarAccount(sealevel.SysvarEpochRewardsAddr, mustEncode(epochRewards.MustMarshalWithEncoder)); err != nil {
		return err
	}

	slotHistory := defaultSlotHistory()
	s.cache.SlotHistory.Sysvar = &slotHistory
	if err := s.writeSysvarAccount(sealevel.SysvarSlotHistoryAddr, slotHistory.MustMarshal()); err != nil {
		return err
	}

	if err := s.installStakeConfigAccount(); err != nil {
		return err
	}

	return nil
}

// installStakeConfigAccount installs the deprecated stake-config account
// under the config program, mirroring Rust set_sysvars (lib.rs:622-631):
// a ConfigKeys header (u64 key count = 0) followed by bincode-serialized
// solana_stake_interface Config::default() {warmup_cooldown_rate: 0.25,
// slash_penalty: 12}, with 1 lamport like Rust's AccountSharedData::new(1,..).
func (s *LiteSVM) installStakeConfigAccount() error {
	data := make([]byte, 8+8+1)
	binary.LittleEndian.PutUint64(data[8:16], math.Float64bits(0.25))
	data[16] = 12 // DEFAULT_SLASH_PENALTY = (5 * 255) / 100
	addr := addresses.StakeProgramConfigAddr
	acct := &accounts.Account{
		Key:      solana.PublicKey(addr),
		Lamports: 1,
		Data:     data,
		Owner:    addresses.ConfigProgramAddr,
	}
	if err := s.mem.SetAccount(&addr, acct); err != nil {
		return fmt.Errorf("%w: write stake-config account: %v", ErrLiteSVM, err)
	}
	return nil
}

// defaultSlotHistory mirrors Agave's SlotHistory::default(): a
// 1024*1024-bit bitvec (16384 u64 words) with slot 0 marked present and
// next_slot = 1.
func defaultSlotHistory() sealevel.SysvarSlotHistory {
	const blocks = sysvar.SlotHistoryMaxEntries / 64
	sh := sealevel.SysvarSlotHistory{NextSlot: 1}
	sh.Bits.Len = sysvar.SlotHistoryMaxEntries
	sh.Bits.Bits.BlocksLen = blocks
	sh.Bits.Bits.Blocks = make([]uint64, blocks)
	sh.Bits.Bits.Blocks[0] = 1 // slot 0 is present
	return sh
}

// mustEncode runs a MustMarshalWithEncoder-style method into a buffer.
func mustEncode(marshal func(*bin.Encoder)) []byte {
	var buf bytesBuffer
	enc := bin.NewBinEncoder(&buf)
	marshal(enc)
	return buf.b
}

type bytesBuffer struct{ b []byte }

func (w *bytesBuffer) Write(p []byte) (int, error) {
	w.b = append(w.b, p...)
	return len(p), nil
}

// sysvarFixedAccountSizes maps sysvar ids to Agave's fixed account data
// sizes for the vector-shaped sysvars: SlotHashes is 8 + 512*40 bytes and
// StakeHistory is 8 + 512*32 bytes regardless of how many entries are
// populated, with the tail zeroed. Programs fetch these through the generic
// sol_get_sysvar syscall with the full fixed length (e.g. solana-program's
// PodSlotHashes, used by the core-BPF address-lookup-table program), and
// mithril's SyscallGetSysvarImpl fails such reads when the backing account
// data is shorter (pkg/sealevel/syscalls_sysvar.go, offsetLenExceedsSysvar).
var sysvarFixedAccountSizes = map[[32]byte]int{
	sealevel.SysvarSlotHashesAddr:   8 + 512*40,
	sealevel.SysvarStakeHistoryAddr: 8 + 512*32,
}

// writeSysvarAccount installs data as a rent-exempt sysvar-owned account.
func (s *LiteSVM) writeSysvarAccount(addr [32]byte, data []byte) error {
	if want, ok := sysvarFixedAccountSizes[addr]; ok && len(data) < want {
		padded := make([]byte, want)
		copy(padded, data)
		data = padded
	}
	lamports := uint64(1)
	if s.cache.Rent.Sysvar != nil {
		lamports = s.cache.Rent.Sysvar.MinimumBalance(uint64(len(data)))
	}
	acct := &accounts.Account{
		Key:      solana.PublicKey(addr),
		Lamports: lamports,
		Data:     data,
		Owner:    addresses.SysvarOwnerAddr,
	}
	if err := s.mem.SetAccount(&addr, acct); err != nil {
		return fmt.Errorf("%w: write sysvar account: %v", ErrLiteSVM, err)
	}
	s.setCacheAcct(addr, acct)
	return nil
}

// setCacheAcct mirrors the sysvar account into the per-instance cache's Acct
// slot. Mithril's generic sol_get_sysvar syscall (fetchSysvarBytesForPubkey)
// falls back to these Acct fields when the sysvar account is not among the
// transaction's accounts, so leaving them nil makes programs that call
// Rent::get()/Clock::get() via sol_get_sysvar (e.g. p-token) fail with
// UnsupportedSysvar.
func (s *LiteSVM) setCacheAcct(addr [32]byte, acct *accounts.Account) {
	switch addr {
	case sealevel.SysvarClockAddr:
		s.cache.Clock.Acct = acct
	case sealevel.SysvarRentAddr:
		s.cache.Rent.Acct = acct
	case sealevel.SysvarEpochScheduleAddr:
		s.cache.EpochSchedule.Acct = acct
	case sealevel.SysvarEpochRewardsAddr:
		s.cache.EpochRewards.Acct = acct
	case sealevel.SysvarSlotHashesAddr:
		s.cache.SlotHashes.Acct = acct
	case sealevel.SysvarSlotHistoryAddr:
		s.cache.SlotHistory.Acct = acct
	case sealevel.SysvarStakeHistoryAddr:
		s.cache.StakeHistory.Acct = acct
	case sealevel.SysvarLastRestartSlotAddr:
		s.cache.LastRestartSlot.Acct = acct
	case sealevel.SysvarRecentBlockHashesAddr:
		s.cache.RecentBlockHashes.Acct = acct
	case sealevel.SysvarFeesAddr:
		s.cache.Fees.Acct = acct
	}
}

// isKnownSysvarID reports whether addr is a sysvar id whose typed cache slot
// this engine maintains (the refreshCache switch plus RecentBlockhashes and
// Fees). SetAccount keys its cache refresh on this, mirroring Rust's
// maybe_handle_sysvar_account which matches on the ADDRESS regardless of the
// account's owner.
func isKnownSysvarID(addr [32]byte) bool {
	switch addr {
	case sealevel.SysvarClockAddr,
		sealevel.SysvarRentAddr,
		sealevel.SysvarEpochScheduleAddr,
		sealevel.SysvarEpochRewardsAddr,
		sealevel.SysvarLastRestartSlotAddr,
		sealevel.SysvarSlotHashesAddr,
		sealevel.SysvarStakeHistoryAddr,
		sealevel.SysvarSlotHistoryAddr,
		sealevel.SysvarRecentBlockHashesAddr,
		sealevel.SysvarFeesAddr:
		return true
	}
	return false
}

// getSysvarData returns the raw bincode account data of a sysvar account.
func (s *LiteSVM) getSysvarData(id solana.PublicKey) ([]byte, error) {
	acct, err := s.mem.GetAccountWithoutLock(id)
	if err != nil || acct == nil {
		return nil, fmt.Errorf("%w: sysvar account %s not found", ErrLiteSVM, id)
	}
	return acct.Data, nil
}

// setSysvarData installs raw bincode account data as the sysvar identified
// by id and refreshes the per-instance cache entry, mirroring the FFI's
// generic set_sysvar.
func (s *LiteSVM) setSysvarData(id solana.PublicKey, data []byte) error {
	if err := s.refreshCache(id, data); err != nil {
		return err
	}
	return s.writeSysvarAccount([32]byte(id), data)
}

// refreshCache decodes data into the matching per-instance cache slot.
func (s *LiteSVM) refreshCache(id solana.PublicKey, data []byte) error {
	dec := bin.NewBinDecoder(data)
	var err error
	switch [32]byte(id) {
	case sealevel.SysvarClockAddr:
		v := sealevel.SysvarClock{}
		if err = v.UnmarshalWithDecoder(dec); err == nil {
			s.cache.Clock.Sysvar = &v
			s.slot = v.Slot
		}
	case sealevel.SysvarRentAddr:
		v := sealevel.SysvarRent{}
		if err = v.UnmarshalWithDecoder(dec); err == nil {
			s.cache.Rent.Sysvar = &v
		}
	case sealevel.SysvarEpochScheduleAddr:
		v := sealevel.SysvarEpochSchedule{}
		if err = v.UnmarshalWithDecoder(dec); err == nil {
			s.cache.EpochSchedule.Sysvar = &v
		}
	case sealevel.SysvarEpochRewardsAddr:
		v := sealevel.SysvarEpochRewards{}
		if err = v.UnmarshalWithDecoder(dec); err == nil {
			s.cache.EpochRewards.Sysvar = &v
		}
	case sealevel.SysvarLastRestartSlotAddr:
		v := sealevel.SysvarLastRestartSlot{}
		if err = v.UnmarshalWithDecoder(dec); err == nil {
			s.cache.LastRestartSlot.Sysvar = &v
		}
	case sealevel.SysvarSlotHashesAddr:
		v := sealevel.SysvarSlotHashes{}
		if err = v.UnmarshalWithDecoder(dec); err == nil {
			s.cache.SlotHashes.Sysvar = &v
		}
	case sealevel.SysvarStakeHistoryAddr:
		v := sealevel.SysvarStakeHistory{}
		if err = v.UnmarshalWithDecoder(dec); err == nil {
			s.cache.StakeHistory.Sysvar = &v
		}
	case sealevel.SysvarSlotHistoryAddr:
		v := sealevel.SysvarSlotHistory{}
		if err = v.UnmarshalWithDecoder(dec); err == nil {
			s.cache.SlotHistory.Sysvar = &v
		}
	case sealevel.SysvarFeesAddr:
		v := sealevel.SysvarFees{}
		if err = v.UnmarshalWithDecoder(dec); err == nil {
			s.cache.Fees.Sysvar = &v
		}
	case sealevel.SysvarRecentBlockHashesAddr:
		// The typed cache is refreshed so programs observe the written data
		// (mirroring Rust's cache refresh), but the internal blockhash queue
		// (LatestBlockhash/ExpireBlockhash) is intentionally NOT rebuilt.
		// Note the pure engine's age validation reads this sysvar queue,
		// whereas Rust keys age checks on its internal latest hash only.
		v := sealevel.SysvarRecentBlockhashes{}
		if err = v.UnmarshalWithDecoder(dec); err == nil {
			s.cache.RecentBlockHashes.Sysvar = &v
		}
	default:
		return fmt.Errorf("%w: unsupported sysvar id %s", ErrLiteSVM, id)
	}
	if err != nil {
		return fmt.Errorf("%w: decode sysvar %s: %v", ErrLiteSVM, id, err)
	}
	return nil
}

// --- typed accessors, mirroring litesvm-go's public API -------------------

func (s *LiteSVM) Clock() (*sysvar.Clock, error) {
	b, err := s.getSysvarData(sysvar.ClockID)
	if err != nil {
		return nil, err
	}
	return sysvar.DecodeClock(b)
}

func (s *LiteSVM) SetClock(c *sysvar.Clock) error {
	if c == nil {
		return fmt.Errorf("%w: SetClock: nil clock", ErrLiteSVM)
	}
	b, err := c.MarshalBinary()
	if err != nil {
		return fmt.Errorf("%w: SetClock: marshal: %v", ErrLiteSVM, err)
	}
	return s.setSysvarData(sysvar.ClockID, b)
}

func (s *LiteSVM) Rent() (*sysvar.Rent, error) {
	b, err := s.getSysvarData(sysvar.RentID)
	if err != nil {
		return nil, err
	}
	return sysvar.DecodeRent(b)
}

func (s *LiteSVM) SetRent(r *sysvar.Rent) error {
	if r == nil {
		return fmt.Errorf("%w: SetRent: nil rent", ErrLiteSVM)
	}
	b, err := r.MarshalBinary()
	if err != nil {
		return fmt.Errorf("%w: SetRent: marshal: %v", ErrLiteSVM, err)
	}
	return s.setSysvarData(sysvar.RentID, b)
}

func (s *LiteSVM) EpochSchedule() (*sysvar.EpochSchedule, error) {
	b, err := s.getSysvarData(sysvar.EpochScheduleID)
	if err != nil {
		return nil, err
	}
	return sysvar.DecodeEpochSchedule(b)
}

func (s *LiteSVM) SetEpochSchedule(e *sysvar.EpochSchedule) error {
	if e == nil {
		return fmt.Errorf("%w: SetEpochSchedule: nil epoch schedule", ErrLiteSVM)
	}
	b, err := e.MarshalBinary()
	if err != nil {
		return fmt.Errorf("%w: SetEpochSchedule: marshal: %v", ErrLiteSVM, err)
	}
	return s.setSysvarData(sysvar.EpochScheduleID, b)
}

func (s *LiteSVM) EpochRewards() (*sysvar.EpochRewards, error) {
	b, err := s.getSysvarData(sysvar.EpochRewardsID)
	if err != nil {
		return nil, err
	}
	return sysvar.DecodeEpochRewards(b)
}

func (s *LiteSVM) SetEpochRewards(e *sysvar.EpochRewards) error {
	if e == nil {
		return fmt.Errorf("%w: SetEpochRewards: nil epoch rewards", ErrLiteSVM)
	}
	b, err := e.MarshalBinary()
	if err != nil {
		return fmt.Errorf("%w: SetEpochRewards: marshal: %v", ErrLiteSVM, err)
	}
	return s.setSysvarData(sysvar.EpochRewardsID, b)
}

func (s *LiteSVM) LastRestartSlot() (*sysvar.LastRestartSlot, error) {
	b, err := s.getSysvarData(sysvar.LastRestartSlotID)
	if err != nil {
		return nil, err
	}
	return sysvar.DecodeLastRestartSlot(b)
}

func (s *LiteSVM) SetLastRestartSlot(l *sysvar.LastRestartSlot) error {
	if l == nil {
		return fmt.Errorf("%w: SetLastRestartSlot: nil last restart slot", ErrLiteSVM)
	}
	b, err := l.MarshalBinary()
	if err != nil {
		return fmt.Errorf("%w: SetLastRestartSlot: marshal: %v", ErrLiteSVM, err)
	}
	return s.setSysvarData(sysvar.LastRestartSlotID, b)
}

func (s *LiteSVM) SlotHashes() (sysvar.SlotHashes, error) {
	b, err := s.getSysvarData(sysvar.SlotHashesID)
	if err != nil {
		return nil, err
	}
	return sysvar.DecodeSlotHashes(b)
}

// SetSlotHashes replaces the SlotHashes sysvar. Entries are canonicalized to
// the layout programs expect: descending slot order, deduplicated, and capped
// at sysvar.SlotHashesMaxEntries.
func (s *LiteSVM) SetSlotHashes(items sysvar.SlotHashes) error {
	var canonical sysvar.SlotHashes
	for _, it := range items {
		canonical.Add(it.Slot, it.Hash)
	}
	b, err := canonical.MarshalBinary()
	if err != nil {
		return fmt.Errorf("%w: SetSlotHashes: marshal: %v", ErrLiteSVM, err)
	}
	return s.setSysvarData(sysvar.SlotHashesID, b)
}

func (s *LiteSVM) StakeHistory() (sysvar.StakeHistory, error) {
	b, err := s.getSysvarData(sysvar.StakeHistoryID)
	if err != nil {
		return nil, err
	}
	return sysvar.DecodeStakeHistory(b)
}

// SetStakeHistory replaces the StakeHistory sysvar. Entries are canonicalized
// to descending epoch order, deduplicated, and capped at
// sysvar.StakeHistoryMaxEntries (newest kept).
func (s *LiteSVM) SetStakeHistory(items sysvar.StakeHistory) error {
	var canonical sysvar.StakeHistory
	for _, it := range items {
		canonical.Add(it.Epoch, it.Entry)
	}
	b, err := canonical.MarshalBinary()
	if err != nil {
		return fmt.Errorf("%w: SetStakeHistory: marshal: %v", ErrLiteSVM, err)
	}
	return s.setSysvarData(sysvar.StakeHistoryID, b)
}

// SlotHistory returns the SlotHistory sysvar.
func (s *LiteSVM) SlotHistory() (*sysvar.SlotHistory, error) {
	b, err := s.getSysvarData(sysvar.SlotHistoryID)
	if err != nil {
		return nil, err
	}
	return sysvar.DecodeSlotHistory(b)
}

// SetSlotHistory replaces the SlotHistory sysvar.
func (s *LiteSVM) SetSlotHistory(sh *sysvar.SlotHistory) error {
	if sh == nil {
		return fmt.Errorf("%w: SetSlotHistory: nil history", ErrLiteSVM)
	}
	// The sysvar is a fixed-size bitvec; reject a malformed one rather than
	// install a wrong-length account that programs would misread.
	if want := sysvar.SlotHistoryMaxEntries / 64; len(sh.Bits) != want {
		return fmt.Errorf("%w: SetSlotHistory: malformed bitvec: %d blocks, want %d", ErrLiteSVM, len(sh.Bits), want)
	}
	b, err := sh.MarshalBinary()
	if err != nil {
		return fmt.Errorf("%w: SetSlotHistory: marshal: %v", ErrLiteSVM, err)
	}
	return s.setSysvarData(sysvar.SlotHistoryID, b)
}
