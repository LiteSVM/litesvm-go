package litesvm

import (
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/addresses"
	"github.com/Overclock-Validator/mithril/pkg/replay"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
)

// SendLegacyTransaction submits a bincode-encoded legacy transaction.
// Version-prefixed (v0) bytes are rejected with a decode error.
func (s *LiteSVM) SendLegacyTransaction(txBytes []byte) (*TxOutcome, error) {
	return s.sendBytes(txBytes, false, false)
}

// SendVersionedTransaction submits a bincode-encoded versioned transaction
// (legacy or v0 wire format).
func (s *LiteSVM) SendVersionedTransaction(txBytes []byte) (*TxOutcome, error) {
	return s.sendBytes(txBytes, false, true)
}

// SimulateLegacyTransaction simulates a bincode-encoded legacy transaction
// without committing state. Version-prefixed (v0) bytes are rejected with a
// decode error.
func (s *LiteSVM) SimulateLegacyTransaction(txBytes []byte) (*TxOutcome, error) {
	return s.sendBytes(txBytes, true, false)
}

// SimulateVersionedTransaction simulates a bincode-encoded versioned
// transaction (legacy or v0 wire format) without committing state.
func (s *LiteSVM) SimulateVersionedTransaction(txBytes []byte) (*TxOutcome, error) {
	return s.sendBytes(txBytes, true, true)
}

func (s *LiteSVM) sendBytes(txBytes []byte, simulate, allowVersioned bool) (*TxOutcome, error) {
	tx, err := solana.TransactionFromDecoder(bin.NewBinDecoder(txBytes))
	if err != nil {
		// The "decode Transaction" wording is load-bearing: callers match
		// on the error text.
		return nil, fmt.Errorf("%w: decode Transaction: %v", ErrLiteSVM, err)
	}
	if !allowVersioned && tx.Message.GetVersion() != solana.MessageVersionLegacy {
		return nil, fmt.Errorf("%w: decode Transaction: versioned bytes passed to a legacy entry point", ErrLiteSVM)
	}
	return s.execute(tx, simulate)
}

// execute runs a decoded transaction through mithril's pure pipeline.
//
// The pre-execution checks mirror Rust litesvm's ordering (lib.rs
// check_and_process_transaction): signature verification (sanitize), lookup
// resolution (sanitize), maybe_blockhash_check, then maybe_history_check.
// The history is consulted only when sigverify is enabled, exactly like
// maybe_history_check; recording happens unconditionally for every included
// transaction, like send_transaction.
func (s *LiteSVM) execute(tx *solana.Transaction, simulate bool) (*TxOutcome, error) {
	outcome := &TxOutcome{}
	if len(tx.Signatures) > 0 {
		outcome.signature = tx.Signatures[0]
	}

	if s.sigverify {
		if err := tx.VerifySignatures(); err != nil {
			outcome.err = `"SignatureFailure"`
			return outcome, nil
		}
	}

	if errName := s.resolveLookups(tx); errName != "" {
		outcome.err = `"` + errName + `"`
		return outcome, nil
	}

	slotCtx := s.newSlotCtx()

	if s.sigverify && s.historyCap > 0 && len(tx.Signatures) > 0 {
		if _, seen := s.history[tx.Signatures[0]]; seen {
			// Rust validates blockhash age BEFORE the history check, so a
			// duplicate whose blockhash has expired yields BlockhashNotFound,
			// not AlreadyProcessed. IsTransactionAgeValid applies the full
			// durable-nonce rules: a nonce tx is age-valid with a stale hash
			// while the nonce account still holds the tx's durable nonce.
			// Invalid-age duplicates fall through so replay's own age check
			// produces the BlockhashNotFound outcome.
			if !s.blockhashCheck || sealevel.IsTransactionAgeValid(tx, firstInstrForAgeCheck(tx), slotCtx) {
				outcome.err = `"AlreadyProcessed"`
				return outcome, nil
			}
		}
	}

	s.applyComputeBudgetOverride(tx)

	// With blockhash checking disabled, make the tx's blockhash valid for
	// the duration of the replay call only. The queue is restored BEFORE
	// commit so MaybeAdvanceNonceAccountForFailedTx sees the real queue and
	// still advances the nonce of a failed durable-nonce tx.
	restore := func() {}
	if !s.blockhashCheck {
		restore = s.injectBlockhash(tx.Message.RecentBlockhash)
	}
	out := replay.LoadAndExecuteTransaction(replay.LoadAndExecuteTransactionInput{
		SlotCtx:                 slotCtx,
		Transaction:             tx,
		IsSimulation:            simulate,
		RecordInnerInstructions: true,
		LogBytesLimit:           s.logBytesLimit,
	})
	restore()

	s.populateOutcome(outcome, tx, &out, simulate)

	if !simulate {
		if errName := s.commit(slotCtx, tx, &out); errName != "" {
			// Rust replaces the tx result when the post-failure fee
			// withdrawal fails (lib.rs:1444-1449).
			outcome.ok = false
			outcome.err = `"` + errName + `"`
		}
		// Rust records every included tx: successes always, failures
		// whenever an execution context was produced (fee charged), i.e.
		// executed-but-failed txs replay as AlreadyProcessed and are
		// visible through GetTransaction (lib.rs:1594-1608).
		if s.historyCap > 0 && len(tx.Signatures) > 0 && txIncluded(&out) {
			sig := tx.Signatures[0]
			if _, seen := s.history[sig]; !seen {
				s.historyOrder = append(s.historyOrder, sig)
			}
			s.history[sig] = outcome
			s.trimHistory()
		}
	}
	return outcome, nil
}

// txIncluded reports whether the transaction was "included" in litesvm
// terms (execution_result_if_context: a TransactionContext was produced):
// it executed successfully, or it executed and failed with an
// InstructionError / rent-state violation after fees were charged. This is
// the same condition commit() uses for the failed-tx fee debit.
func txIncluded(out *replay.LoadAndExecuteTransactionOutput) bool {
	if out.ProcessingResult.ProcessedTransaction != nil {
		return true
	}
	te := out.ProcessingResult.TransactionError
	if te == nil || out.ExecCtx == nil || out.FeeInfo == nil {
		return false
	}
	return te.ErrorType == replay.TransactionErrorInstructionError ||
		te.ErrorType == replay.TransactionErrorInsufficientFundsForRent
}

// firstInstrForAgeCheck converts the transaction's first compiled
// instruction into the sealevel.Instruction shape so that
// sealevel.IsTransactionAgeValid can apply its durable-nonce rules. An
// empty slice is returned when the message yields no usable instruction
// (no instructions, unresolved indexes); IsTransactionAgeValid then falls
// back to plain blockhash-age semantics.
func firstInstrForAgeCheck(tx *solana.Transaction) []sealevel.Instruction {
	msg := &tx.Message
	if len(msg.Instructions) == 0 {
		return nil
	}
	ci := msg.Instructions[0]
	programID, err := msg.Program(ci.ProgramIDIndex)
	if err != nil {
		return nil
	}
	metas, err := msg.AccountMetaList()
	if err != nil {
		return nil
	}
	accts := make([]sealevel.AccountMeta, 0, len(ci.Accounts))
	for _, idx := range ci.Accounts {
		if int(idx) >= len(metas) || metas[idx] == nil {
			return nil
		}
		accts = append(accts, sealevel.AccountMeta{
			Pubkey:     metas[idx].PublicKey,
			IsSigner:   metas[idx].IsSigner,
			IsWritable: metas[idx].IsWritable,
		})
	}
	return []sealevel.Instruction{{
		ProgramId: programID,
		Data:      ci.Data,
		Accounts:  accts,
	}}
}

// newSlotCtx assembles a fresh slot context over the instance's shared
// state. LoadAndExecuteTransaction treats it as read-only.
//
// The execution slot is s.slot+1, not s.slot: mithril's BPF loaders enforce
// Agave's delay-visibility rule (a program deployed at slot N is executable
// only at slots > N; pkg/sealevel/bpf_loader.go:1549,1578), but litesvm
// semantics require programs added via AddProgram/SetDefaultPrograms to be
// executable immediately - Rust litesvm forces effective_slot = current_slot
// on its program cache entries (litesvm-0.13.0 src/lib.rs add_program_internal).
// addProgramV3 records deployment slot 0, so executing at s.slot+1 makes
// installed programs visible even at the genesis slot. Programs observe the
// slot through the Clock sysvar cache (still s.slot), so the bump is
// invisible to everything except the loader visibility checks.
//
// Known divergence: a program deployed through a REAL bpf-loader deploy
// transaction at clock slot N becomes invokable at slot N here (the exec
// slot N+1 clears Agave's delay-visibility rule), while Rust litesvm
// requires warping past N first.
func (s *LiteSVM) newSlotCtx() *sealevel.SlotCtx {
	var latest solana.Hash
	if len(s.blockhashes) > 0 {
		latest = s.blockhashes[0]
	}
	execSlot := s.slot + 1
	if execSlot == 0 { // saturate on WarpToSlot(MaxUint64)
		execSlot = s.slot
	}
	return &sealevel.SlotCtx{
		Accounts:        s.mem,
		ParentAccts:     accounts.NewMemAccounts(),
		AccountsDb:      s.stubDb,
		FeeRateGovernor: s.feeGov,
		Slot:            execSlot,
		Features:        s.feats,
		SysvarCache:     s.cache,
		Blockhash:       latest,
		LastBlockhash:   latest,
		// The zero value would make an all-zero tx blockhash pass mithril's
		// age check (blockhash_nonce.go compares against this field); the
		// sentinel can never match a real blockhash, so unset blockhashes
		// fail with BlockhashNotFound like Rust.
		LatestEvictedBlockhash: latestEvictedSentinel,
		AcctMapsMu:             &sync.Mutex{},
		ModifiedAccts:          make(map[solana.PublicKey]bool),
		WritableAccts:          make(map[solana.PublicKey]bool),
	}
}

// injectBlockhash temporarily prepends h to the RecentBlockhashes cache so
// age validation passes; the returned func restores the previous queue.
//
// Known divergence: Rust with blockhash_check=false simply skips the age
// check and never mutates RecentBlockhashes, so a program (or nonce
// instruction) reading the sysvar DURING such a tx sees the fabricated
// newest entry only on this engine. Mithril's replay pipeline offers no
// skip-age-check switch, so the injection is the closest available
// mechanism; it is scoped to the replay call only (restored before commit,
// so failed-nonce advancement sees the real queue).
func (s *LiteSVM) injectBlockhash(h solana.Hash) func() {
	prev := s.cache.RecentBlockHashes.Sysvar
	if prev != nil {
		for _, e := range *prev {
			if e.Blockhash == h {
				return func() {}
			}
		}
	}
	injected := sealevel.SysvarRecentBlockhashes{{
		Blockhash:     h,
		FeeCalculator: sealevel.FeeCalculator{LamportsPerSignature: s.feeGov.LamportsPerSignature},
	}}
	if prev != nil {
		injected = append(injected, *prev...)
	}
	s.cache.RecentBlockHashes.Sysvar = &injected
	return func() { s.cache.RecentBlockHashes.Sysvar = prev }
}

// resolveLookups resolves v0 address-table lookups against the in-memory
// accounts, mirroring Rust litesvm's load_lookup_table_addresses
// (accounts_db.rs:355-383) which delegates to AddressLookupTable::lookup:
// deactivated tables fail with AddressLookupTableNotFound, and addresses
// appended in the current slot are not yet visible (capped at
// LastExtendedSlotStartIndex). Returns an Agave TransactionError variant
// name on failure.
func (s *LiteSVM) resolveLookups(tx *solana.Transaction) string {
	if !tx.Message.IsVersioned() || tx.Message.IsResolved() {
		return ""
	}
	lookups := tx.Message.GetAddressTableLookups()
	if lookups.NumLookups() == 0 {
		return ""
	}
	// Rust reads the current slot and SlotHashes from the sysvar cache.
	currentSlot := s.slot
	if s.cache.Clock.Sysvar != nil {
		currentSlot = s.cache.Clock.Sysvar.Slot
	}
	var slotHashes sealevel.SysvarSlotHashes
	if s.cache.SlotHashes.Sysvar != nil {
		slotHashes = *s.cache.SlotHashes.Sysvar
	}
	tables := make(map[solana.PublicKey]solana.PublicKeySlice)
	for _, tableID := range lookups.GetTableIDs() {
		acct, err := s.mem.GetAccountWithoutLock(tableID)
		if err != nil || acct == nil {
			return "AddressLookupTableNotFound"
		}
		if acct.Owner != addresses.AddressLookupTableAddr {
			return "InvalidAddressLookupTableOwner"
		}
		table, err := sealevel.UnmarshalAddressLookupTable(acct.Data)
		if err != nil {
			return "InvalidAddressLookupTableData"
		}
		// AddressLookupTable::lookup: a Deactivated table is "not found";
		// Activated and Deactivating tables remain usable.
		if table.Meta.Status(currentSlot, slotHashes).Status == sealevel.AddressLookupTableStatusTypeDeactivated {
			return "AddressLookupTableNotFound"
		}
		active := table.Addresses
		if table.Meta.LastExtendedSlot == currentSlot {
			n := int(table.Meta.LastExtendedSlotStartIndex)
			if n > len(active) {
				n = len(active)
			}
			active = active[:n]
		}
		tables[tableID] = active
	}
	if err := tx.Message.SetAddressTables(tables); err != nil {
		return "InvalidAddressLookupTableData"
	}
	if err := tx.Message.ResolveLookups(); err != nil {
		return "InvalidAddressLookupTableIndex"
	}
	return ""
}

// applyComputeBudgetOverride threads the custom compute budget's honored
// knobs (ComputeUnitLimit, HeapSize) into a transaction by appending
// synthetic compute-budget instructions, matching Rust litesvm's override
// semantics as closely as mithril's pure pipeline allows: replay derives
// its budget solely from the transaction's compute-budget instructions.
//
// Transactions that carry their own compute-budget instructions are left
// untouched (injecting would trip the runtime's duplicate-instruction
// check). Divergences from the Rust engine, which swaps the budget out of
// band: each injected instruction consumes the usual 150 CU, executes as a
// trailing top-level instruction (visible in logs), and versioned
// transactions with address-table lookups skip the override when the
// compute-budget program is not already a static key (appending a static
// key would shift the loaded addresses' indexes).
func (s *LiteSVM) applyComputeBudgetOverride(tx *solana.Transaction) {
	b := s.computeBudget
	if b == nil {
		return
	}
	cbProgram := solana.PublicKey(addresses.ComputeBudgetProgramAddr)
	keyIdx := -1
	for i, k := range tx.Message.AccountKeys {
		if k.Equals(cbProgram) {
			keyIdx = i
			break
		}
	}
	if keyIdx >= 0 {
		// Program ids are always static keys, so scanning the static index
		// finds every compute-budget instruction.
		for _, ci := range tx.Message.Instructions {
			if int(ci.ProgramIDIndex) == keyIdx {
				return
			}
		}
	} else {
		if tx.Message.GetAddressTableLookups().NumLookups() > 0 {
			return
		}
		tx.Message.AccountKeys = append(tx.Message.AccountKeys, cbProgram)
		keyIdx = len(tx.Message.AccountKeys) - 1
		// The appended key joins the readonly non-signer tail, leaving the
		// writability of every existing key unchanged.
		tx.Message.Header.NumReadonlyUnsignedAccounts++
	}

	limitData := make([]byte, 5)
	limitData[0] = sealevel.ComputeBudgetInstrTypeSetComputeUnitLimit
	binary.LittleEndian.PutUint32(limitData[1:], uint32(b.ComputeUnitLimit))
	tx.Message.Instructions = append(tx.Message.Instructions, solana.CompiledInstruction{
		ProgramIDIndex: uint16(keyIdx),
		Data:           limitData,
	})

	if b.HeapSize != DefaultComputeBudget().HeapSize {
		heapData := make([]byte, 5)
		heapData[0] = sealevel.ComputeBudgetInstrTypeRequestHeapFrame
		binary.LittleEndian.PutUint32(heapData[1:], b.HeapSize)
		tx.Message.Instructions = append(tx.Message.Instructions, solana.CompiledInstruction{
			ProgramIDIndex: uint16(keyIdx),
			Data:           heapData,
		})
	}
}

// commit applies a send's state changes to the instance accounts: full
// account updates on success (accounts drained to zero lamports are deleted,
// matching Rust accounts_db add_account); on executed-but-failed
// transactions the fee is debited and, mirroring mithril's impure wrapper
// (handleFailedTx), a leading AdvanceNonceAccount instruction advances the
// durable nonce so the failed transaction cannot be replayed.
//
// The returned string is empty on success; otherwise it is the Agave
// TransactionError variant name that replaces the outcome's error, mirroring
// Rust's post-failure fee withdrawal (lib.rs:1444-1449 accounts.withdraw)
// which surfaces an error instead of silently clamping the payer balance.
func (s *LiteSVM) commit(slotCtx *sealevel.SlotCtx, tx *solana.Transaction, out *replay.LoadAndExecuteTransactionOutput) string {
	if out.ExecutionResult != nil {
		for _, upd := range out.ExecutionResult.AccountUpdates {
			pk := [32]byte(upd.Pubkey)
			// collectAccountUpdates rewrites closed accounts as
			// {Key, RentEpoch: MaxUint64} with zero lamports; Rust removes
			// zero-lamport accounts on commit, so delete instead of storing
			// a tombstone.
			if upd.Account.Lamports == 0 {
				delete(s.mem.Map, pk)
				continue
			}
			acct := upd.Account
			_ = s.mem.SetAccount(&pk, &acct)
		}
		return ""
	}
	// Executed-but-failed transactions (instruction error or rent-state
	// violation) are still "processed" in Agave terms: fees are charged
	// and durable nonces advance. Pre-execution failures (blockhash, fee
	// or account validation) commit nothing.
	te := out.ProcessingResult.TransactionError
	if te == nil || out.ExecCtx == nil || out.FeeInfo == nil {
		return ""
	}
	if te.ErrorType != replay.TransactionErrorInstructionError &&
		te.ErrorType != replay.TransactionErrorInsufficientFundsForRent {
		return ""
	}
	errName := ""
	if len(tx.Message.AccountKeys) > 0 {
		payer := tx.Message.AccountKeys[0]
		acct, err := s.mem.GetAccountWithoutLock(payer)
		switch {
		case err != nil || acct == nil:
			// Rust's withdraw fails with AccountNotFound for a missing payer.
			errName = "AccountNotFound"
		case acct.Lamports < out.FeeInfo.TotalFee:
			// Rust's withdraw charges nothing and surfaces
			// InsufficientFundsForFee. (The nonce-payer minimum-balance rule
			// of accounts_db withdraw is not replicated; the fee payer was
			// already validated against pre-execution state, so this branch
			// is only reachable through exotic state edits.)
			errName = "InsufficientFundsForFee"
		default:
			// Rust's withdraw mutates the stored account in place and,
			// unlike the success-path add_account, never removes it, so a
			// payer drained to exactly zero by the fee stays present.
			acct.Lamports -= out.FeeInfo.TotalFee
			pk := [32]byte(payer)
			_ = s.mem.SetAccount(&pk, acct)
		}
	}
	// out.Instrs carries the lookup-resolved instructions; the nonce check
	// only applies to a leading system AdvanceNonceAccount instruction.
	// MaybeAdvanceNonceAccountForFailedTx writes through slotCtx into the
	// instance's account map.
	if len(out.Instrs) > 0 {
		sealevel.MaybeAdvanceNonceAccountForFailedTx(slotCtx, tx, out.Instrs[0])
	}
	return errName
}

// populateOutcome maps mithril's execution output onto the TxOutcome shape.
func (s *LiteSVM) populateOutcome(o *TxOutcome, tx *solana.Transaction, out *replay.LoadAndExecuteTransactionOutput, simulate bool) {
	if te := out.ProcessingResult.TransactionError; te != nil {
		if b, err := te.MarshalJSON(); err == nil {
			o.err = string(b)
		} else {
			o.err = te.ErrorType.String()
		}
	} else {
		o.ok = true
	}

	if out.FeeInfo != nil {
		o.fee = out.FeeInfo.TotalFee
	}

	pt := out.ProcessingResult.ProcessedTransaction
	if pt == nil || pt.Executed == nil {
		// Executed-but-failed transactions carry no ProcessedTransaction in
		// mithril's pure path, but litesvm still reports their metadata:
		// recover logs, consumed compute units, inner instructions, and
		// return data from the execution context (Rust
		// execution_result_if_context -> execute_tx_helper).
		if out.ExecCtx != nil {
			if rec, ok := out.ExecCtx.Log.(*sealevel.LogRecorder); ok && rec != nil {
				o.logs = rec.Logs
			}
			o.computeUnits = out.ExecCtx.ComputeMeter.Used()
			o.inner = innerFromGroups(len(tx.Message.Instructions), replay.AssembleInnerInstructions(out.ExecCtx))
			if pid, data := out.ExecCtx.TransactionContext.ReturnData(); len(data) > 0 {
				o.returnDataProgramID = pid
				o.returnData = data
			}
		}
		return
	}
	det := pt.Executed.ExecutionDetails
	o.computeUnits = det.ExecutedUnits
	o.logs = det.LogMessages
	if det.ReturnData != nil {
		o.returnDataProgramID = det.ReturnData.ProgramId
		o.returnData = det.ReturnData.Data
	}

	o.inner = innerFromGroups(len(tx.Message.Instructions), det.InnerInstructions)

	// Rust populates post accounts only for successful SIMULATIONS
	// (from_sim_success; send_transaction's metadata has none), and they
	// contain every writable account of the resolved message with its
	// post-execution state, touched or not (execute_tx_helper filters on
	// msg.is_writable). Untouched writables keep their pre-execution state,
	// which simulate never commits, so the live map provides it.
	if simulate && out.ExecutionResult != nil {
		updated := make(map[solana.PublicKey]*accounts.Account, len(out.ExecutionResult.AccountUpdates))
		for i := range out.ExecutionResult.AccountUpdates {
			upd := &out.ExecutionResult.AccountUpdates[i]
			updated[upd.Pubkey] = &upd.Account
		}
		seen := make(map[solana.PublicKey]bool, len(out.ExecutionResult.WritableAccounts))
		for _, pk := range out.ExecutionResult.WritableAccounts {
			if seen[pk] {
				continue
			}
			seen[pk] = true
			acct := updated[pk]
			if acct == nil {
				if a, err := s.mem.GetAccountWithoutLock(pk); err == nil && a != nil {
					acct = a
				} else {
					// Non-existent writable accounts load as default
					// (zero-lamport, system-owned) accounts.
					acct = &accounts.Account{Key: pk, Owner: addresses.SystemProgramAddr}
				}
			}
			o.postAccounts = append(o.postAccounts, PostAccount{
				Address: pk,
				Account: toSolanaAccount(acct),
			})
		}
	}
}

// innerFromGroups converts mithril's grouped inner-instruction recording
// into the solana-go response shape, one (possibly empty) list per top-level
// instruction.
func innerFromGroups(numInstrs int, groups []replay.InnerInstructionsList) InnerInstructionsList {
	inner := make(InnerInstructionsList, numInstrs)
	for _, group := range groups {
		if int(group.Index) >= len(inner) {
			continue
		}
		for _, instr := range group.Instructions {
			accts := make([]uint16, len(instr.Accounts))
			for i, a := range instr.Accounts {
				accts[i] = uint16(a)
			}
			inner[group.Index] = append(inner[group.Index], InnerInstruction{
				Instruction: solana.CompiledInstruction{
					ProgramIDIndex: uint16(instr.ProgramIdIndex),
					Accounts:       accts,
					Data:           instr.Data,
				},
				StackHeight: uint16(instr.StackHeight),
			})
		}
	}
	return inner
}
