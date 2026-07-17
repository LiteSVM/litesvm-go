package difftest

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Scenarios
// ---------------------------------------------------------------------------

func scenarios() []Scenario {
	return []Scenario{
		{Name: "airdrop", Run: scenarioAirdrop},
		{Name: "transfer-ok", Run: scenarioTransferOK},
		{Name: "transfer-insufficient-funds", Run: scenarioTransferInsufficientFunds},
		{Name: "failed-instruction-logs", Run: scenarioFailedInstructionLogs},
		{Name: "create-account-already-in-use", Run: scenarioCreateAccountAlreadyInUse},
		{Name: "transfer-rent-shortfall", Run: scenarioTransferRentShortfall},
		{Name: "simulate-uncommitted", Run: scenarioSimulate},
		{Name: "expired-blockhash", Run: scenarioExpiredBlockhash},
		{Name: "sigverify-toggle", Run: scenarioSigverify},
		{Name: "blockhash-check-off", Run: scenarioBlockhashCheckOff},
		{Name: "warp-clock", Run: scenarioWarpClock},
		{Name: "rent-minimums", Run: scenarioRentMinimums},
		{Name: "memo", Run: scenarioMemo},
		{Name: "duplicate-tx", Run: scenarioDuplicateTx},
		{Name: "failed-tx-replay", Run: scenarioFailedTxReplay},
		{Name: "close-account-to-zero", Run: scenarioCloseAccountToZero},
		{Name: "zero-blockhash", Run: scenarioZeroBlockhash},
		{Name: "sigverify-off-replay", Run: scenarioSigverifyOffReplay},
		{Name: "expire-then-replay", Run: scenarioExpireThenReplay},
		{Name: "postaccounts-untouched-writable", Run: scenarioPostAccountsUntouchedWritable},
		{Name: "genesis-blockhash", Run: scenarioGenesisBlockhash},
	}
}

// scenarioAirdrop: airdrop then balance readback, plus a miss on an
// untouched account.
func scenarioAirdrop(e Engine) ([]Observation, error) {
	w := seededKey("airdrop.wallet").PublicKey()
	if err := e.Airdrop(w, 5*lamportsPerSol); err != nil {
		return nil, fmt.Errorf("airdrop: %w", err)
	}
	bal, exists, err := e.Balance(w)
	if err != nil {
		return nil, err
	}
	_, missExists, err := e.Balance(seededKey("airdrop.untouched").PublicKey())
	if err != nil {
		return nil, err
	}
	return []Observation{
		obs("balance", bal),
		obs("exists", exists),
		obs("untouched.exists", missExists),
	}, nil
}

// scenarioTransferOK: a plain 1 SOL system transfer.
func scenarioTransferOK(e Engine) ([]Observation, error) {
	payer := seededKey("transfer.payer")
	recipient := seededKey("transfer.recipient").PublicKey()
	if err := e.Airdrop(payer.PublicKey(), 2*lamportsPerSol); err != nil {
		return nil, err
	}
	txBytes, err := buildTransfer(e, payer, recipient, lamportsPerSol)
	if err != nil {
		return nil, err
	}
	res, err := e.SendTx(txBytes)
	if err != nil {
		return nil, err
	}
	recipBal, _, err := e.Balance(recipient)
	if err != nil {
		return nil, err
	}
	payerBal, _, err := e.Balance(payer.PublicKey())
	if err != nil {
		return nil, err
	}
	return []Observation{
		obs("ok", res.Ok),
		obs("error", normalizeError(res.Err)),
		obs("fee", res.Fee),
		softObs("cu", res.CU),
		obs("recipient.balance", recipBal),
		obs("payer.balance", payerBal),
		obs("logs", strings.Join(res.Logs, " | ")),
	}, nil
}

// scenarioTransferInsufficientFunds: transfer more than the payer holds.
// System program fails with Custom(1); the fee is still charged.
func scenarioTransferInsufficientFunds(e Engine) ([]Observation, error) {
	payer := seededKey("insufficient.payer")
	recipient := seededKey("insufficient.recipient").PublicKey()
	if err := e.Airdrop(payer.PublicKey(), 1*lamportsPerSol); err != nil {
		return nil, err
	}
	txBytes, err := buildTransfer(e, payer, recipient, 2*lamportsPerSol)
	if err != nil {
		return nil, err
	}
	res, err := e.SendTx(txBytes)
	if err != nil {
		return nil, err
	}
	payerBal, _, err := e.Balance(payer.PublicKey())
	if err != nil {
		return nil, err
	}
	_, recipExists, err := e.Balance(recipient)
	if err != nil {
		return nil, err
	}
	return []Observation{
		obs("ok", res.Ok),
		obs("error", normalizeError(res.Err)),
		obs("fee", res.Fee),
		obs("payer.balance", payerBal),
		obs("recipient.exists", recipExists),
	}, nil
}

// scenarioFailedInstructionLogs: an executed-but-failed system transfer (more
// lamports than the payer holds) emits the Agave failure framing. The full
// log array is compared hard: the mithril fork ports the system program's
// ic_msg detail lines, so both engines emit "Transfer: insufficient lamports
// N, need M" between the invoke and failed lines byte-identically.
func scenarioFailedInstructionLogs(e Engine) ([]Observation, error) {
	payer := seededKey("failedlogs.payer")
	recipient := seededKey("failedlogs.recipient").PublicKey()
	if err := e.Airdrop(payer.PublicKey(), 1*lamportsPerSol); err != nil {
		return nil, err
	}
	txBytes, err := buildTransfer(e, payer, recipient, 2*lamportsPerSol)
	if err != nil {
		return nil, err
	}
	res, err := e.SendTx(txBytes)
	if err != nil {
		return nil, err
	}
	firstLine, lastLine := "", ""
	if n := len(res.Logs); n > 0 {
		firstLine, lastLine = res.Logs[0], res.Logs[n-1]
	}
	return []Observation{
		obs("ok", res.Ok),
		obs("error", normalizeError(res.Err)),
		obs("log.invoke.line", firstLine),
		obs("log.failed.line", lastLine),
		obs("log.failed.line.is.failed", strings.HasPrefix(lastLine,
			"Program "+solana.SystemProgramID.String()+" failed: ")),
		obs("logs", strings.Join(res.Logs, " | ")),
	}, nil
}

// scenarioCreateAccountAlreadyInUse: pre-seed the target account via
// SetAccount, then CreateAccount over it. The system program fails with
// SystemError::AccountAlreadyInUse (Custom(0)) and logs the ic_msg detail
// line "Create Account: account Address { address: X, base: None } already
// in use" (the Rust Debug rendering of Agave's Address struct); both engines
// emit it byte-identically, so the full log array is compared hard.
func scenarioCreateAccountAlreadyInUse(e Engine) ([]Observation, error) {
	payer := seededKey("createinuse.payer")
	target := seededKey("createinuse.target")
	if err := e.Airdrop(payer.PublicKey(), 2*lamportsPerSol); err != nil {
		return nil, err
	}
	if err := e.SetAccount(target.PublicKey(), &solana.Account{
		Lamports: lamportsPerSol,
		Owner:    solana.SystemProgramID,
	}); err != nil {
		return nil, err
	}
	blockhash, err := e.LatestBlockhash()
	if err != nil {
		return nil, err
	}
	ix := system.NewCreateAccountInstruction(
		lamportsPerSol, 0, solana.SystemProgramID,
		payer.PublicKey(), target.PublicKey()).Build()
	txBytes, err := buildSignedTx(e, []solana.Instruction{ix}, blockhash, payer, target)
	if err != nil {
		return nil, err
	}
	res, err := e.SendTx(txBytes)
	if err != nil {
		return nil, err
	}
	targetBal, _, err := e.Balance(target.PublicKey())
	if err != nil {
		return nil, err
	}
	return []Observation{
		obs("ok", res.Ok),
		obs("error", normalizeError(res.Err)),
		obs("fee", res.Fee),
		obs("target.balance", targetBal),
		obs("logs", strings.Join(res.Logs, " | ")),
	}, nil
}

// scenarioTransferRentShortfall: transfer a dust amount to a fresh account,
// leaving it below the rent-exempt minimum (rent-state check).
//
// Both the error variant and the account_index detail are compared hard: the
// mithril fork plumbs the offending account's transaction index through
// TransactionError.AccountIndex, so both engines report account_index 1 (the
// recipient) here.
func scenarioTransferRentShortfall(e Engine) ([]Observation, error) {
	payer := seededKey("rentshortfall.payer")
	recipient := seededKey("rentshortfall.recipient").PublicKey()
	if err := e.Airdrop(payer.PublicKey(), 1*lamportsPerSol); err != nil {
		return nil, err
	}
	txBytes, err := buildTransfer(e, payer, recipient, 100)
	if err != nil {
		return nil, err
	}
	res, err := e.SendTx(txBytes)
	if err != nil {
		return nil, err
	}
	payerBal, _, err := e.Balance(payer.PublicKey())
	if err != nil {
		return nil, err
	}
	_, recipExists, err := e.Balance(recipient)
	if err != nil {
		return nil, err
	}
	variant, detail := splitErrorVariant(normalizeError(res.Err))
	return []Observation{
		obs("ok", res.Ok),
		obs("error.variant", variant),
		obs("error.account_index", detail),
		obs("fee", res.Fee),
		obs("payer.balance", payerBal),
		obs("recipient.exists", recipExists),
	}, nil
}

// scenarioSimulate: simulation reports post state but commits nothing.
func scenarioSimulate(e Engine) ([]Observation, error) {
	payer := seededKey("simulate.payer")
	recipient := seededKey("simulate.recipient").PublicKey()
	if err := e.Airdrop(payer.PublicKey(), 2*lamportsPerSol); err != nil {
		return nil, err
	}
	txBytes, err := buildTransfer(e, payer, recipient, lamportsPerSol)
	if err != nil {
		return nil, err
	}
	res, err := e.SimulateTx(txBytes)
	if err != nil {
		return nil, err
	}
	postRecip, postFound := res.PostLamports(recipient)
	_, recipExists, err := e.Balance(recipient)
	if err != nil {
		return nil, err
	}
	payerBal, _, err := e.Balance(payer.PublicKey())
	if err != nil {
		return nil, err
	}
	return []Observation{
		obs("ok", res.Ok),
		obs("error", normalizeError(res.Err)),
		obs("post.recipient.found", postFound),
		obs("post.recipient.lamports", postRecip),
		obs("recipient.exists.after", recipExists),
		obs("payer.balance.after", payerBal),
	}, nil
}

// scenarioExpiredBlockhash: a transaction built on a blockhash that has
// since been expired is rejected without charging the payer.
func scenarioExpiredBlockhash(e Engine) ([]Observation, error) {
	payer := seededKey("expired.payer")
	recipient := seededKey("expired.recipient").PublicKey()
	if err := e.Airdrop(payer.PublicKey(), 2*lamportsPerSol); err != nil {
		return nil, err
	}
	txBytes, err := buildTransfer(e, payer, recipient, lamportsPerSol)
	if err != nil {
		return nil, err
	}
	if err := e.ExpireBlockhash(); err != nil {
		return nil, err
	}
	res, err := e.SendTx(txBytes)
	if err != nil {
		return nil, err
	}
	payerBal, _, err := e.Balance(payer.PublicKey())
	if err != nil {
		return nil, err
	}
	return []Observation{
		obs("ok", res.Ok),
		obs("error", normalizeError(res.Err)),
		obs("fee", res.Fee),
		obs("payer.balance", payerBal),
	}, nil
}

// scenarioSigverify: a corrupted signature is rejected with sigverify on and
// accepted with sigverify off. Two distinct transactions are used so the
// second send cannot collide with any history entry from the first.
func scenarioSigverify(e Engine) ([]Observation, error) {
	payer := seededKey("sigverify.payer")
	recipient1 := seededKey("sigverify.recipient1").PublicKey()
	recipient2 := seededKey("sigverify.recipient2").PublicKey()
	if err := e.Airdrop(payer.PublicKey(), 3*lamportsPerSol); err != nil {
		return nil, err
	}

	corrupt := func(txBytes []byte) []byte {
		out := append([]byte(nil), txBytes...)
		out[2] ^= 0xff // inside the first (and only) signature
		return out
	}

	tx1, err := buildTransfer(e, payer, recipient1, lamportsPerSol)
	if err != nil {
		return nil, err
	}
	resOn, err := e.SendTx(corrupt(tx1))
	if err != nil {
		return nil, err
	}
	if err := e.SetSigverify(false); err != nil {
		return nil, err
	}
	tx2, err := buildTransfer(e, payer, recipient2, lamportsPerSol)
	if err != nil {
		return nil, err
	}
	resOff, err := e.SendTx(corrupt(tx2))
	if err != nil {
		return nil, err
	}
	recip2Bal, _, err := e.Balance(recipient2)
	if err != nil {
		return nil, err
	}
	return []Observation{
		obs("on.ok", resOn.Ok),
		obs("on.error", normalizeError(resOn.Err)),
		obs("off.ok", resOff.Ok),
		obs("off.error", normalizeError(resOff.Err)),
		obs("off.fee", resOff.Fee),
		obs("off.recipient.balance", recip2Bal),
	}, nil
}

// scenarioBlockhashCheckOff: with the blockhash check disabled, a
// transaction carrying an expired blockhash still executes.
func scenarioBlockhashCheckOff(e Engine) ([]Observation, error) {
	payer := seededKey("nochk.payer")
	recipient := seededKey("nochk.recipient").PublicKey()
	if err := e.Airdrop(payer.PublicKey(), 2*lamportsPerSol); err != nil {
		return nil, err
	}
	txBytes, err := buildTransfer(e, payer, recipient, lamportsPerSol)
	if err != nil {
		return nil, err
	}
	if err := e.ExpireBlockhash(); err != nil {
		return nil, err
	}
	if err := e.SetBlockhashCheck(false); err != nil {
		return nil, err
	}
	res, err := e.SendTx(txBytes)
	if err != nil {
		return nil, err
	}
	recipBal, _, err := e.Balance(recipient)
	if err != nil {
		return nil, err
	}
	return []Observation{
		obs("ok", res.Ok),
		obs("error", normalizeError(res.Err)),
		obs("fee", res.Fee),
		obs("recipient.balance", recipBal),
	}, nil
}

// scenarioWarpClock: warp to a slot and read the Clock back; then set the
// unix timestamp via SetClock and read both fields back.
func scenarioWarpClock(e Engine) ([]Observation, error) {
	if err := e.WarpToSlot(1234); err != nil {
		return nil, err
	}
	clock, err := e.Clock()
	if err != nil {
		return nil, err
	}
	slotAfterWarp := clock.Slot

	clock.UnixTimestamp = 1_700_000_000
	if err := e.SetClock(clock); err != nil {
		return nil, err
	}
	clock2, err := e.Clock()
	if err != nil {
		return nil, err
	}
	return []Observation{
		obs("slot.after.warp", slotAfterWarp),
		obs("slot.after.setclock", clock2.Slot),
		obs("unixtimestamp.after.setclock", clock2.UnixTimestamp),
	}, nil
}

// scenarioRentMinimums: rent-exempt minimums for the standard data sizes
// (empty account, SPL token account, SPL mint).
func scenarioRentMinimums(e Engine) ([]Observation, error) {
	out := make([]Observation, 0, 3)
	for _, n := range []int{0, 165, 82} {
		min, err := e.MinimumBalanceForRentExemption(n)
		if err != nil {
			return nil, err
		}
		out = append(out, obs(fmt.Sprintf("min.%dbytes", n), min))
	}
	return out, nil
}

// scenarioMemo: end-to-end BPF execution through the vendored SPL Memo v3
// program. Also probes that the default program set is aligned.
func scenarioMemo(e Engine) ([]Observation, error) {
	if err := e.SetDefaultPrograms(); err != nil {
		return nil, err
	}
	memoAcct := e.GetAccount(memoV3ProgramID)
	memoInstalled := memoAcct != nil && memoAcct.Executable
	memoOwner := ""
	if memoAcct != nil {
		memoOwner = memoAcct.Owner.String()
	}

	payer := seededKey("memo.payer")
	if err := e.Airdrop(payer.PublicKey(), 2*lamportsPerSol); err != nil {
		return nil, err
	}
	const memoText = "hello differential memo"
	txBytes, err := buildMemo(e, payer, memoText)
	if err != nil {
		return nil, err
	}
	res, err := e.SendTx(txBytes)
	if err != nil {
		return nil, err
	}
	joined := strings.Join(res.Logs, " | ")
	return []Observation{
		obs("program.installed", memoInstalled),
		obs("program.owner", memoOwner),
		obs("ok", res.Ok),
		obs("error", normalizeError(res.Err)),
		obs("fee", res.Fee),
		obs("log.contains.memo", strings.Contains(joined, memoText)),
		softObs("cu", res.CU),
		obs("logs", joined),
	}, nil
}

// scenarioDuplicateTx: resubmitting the identical transaction bytes is
// rejected as AlreadyProcessed.
func scenarioDuplicateTx(e Engine) ([]Observation, error) {
	payer := seededKey("dup.payer")
	recipient := seededKey("dup.recipient").PublicKey()
	if err := e.Airdrop(payer.PublicKey(), 3*lamportsPerSol); err != nil {
		return nil, err
	}
	txBytes, err := buildTransfer(e, payer, recipient, lamportsPerSol)
	if err != nil {
		return nil, err
	}
	first, err := e.SendTx(txBytes)
	if err != nil {
		return nil, err
	}
	second, err := e.SendTx(txBytes)
	if err != nil {
		return nil, err
	}
	recipBal, _, err := e.Balance(recipient)
	if err != nil {
		return nil, err
	}
	return []Observation{
		obs("first.ok", first.Ok),
		obs("second.ok", second.Ok),
		obs("second.error", normalizeError(second.Err)),
		obs("recipient.balance", recipBal),
	}, nil
}

// scenarioFailedTxReplay: an executed-but-failed transaction charges its fee
// exactly once, is recorded in history (GetTransaction non-nil), and
// resubmitting the identical bytes returns AlreadyProcessed.
func scenarioFailedTxReplay(e Engine) ([]Observation, error) {
	payer := seededKey("failedreplay.payer")
	recipient := seededKey("failedreplay.recipient").PublicKey()
	if err := e.Airdrop(payer.PublicKey(), 1*lamportsPerSol); err != nil {
		return nil, err
	}
	// More than the payer holds: executes and fails with SystemError(1).
	txBytes, err := buildTransfer(e, payer, recipient, 2*lamportsPerSol)
	if err != nil {
		return nil, err
	}
	first, err := e.SendTx(txBytes)
	if err != nil {
		return nil, err
	}
	second, err := e.SendTx(txBytes)
	if err != nil {
		return nil, err
	}
	payerBal, _, err := e.Balance(payer.PublicKey())
	if err != nil {
		return nil, err
	}
	return []Observation{
		obs("first.ok", first.Ok),
		obs("first.error", normalizeError(first.Err)),
		obs("first.fee", first.Fee),
		obs("first.recorded", e.GetTransaction(first.Sig)),
		obs("second.ok", second.Ok),
		obs("second.error", normalizeError(second.Err)),
		obs("payer.balance", payerBal), // fee charged exactly once
	}, nil
}

// scenarioCloseAccountToZero: draining an account to exactly zero lamports
// deletes it (GetAccount nil, Balance exists=false).
func scenarioCloseAccountToZero(e Engine) ([]Observation, error) {
	payer := seededKey("closezero.payer")
	recipient := seededKey("closezero.recipient").PublicKey()
	if err := e.Airdrop(payer.PublicKey(), 1*lamportsPerSol); err != nil {
		return nil, err
	}
	txBytes, err := buildTransfer(e, payer, recipient, lamportsPerSol-5000)
	if err != nil {
		return nil, err
	}
	res, err := e.SendTx(txBytes)
	if err != nil {
		return nil, err
	}
	_, payerExists, err := e.Balance(payer.PublicKey())
	if err != nil {
		return nil, err
	}
	recipBal, _, err := e.Balance(recipient)
	if err != nil {
		return nil, err
	}
	return []Observation{
		obs("ok", res.Ok),
		obs("error", normalizeError(res.Err)),
		obs("payer.exists", payerExists),
		obs("payer.getaccount.nil", e.GetAccount(payer.PublicKey()) == nil),
		obs("recipient.balance", recipBal),
	}, nil
}

// scenarioZeroBlockhash: a transaction carrying the all-zero recent
// blockhash is rejected with BlockhashNotFound and charges nothing.
func scenarioZeroBlockhash(e Engine) ([]Observation, error) {
	payer := seededKey("zerohash.payer")
	recipient := seededKey("zerohash.recipient").PublicKey()
	if err := e.Airdrop(payer.PublicKey(), 2*lamportsPerSol); err != nil {
		return nil, err
	}
	txBytes, err := buildTransferAt(e, payer, recipient, lamportsPerSol, solana.Hash{})
	if err != nil {
		return nil, err
	}
	res, err := e.SendTx(txBytes)
	if err != nil {
		return nil, err
	}
	payerBal, _, err := e.Balance(payer.PublicKey())
	if err != nil {
		return nil, err
	}
	return []Observation{
		obs("ok", res.Ok),
		obs("error", normalizeError(res.Err)),
		obs("fee", res.Fee),
		obs("payer.balance", payerBal),
	}, nil
}

// scenarioSigverifyOffReplay: with sigverify disabled the duplicate check is
// skipped, so identical bytes re-execute and the transfer lands twice.
func scenarioSigverifyOffReplay(e Engine) ([]Observation, error) {
	payer := seededKey("svoffreplay.payer")
	recipient := seededKey("svoffreplay.recipient").PublicKey()
	if err := e.Airdrop(payer.PublicKey(), 3*lamportsPerSol); err != nil {
		return nil, err
	}
	if err := e.SetSigverify(false); err != nil {
		return nil, err
	}
	txBytes, err := buildTransfer(e, payer, recipient, lamportsPerSol)
	if err != nil {
		return nil, err
	}
	first, err := e.SendTx(txBytes)
	if err != nil {
		return nil, err
	}
	second, err := e.SendTx(txBytes)
	if err != nil {
		return nil, err
	}
	recipBal, _, err := e.Balance(recipient)
	if err != nil {
		return nil, err
	}
	return []Observation{
		obs("first.ok", first.Ok),
		obs("second.ok", second.Ok),
		obs("second.error", normalizeError(second.Err)),
		obs("recipient.balance", recipBal),
		obs("recorded", e.GetTransaction(second.Sig)),
	}, nil
}

// scenarioExpireThenReplay: resubmitting an already-processed transaction
// after ExpireBlockhash yields BlockhashNotFound (age check runs before the
// history check), not AlreadyProcessed.
func scenarioExpireThenReplay(e Engine) ([]Observation, error) {
	payer := seededKey("expirereplay.payer")
	recipient := seededKey("expirereplay.recipient").PublicKey()
	if err := e.Airdrop(payer.PublicKey(), 2*lamportsPerSol); err != nil {
		return nil, err
	}
	txBytes, err := buildTransfer(e, payer, recipient, lamportsPerSol)
	if err != nil {
		return nil, err
	}
	first, err := e.SendTx(txBytes)
	if err != nil {
		return nil, err
	}
	if err := e.ExpireBlockhash(); err != nil {
		return nil, err
	}
	replayed, err := e.SendTx(txBytes)
	if err != nil {
		return nil, err
	}
	recipBal, _, err := e.Balance(recipient)
	if err != nil {
		return nil, err
	}
	return []Observation{
		obs("first.ok", first.Ok),
		obs("replay.ok", replayed.Ok),
		obs("replay.error", normalizeError(replayed.Err)),
		obs("recipient.balance", recipBal),
	}, nil
}

// scenarioPostAccountsUntouchedWritable: successful simulations report every
// writable account of the message (including writable-but-untouched ones)
// with post-execution state; sends carry no post accounts at all.
func scenarioPostAccountsUntouchedWritable(e Engine) ([]Observation, error) {
	payer := seededKey("postacct.payer")
	recipient := seededKey("postacct.recipient").PublicKey()
	extra := seededKey("postacct.extra").PublicKey()
	if err := e.Airdrop(payer.PublicKey(), 2*lamportsPerSol); err != nil {
		return nil, err
	}
	if err := e.Airdrop(extra, 1*lamportsPerSol); err != nil {
		return nil, err
	}
	const amount = uint64(lamportsPerSol / 2)
	build := func() ([]byte, error) {
		blockhash, err := e.LatestBlockhash()
		if err != nil {
			return nil, err
		}
		ix := rawTransferIx(payer.PublicKey(), recipient, amount,
			solana.NewAccountMeta(extra, true, false)) // writable, untouched
		return buildSignedTx(e, []solana.Instruction{ix}, blockhash, payer)
	}
	txBytes, err := build()
	if err != nil {
		return nil, err
	}
	sim, err := e.SimulateTx(txBytes)
	if err != nil {
		return nil, err
	}
	payerPost, payerFound := sim.PostLamports(payer.PublicKey())
	recipPost, recipFound := sim.PostLamports(recipient)
	extraPost, extraFound := sim.PostLamports(extra)
	sent, err := e.SendTx(txBytes)
	if err != nil {
		return nil, err
	}
	return []Observation{
		obs("sim.ok", sim.Ok),
		obs("sim.post.payer.found", payerFound),
		obs("sim.post.payer.lamports", payerPost),
		obs("sim.post.recipient.found", recipFound),
		obs("sim.post.recipient.lamports", recipPost),
		obs("sim.post.extra.found", extraFound),
		obs("sim.post.extra.lamports", extraPost),
		obs("send.ok", sent.Ok),
		obs("send.post.count", len(sent.Post)),
	}, nil
}

// scenarioGenesisBlockhash: both engines derive the genesis blockhash as
// sha256("genesis") and chain expirations as sha256(previous hash), so the
// hashes are byte-identical across engines.
func scenarioGenesisBlockhash(e Engine) ([]Observation, error) {
	genesis, err := e.LatestBlockhash()
	if err != nil {
		return nil, err
	}
	if err := e.ExpireBlockhash(); err != nil {
		return nil, err
	}
	expired, err := e.LatestBlockhash()
	if err != nil {
		return nil, err
	}
	return []Observation{
		obs("genesis", genesis.String()),
		obs("after.expire", expired.String()),
	}, nil
}

// ---------------------------------------------------------------------------
// Runner
// ---------------------------------------------------------------------------

type row struct {
	scenario    string
	observation string
	root        string
	pure        string
	soft        bool
}

func (r row) match() bool { return r.root == r.pure }

func (r row) status() string {
	switch {
	case r.match():
		return "MATCH"
	case r.soft:
		return "DRIFT(soft)"
	default:
		return "DRIFT(HARD)"
	}
}

// TestDifferential runs every scenario on both engine views, compares the
// observations pairwise, logs a drift table, and fails only on hard
// drifts. DRIFT.md is a frozen historical record and is not rewritten.
func TestDifferential(t *testing.T) {
	var rows []row

	for _, sc := range scenarios() {
		rootE, err := newRootEngine()
		require.NoError(t, err, "scenario %s: new root engine", sc.Name)
		pureE, err := newPureEngine()
		require.NoError(t, err, "scenario %s: new pure engine", sc.Name)

		obsR, errR := sc.Run(rootE)
		obsP, errP := sc.Run(pureE)
		rootE.Close()
		pureE.Close()
		require.NoError(t, errR, "scenario %s failed on root engine", sc.Name)
		require.NoError(t, errP, "scenario %s failed on pure engine", sc.Name)

		require.Equal(t, len(obsR), len(obsP),
			"scenario %s: observation count mismatch (harness bug)", sc.Name)
		for i := range obsR {
			require.Equal(t, obsR[i].Name, obsP[i].Name,
				"scenario %s: observation order mismatch (harness bug)", sc.Name)
			rows = append(rows, row{
				scenario:    sc.Name,
				observation: obsR[i].Name,
				root:        obsR[i].Value,
				pure:        obsP[i].Value,
				soft:        obsR[i].Soft || obsP[i].Soft,
			})
		}
	}

	table := renderTable(rows)
	t.Log("\n" + table)

	for _, r := range rows {
		if !r.match() && !r.soft {
			t.Errorf("hard drift: %s | %s | root=%q pure=%q",
				r.scenario, r.observation, r.root, r.pure)
		}
	}
}

// renderTable renders the drift table as fixed-width ASCII.
func renderTable(rows []row) string {
	headers := []string{"scenario", "observation", "root", "pure", "status"}
	w := make([]int, len(headers))
	for i, h := range headers {
		w[i] = len(h)
	}
	cells := make([][]string, 0, len(rows))
	for _, r := range rows {
		c := []string{r.scenario, r.observation, clip(r.root, 48), clip(r.pure, 48), r.status()}
		for i, v := range c {
			if len(v) > w[i] {
				w[i] = len(v)
			}
		}
		cells = append(cells, c)
	}
	var b strings.Builder
	line := func(c []string) {
		for i, v := range c {
			fmt.Fprintf(&b, "| %-*s ", w[i], v)
		}
		b.WriteString("|\n")
	}
	line(headers)
	for i := range headers {
		fmt.Fprintf(&b, "|%s", strings.Repeat("-", w[i]+2))
	}
	b.WriteString("|\n")
	for _, c := range cells {
		line(c)
	}
	return b.String()
}

func clip(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// TestNormalizeError pins the error normalization on known renderings from
// both engines.
func TestNormalizeError(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		// Rust Debug shapes (as rendered by Rust litesvm)
		{"BlockhashNotFound", "BlockhashNotFound"},
		{"AlreadyProcessed", "AlreadyProcessed"},
		{"SignatureFailure", "SignatureFailure"},
		{"InstructionError(0, Custom(1))", "InstructionError(0, Custom(1))"},
		{"InstructionError(0,Custom(1))", "InstructionError(0, Custom(1))"},
		{"InstructionError(2, ProgramFailedToComplete)", "InstructionError(2, ProgramFailedToComplete)"},
		{"InsufficientFundsForRent { account_index: 1 }", "InsufficientFundsForRent(account_index=1)"},
		{"DuplicateInstruction(2)", "DuplicateInstruction(2)"},
		{`InstructionError(0, BorshIoError("failed to fill buffer"))`, `InstructionError(0, BorshIoError("failed to fill buffer"))`},
		// Agave wire-format JSON shapes (as emitted by this engine)
		{`"BlockhashNotFound"`, "BlockhashNotFound"},
		{`"AlreadyProcessed"`, "AlreadyProcessed"},
		{`"SignatureFailure"`, "SignatureFailure"},
		{`{"InstructionError":[0,{"Custom":1}]}`, "InstructionError(0, Custom(1))"},
		{`{"InstructionError":[2,"ProgramFailedToComplete"]}`, "InstructionError(2, ProgramFailedToComplete)"},
		{`{"InsufficientFundsForRent":{"account_index":1}}`, "InsufficientFundsForRent(account_index=1)"},
		{`{"DuplicateInstruction":2}`, "DuplicateInstruction(2)"},
		{`{"InstructionError":[0,{"BorshIoError":"failed to fill buffer"}]}`, `InstructionError(0, BorshIoError("failed to fill buffer"))`},
		// mithril system-program sentinels fold onto Agave Custom codes
		{`{"InstructionError":[0,"SystemProgErrResultWithNegativeLamports"]}`, "InstructionError(0, Custom(1))"},
		{`{"InstructionError":[1,"SystemProgErrAccountAlreadyInUse"]}`, "InstructionError(1, Custom(0))"},
	}
	for _, c := range cases {
		if got := normalizeError(c.in); got != c.want {
			t.Errorf("normalizeError(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestBothEnginesShipDefaultProgramsFromNew pins the default-program parity:
// both New() constructors install the default SPL/core-BPF program set (Rust
// LiteSVM::new runs with_default_programs; mithrilsvm.New mirrors it), and
// SetDefaultPrograms stays an idempotent re-install on both.
func TestBothEnginesShipDefaultProgramsFromNew(t *testing.T) {
	for _, mk := range []func() (Engine, error){newRootEngine, newPureEngine} {
		e, err := mk()
		require.NoError(t, err)
		acct := e.GetAccount(memoV3ProgramID)
		require.NotNil(t, acct, "%s: memo v3 program account missing after New()", e.Name())
		require.True(t, acct.Executable, "%s: memo v3 program account not executable", e.Name())
		require.NoError(t, e.SetDefaultPrograms(), "%s: SetDefaultPrograms must stay idempotent", e.Name())
		require.NotNil(t, e.GetAccount(memoV3ProgramID))
		e.Close()
	}
}
