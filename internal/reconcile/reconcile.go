// Package reconcile checks this service's books against the ledger.
//
// Every cycle it compares what the order table says happened with what Stellar
// and the Linq backend say happened, and writes down each result in
// reconciliation_items. It never acts on what it finds. A mismatch is a note
// for a person, never a trigger: an engine that "fixed" a balance it had
// misread would move money on the strength of a bug. That is enforced by what
// it is given, not by care. Ledger and Payouts can only read, and the engine
// changes no order.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Rinku-Labs/linq-stellar/internal/money"
	"github.com/Rinku-Labs/linq-stellar/internal/payout"
	"github.com/Rinku-Labs/linq-stellar/internal/stellar"
	"github.com/Rinku-Labs/linq-stellar/internal/store"
	"gorm.io/gorm"
)

// Ledger is what the engine reads from Stellar. *stellar.Client satisfies it;
// tests give it a fake. There is no way to submit anything through it.
type Ledger interface {
	AccountState(address string) (stellar.AccountState, error)
	LedgerTransaction(hash string) (stellar.LedgerTx, error)
	SponsoredAccounts(cursor string, limit int) (stellar.SponsoredPage, error)
}

// Payouts is what the engine reads from the payout provider: the Linq backend
// in production, the fake in test mode.
type Payouts interface {
	Status(reference string) (payout.Result, error)
}

// The checks, one name per question asked. The numbers are the ones in the
// SD-5 ticket.
const (
	// 1. An open deposit account holds what its order expects.
	CheckOpenAccount = "open_account"
	// 2. A settled, refunded or reclaimed order's account has been merged away.
	CheckAccountClosed = "account_closed"
	// 3. The recorded deposit, sweep and refund transactions are on the ledger,
	// succeeded, and moved the recorded amount to the right place.
	CheckDepositTx = "deposit_tx"
	CheckSweepTx   = "sweep_tx"
	CheckRefundTx  = "refund_tx"
	// 4. A sweep's USDC reached the treasury.
	CheckSweepToTreasury = "sweep_to_treasury"
	// 5. Every account the sponsor pays reserves for has an order or a pool
	// row behind it.
	CheckSponsoredAccount = "sponsored_account"
	// 6. The Linq backend's record of the NGN payout agrees with ours, under
	// every reference the order's payout attempts sent.
	CheckPayout = "payout"
)

// Defaults for a deployment that sets nothing.
const (
	DefaultInterval = 15 * time.Minute
	// DefaultHorizonCalls keeps a cycle well inside Horizon's public limit of
	// 3,600 requests an hour per address, alongside the scanner and streams:
	// four cycles an hour at 200 is 800.
	DefaultHorizonCalls = 200
	// DefaultPayoutCalls bounds how hard one cycle leans on the Linq backend.
	DefaultPayoutCalls = 50
)

// sponsoredPage is how many accounts one sponsor search returns, Horizon's
// maximum.
const sponsoredPage = 200

// sponsoredCursorID is where the sponsor search keeps its place between
// cycles, in the same table as the stream cursors.
const sponsoredCursorID = "reconcile:sponsored_accounts"

// beforeSecondLook runs between the first and second look at accounts the
// sponsor search could not place. Tests use it to write an order in between;
// it does nothing otherwise.
var beforeSecondLook = func() {}

// pageSize is how many orders are read from the database at a time.
const pageSize = 200

// finishedColumns are all the finished-order checks read. Seeds and bank
// details are left in the table.
var finishedColumns = []string{
	"id", "created_at", "status", "financial_outcome", "reserves_reclaimed",
	"deposit_address", "deposit_tx_hash", "deposit_from", "sweep_tx_hash",
	"refund_tx_hash", "refund_address", "amount_usdc", "amount_ngn",
	"payout_reference", "payout_provider",
}

// Engine runs reconciliation cycles.
type Engine struct {
	DB     *gorm.DB
	Ledger Ledger
	// Payouts may be nil, in which case check 6 does not run.
	Payouts  Payouts
	Treasury string

	// HorizonCalls and PayoutCalls are each cycle's budget. Whatever a cycle
	// cannot reach waits for the next one, so a large backlog is spread over
	// several cycles instead of tripping a rate limit.
	HorizonCalls int
	PayoutCalls  int
	// CheckSponsored turns on check 5. It is off by default: if another
	// service opens accounts with the same sponsor, every one of those would be
	// reported as having no order behind it.
	CheckSponsored bool

	Log *slog.Logger
	Now func() time.Time

	// openAfter is the last open order whose account was checked. The next
	// cycle carries on after it, so open accounts take turns when there are
	// more than a cycle's share of the budget can reach. It is not saved: a
	// restarted worker starts again from the first.
	openAfter string
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}

func (e *Engine) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

// openStates are the states in which an order's deposit account should exist
// and hold a known amount: none yet while awaiting a deposit, the deposit
// after one. A sweep or refund in progress is left out, because its
// transaction may land between reading the order and reading the account.
var openStates = []string{
	store.StateAwaitingDeposit,
	store.StateDepositDetected,
	store.StatePayoutQueued,
	store.StatePayoutProcessing,
	store.StateDisbursed,
	store.StateSweepQueued,
	store.StateRefundQueued,
}

// finalOnceResolved are the checks whose answer the ledger fixes for good: a
// transaction, once on the ledger or not, does not change. A resolved mismatch
// on one of these is not checked again. Every other check keeps running after
// a resolution, because its answer can change: a payout that was "still in
// progress" when someone explained it can pay later, and an account can be
// merged after all.
var finalOnceResolved = []string{CheckDepositTx, CheckSweepTx, CheckRefundTx, CheckSweepToTreasury}

// paidStates are the states in which the merchant has been paid.
var paidStates = []string{
	store.StateDisbursed,
	store.StateSweepQueued,
	store.StateSweepProcessing,
	store.StateSettledInTreasury,
}

// run is one cycle in progress.
type run struct {
	e     *Engine
	ctx   context.Context
	cycle store.ReconciliationCycle

	horizonLeft, payoutsLeft int
	// limited is set once Horizon refuses for being over its rate limit, and
	// stops every Horizon check for the rest of the cycle.
	limited error
	// deferred counts checks left for a later cycle by the budget.
	deferred int
	// explained counts mismatches not recorded again because a person has
	// already resolved that exact answer.
	explained int
	orders    map[string]bool
	// open is every open order, loaded once a cycle, and openSeen those whose
	// account this cycle has checked.
	open     []store.Order
	openSeen map[string]bool
}

// RunCycle runs one reconciliation cycle and returns its record.
//
// The cycle's row is written first, items are written as they are found, and
// the counts are written last even when the cycle stops early, so a cycle that
// fails partway still shows what it checked and why it stopped.
func (e *Engine) RunCycle(ctx context.Context) (result store.ReconciliationCycle, err error) {
	started := e.now()
	cycle, err := store.StartCycle(e.DB, started)
	if err != nil {
		return cycle, fmt.Errorf("reconcile: start cycle: %w", err)
	}
	r := &run{
		e: e, ctx: ctx, cycle: cycle,
		horizonLeft: e.HorizonCalls, payoutsLeft: e.PayoutCalls,
		orders: map[string]bool{}, openSeen: map[string]bool{},
	}
	defer func() {
		if p := recover(); p != nil {
			r.fail(fmt.Errorf("panic: %v", p))
		}
		r.finish(started)
		result = r.cycle
	}()

	// Open accounts get half the Horizon budget first, so a long list of them
	// cannot starve the finished orders, and whatever the cycle leaves over at
	// the end.
	half := (r.horizonLeft + 1) / 2
	steps := []func() error{
		func() error { return r.openAccounts(half) },
		r.finishedOrders,
		r.sponsoredAccounts,
		func() error { return r.openAccounts(len(r.open)) },
	}
	for _, step := range steps {
		if ctx.Err() != nil {
			r.fail(errors.New("stopped early: the worker is shutting down"))
			break
		}
		if err := step(); err != nil {
			r.fail(err)
			break
		}
	}
	r.deferred += len(r.open) - len(r.openSeen)
	if r.limited != nil && r.cycle.Error == "" {
		r.fail(fmt.Errorf("stopped Horizon checks early: %v", r.limited))
	}
	return r.cycle, nil
}

func (r *run) fail(err error) {
	if r.cycle.Error == "" {
		r.cycle.Error = err.Error()
	}
}

func (r *run) finish(started time.Time) {
	r.cycle.OrdersChecked = len(r.orders)
	finished := r.e.now()
	r.cycle.FinishedAt = &finished
	log := r.e.log()
	if err := store.FinishCycle(r.e.DB, r.cycle, finished); err != nil {
		log.Error("could not record the end of a reconciliation cycle", "cycle", r.cycle.ID, "error", err)
	}
	fields := []any{
		"cycle", r.cycle.ID,
		"orders", r.cycle.OrdersChecked,
		"accounts", r.cycle.AccountsChecked,
		"matched", r.cycle.Matched,
		"mismatches", r.cycle.Mismatches,
		"deferred", r.deferred,
		"explained", r.explained,
		"elapsed_ms", r.e.now().Sub(started).Milliseconds(),
	}
	if r.cycle.Error != "" {
		log.Error("reconciliation cycle stopped early", append(fields, "error", r.cycle.Error)...)
		return
	}
	log.Info("reconciliation cycle finished", fields...)
}

// spendHorizon takes one Horizon call from the budget, or reports there is
// none left.
func (r *run) spendHorizon() bool {
	if r.limited != nil || r.horizonLeft <= 0 {
		r.deferred++
		return false
	}
	r.horizonLeft--
	return true
}

// spendPayouts takes n calls to the payout provider from the budget, or none
// if fewer are left: an order's payout is checked whole or not at all.
func (r *run) spendPayouts(n int) bool {
	if r.payoutsLeft < n {
		r.deferred++
		return false
	}
	r.payoutsLeft -= n
	return true
}

func (r *run) outOfBudget() bool {
	horizonDone := r.limited != nil || r.horizonLeft <= 0
	payoutsDone := r.e.Payouts == nil || r.payoutsLeft <= 0
	return horizonDone && payoutsDone
}

// record writes one item and counts it. A mismatch whose exact answer a person
// has already resolved is not written again; a different answer is.
func (r *run) record(item store.ReconciliationItem) error {
	item.CycleID = r.cycle.ID
	if item.Result == store.ReconcileMismatch {
		explained, err := store.ExplainedAlready(r.e.DB, item)
		if err != nil {
			return fmt.Errorf("look up resolutions of %s for %q: %w", item.CheckName, item.OrderID, err)
		}
		if explained {
			if item.OrderID != "" {
				r.orders[item.OrderID] = true
			}
			r.explained++
			return nil
		}
	}
	if err := store.AddReconciliationItem(r.e.DB, &item); err != nil {
		return fmt.Errorf("record %s for %q: %w", item.CheckName, item.OrderID, err)
	}
	if item.OrderID != "" {
		r.orders[item.OrderID] = true
	}
	switch item.Result {
	case store.ReconcileOK:
		r.cycle.Matched++
	case store.ReconcileMismatch:
		r.cycle.Mismatches++
		r.e.log().Warn("reconciliation mismatch", "cycle", r.cycle.ID, "order", item.OrderID,
			"check", item.CheckName, "account", item.Account, "tx", item.TxHash,
			"expected", item.Expected, "actual", item.Actual)
	case store.ReconcileError:
		r.e.log().Warn("reconciliation check could not be made", "cycle", r.cycle.ID,
			"order", item.OrderID, "check", item.CheckName, "actual", item.Actual)
	}
	return nil
}

func (r *run) ok(item store.ReconciliationItem, actual string) error {
	item.Result, item.Actual = store.ReconcileOK, actual
	return r.record(item)
}

func (r *run) mismatch(item store.ReconciliationItem, actual string) error {
	item.Result, item.Actual = store.ReconcileMismatch, actual
	return r.record(item)
}

// unanswered records a check that could not be made. After a rate-limit
// refusal it also stops every Horizon check left in the cycle.
func (r *run) unanswered(item store.ReconciliationItem, err error) error {
	if errors.Is(err, stellar.ErrRateLimited) {
		r.limited = err
	}
	item.Result, item.Actual = store.ReconcileError, "could not check: "+err.Error()
	return r.record(item)
}

// reload reads an order again, to tell a real mismatch from one that only
// looks like one because the order moved while it was being checked.
func (r *run) reload(id string) (store.Order, error) {
	var o store.Order
	err := r.e.DB.Select("id", "status").First(&o, "id = ?", id).Error
	return o, err
}

// --- 1. Open deposit accounts -------------------------------------------------

// openAccounts checks up to limit open deposit accounts, carrying on after the
// last one checked and wrapping round to the first, so a list longer than the
// budget reaches is covered over several cycles instead of the same ones every
// time. An account already checked this cycle is not checked again.
func (r *run) openAccounts(limit int) error {
	if r.open == nil {
		open := []store.Order{}
		err := r.e.DB.Select("id", "status", "deposit_address", "amount_usdc").
			Where("status IN ? AND deposit_address <> ''", openStates).
			Find(&open).Error
		if err != nil {
			return fmt.Errorf("load open orders: %w", err)
		}
		// Sorted here rather than by the database, whose collation need not
		// order the IDs the way the cursor compares them.
		sort.Slice(open, func(i, j int) bool { return open[i].ID < open[j].ID })
		r.open = open
	}
	start := sort.Search(len(r.open), func(i int) bool { return r.open[i].ID > r.e.openAfter })
	checked := 0
	for k := 0; k < len(r.open) && checked < limit; k++ {
		o := &r.open[(start+k)%len(r.open)]
		if r.openSeen[o.ID] {
			continue
		}
		if r.ctx.Err() != nil || r.limited != nil || r.horizonLeft <= 0 {
			return nil
		}
		r.horizonLeft--
		r.openSeen[o.ID] = true
		r.e.openAfter = o.ID
		checked++
		if err := r.checkOpenAccount(o); err != nil {
			return err
		}
	}
	return nil
}

func (r *run) checkOpenAccount(o *store.Order) error {
	item := store.ReconciliationItem{OrderID: o.ID, CheckName: CheckOpenAccount, Account: o.DepositAddress}
	awaiting := o.Status == store.StateAwaitingDeposit
	if awaiting {
		item.Expected = "account exists and holds no USDC yet (awaiting a deposit)"
	} else {
		item.Expected = fmt.Sprintf("account holds the deposit, %s USDC (%s)", usdc(o.AmountUSDC), o.Status)
	}
	state, err := r.e.Ledger.AccountState(o.DepositAddress)
	r.cycle.AccountsChecked++
	if err != nil {
		return r.unanswered(item, err)
	}

	var problem string
	switch {
	case !state.Exists:
		problem = "the account is not on the ledger"
	case awaiting && state.USDC > money.DustUSDC:
		problem = fmt.Sprintf("holds %s USDC, but the order is not marked as deposited", usdc(state.USDC))
	case !awaiting && math.Abs(state.USDC-o.AmountUSDC) > money.DustUSDC:
		problem = fmt.Sprintf("holds %s USDC", usdc(state.USDC))
	}
	if problem == "" {
		return r.ok(item, fmt.Sprintf("holds %s USDC", usdc(state.USDC)))
	}
	// A deposit seen a second before the detector claims it, or a sweep that
	// lands just after the order was read, is the order moving, not the books
	// being wrong. Only a mismatch that survives a second look is recorded;
	// an order that moved is checked in its new state next cycle.
	now, err := r.reload(o.ID)
	if err != nil {
		return fmt.Errorf("reload order %s: %w", o.ID, err)
	}
	if now.Status != o.Status {
		return nil
	}
	return r.mismatch(item, problem)
}

// --- 2, 3, 4 and 6: finished work ---------------------------------------------

// finishedOrders runs the checks on an order past waiting for a deposit: the
// transactions it recorded, whether its account was closed, and the payout.
// Newest orders first, so a demo order is checked in the next cycle whatever
// backlog sits behind it. A check that has passed is not run again, nor is a
// resolved transaction check (finalOnceResolved).
func (r *run) finishedOrders() error {
	var afterCreated time.Time
	afterID := ""
	for {
		if r.ctx.Err() != nil || r.outOfBudget() {
			return nil
		}
		// Every order past waiting for a deposit. Reading them is a database
		// cost only; the budget applies to what is asked of Horizon and the
		// payout provider.
		q := r.e.DB.Where("status NOT IN ?", []string{store.StateInitiated, store.StateAwaitingDeposit})
		if afterID != "" {
			q = q.Where("created_at < ? OR (created_at = ? AND id < ?)", afterCreated, afterCreated, afterID)
		}
		var page []store.Order
		if err := q.Select(finishedColumns).Order("created_at DESC, id DESC").Limit(pageSize).Find(&page).Error; err != nil {
			return fmt.Errorf("load finished orders: %w", err)
		}
		if len(page) == 0 {
			return nil
		}
		ids := make([]string, len(page))
		for i, o := range page {
			ids[i] = o.ID
		}
		settled, err := store.SettledChecks(r.e.DB, ids, finalOnceResolved)
		if err != nil {
			return fmt.Errorf("load settled checks: %w", err)
		}
		for i := range page {
			if r.ctx.Err() != nil {
				return nil
			}
			if err := r.checkFinished(&page[i], settled[page[i].ID]); err != nil {
				return err
			}
		}
		last := page[len(page)-1]
		afterCreated, afterID = last.CreatedAt, last.ID
		if len(page) < pageSize {
			return nil
		}
	}
}

func (r *run) checkFinished(o *store.Order, settled map[string]bool) error {
	due := func(names ...string) bool {
		for _, n := range names {
			if !settled[n] {
				return true
			}
		}
		return false
	}

	if o.DepositTxHash != "" && due(CheckDepositTx) && r.spendHorizon() {
		if err := r.checkDeposit(o); err != nil {
			return err
		}
	}
	// A settled or refunded order with no transaction on record is the books
	// contradicting themselves, and needs no ledger call to say so.
	if o.SweepTxHash == "" && o.Status == store.StateSettledInTreasury && due(CheckSweepTx) {
		if err := r.mismatch(store.ReconciliationItem{OrderID: o.ID, CheckName: CheckSweepTx, Account: o.DepositAddress,
			Expected: "a recorded sweep transaction"}, "the order is settled, but no sweep transaction is recorded"); err != nil {
			return err
		}
	}
	if o.RefundTxHash == "" && o.Status == store.StateRefunded && due(CheckRefundTx) {
		if err := r.mismatch(store.ReconciliationItem{OrderID: o.ID, CheckName: CheckRefundTx, Account: o.DepositAddress,
			Expected: "a recorded refund transaction"}, "the order is refunded, but no refund transaction is recorded"); err != nil {
			return err
		}
	}
	if o.SweepTxHash != "" && due(CheckSweepTx, CheckSweepToTreasury) && r.spendHorizon() {
		if err := r.checkSweep(o, settled); err != nil {
			return err
		}
	}
	if o.RefundTxHash != "" && due(CheckRefundTx) && r.spendHorizon() {
		if err := r.checkRefund(o); err != nil {
			return err
		}
	}
	if closedState(o) && due(CheckAccountClosed) && r.spendHorizon() {
		if err := r.checkClosed(o); err != nil {
			return err
		}
	}
	if r.e.Payouts != nil && payoutDue(o) && due(CheckPayout) {
		if err := r.checkPayout(o); err != nil {
			return err
		}
	}
	return nil
}

// closedState says whether an order's deposit account should be gone: merged
// by its sweep or refund, or by the reclaim of an order that expired unpaid.
// An expired order is only due once reclaimed, since until then its account is
// supposed to exist.
func closedState(o *store.Order) bool {
	if o.DepositAddress == "" {
		return false
	}
	switch o.Status {
	case store.StateSettledInTreasury, store.StateRefunded:
		return true
	case store.StateExpired:
		return o.ReservesReclaimed
	}
	return false
}

// payoutDue says whether the Linq backend should have a settled answer about
// this order's payout: paid, or refunded after the payout failed.
func payoutDue(o *store.Order) bool {
	if o.FinancialOutcome == "disbursed" {
		for _, s := range paidStates {
			if o.Status == s {
				return true
			}
		}
	}
	return o.FinancialOutcome == "refunded" && o.Status == store.StateRefunded
}

// usdcInto adds up the USDC a transaction paid into an account.
func usdcInto(tx stellar.LedgerTx, to string) float64 {
	total := 0.0
	for _, m := range tx.Movements {
		if m.Kind == "payment" && m.USDC && m.To == to {
			total += m.Amount
		}
	}
	return total
}

// usdcOutOf adds up the USDC a transaction paid out of an account, by
// destination.
func usdcOutOf(tx stellar.LedgerTx, from string) map[string]float64 {
	out := map[string]float64{}
	for _, m := range tx.Movements {
		if m.Kind == "payment" && m.USDC && m.From == from {
			out[m.To] += m.Amount
		}
	}
	return out
}

// txProblem is what is wrong with a transaction before its amounts are looked
// at, or "".
func txProblem(tx stellar.LedgerTx) string {
	switch {
	case !tx.Found:
		return "the transaction is not on the ledger"
	case !tx.Successful:
		return "the transaction is on the ledger but failed, so it moved nothing"
	}
	return ""
}

func (r *run) checkDeposit(o *store.Order) error {
	item := store.ReconciliationItem{
		OrderID: o.ID, CheckName: CheckDepositTx, Account: o.DepositAddress, TxHash: o.DepositTxHash,
		Expected: fmt.Sprintf("a successful payment of %s USDC into the deposit account", usdc(o.AmountUSDC)),
	}
	tx, err := r.e.Ledger.LedgerTransaction(o.DepositTxHash)
	if err != nil {
		return r.unanswered(item, err)
	}
	if p := txProblem(tx); p != "" {
		return r.mismatch(item, p)
	}
	paid := usdcInto(tx, o.DepositAddress)
	if paid == 0 {
		return r.mismatch(item, "the transaction paid no USDC into the deposit account")
	}
	if math.Abs(paid-o.AmountUSDC) > money.DustUSDC {
		actual := fmt.Sprintf("the transaction paid %s USDC into the deposit account", usdc(paid))
		if paid < o.AmountUSDC {
			// The order records one deposit transaction, but its amount is
			// everything that arrived, so a payer who sent it in two parts
			// lands here. Say so, for whoever resolves it.
			actual += "; if the payer sent the deposit in more than one payment, the rest is in a transaction this order does not record"
		}
		return r.mismatch(item, actual)
	}
	return r.ok(item, fmt.Sprintf("a successful payment of %s USDC into the deposit account", usdc(paid)))
}

// checkSweep answers checks 3 (the sweep) and 4 (its USDC reached treasury)
// from one transaction.
func (r *run) checkSweep(o *store.Order, settled map[string]bool) error {
	sweep := store.ReconciliationItem{
		OrderID: o.ID, CheckName: CheckSweepTx, Account: o.DepositAddress, TxHash: o.SweepTxHash,
		Expected: fmt.Sprintf("a successful sweep of %s USDC out of the deposit account", usdc(o.AmountUSDC)),
	}
	treasury := store.ReconciliationItem{
		OrderID: o.ID, CheckName: CheckSweepToTreasury, Account: r.e.Treasury, TxHash: o.SweepTxHash,
		Expected: fmt.Sprintf("%s USDC paid to the treasury, %s", usdc(o.AmountUSDC), short(r.e.Treasury)),
	}
	tx, err := r.e.Ledger.LedgerTransaction(o.SweepTxHash)
	if err != nil {
		if !settled[CheckSweepTx] {
			if rerr := r.unanswered(sweep, err); rerr != nil {
				return rerr
			}
		}
		if !settled[CheckSweepToTreasury] {
			return r.unanswered(treasury, err)
		}
		return nil
	}

	if !settled[CheckSweepTx] {
		var err error
		out := usdcOutOf(tx, o.DepositAddress)
		total := 0.0
		for _, v := range out {
			total += v
		}
		switch p := txProblem(tx); {
		case p != "":
			err = r.mismatch(sweep, p)
		case total == 0:
			err = r.mismatch(sweep, "the transaction moved no USDC out of the deposit account")
		case math.Abs(total-o.AmountUSDC) > money.DustUSDC:
			err = r.mismatch(sweep, fmt.Sprintf("the transaction moved %s USDC out of the deposit account", usdc(total)))
		default:
			err = r.ok(sweep, fmt.Sprintf("a successful sweep of %s USDC out of the deposit account", usdc(total)))
		}
		if err != nil {
			return err
		}
	}

	if !settled[CheckSweepToTreasury] {
		if p := txProblem(tx); p != "" {
			return r.mismatch(treasury, p)
		}
		out := usdcOutOf(tx, o.DepositAddress)
		toTreasury := out[r.e.Treasury]
		if math.Abs(toTreasury-o.AmountUSDC) > money.DustUSDC {
			var elsewhere []string
			for to, v := range out {
				if to != r.e.Treasury {
					elsewhere = append(elsewhere, fmt.Sprintf("%s USDC to %s", usdc(v), short(to)))
				}
			}
			actual := fmt.Sprintf("%s USDC reached the treasury", usdc(toTreasury))
			if len(elsewhere) > 0 {
				actual += "; " + strings.Join(elsewhere, ", ")
			}
			return r.mismatch(treasury, actual)
		}
		return r.ok(treasury, fmt.Sprintf("%s USDC reached the treasury", usdc(toTreasury)))
	}
	return nil
}

// refundDestination is where a refund was sent: the address the payer gave,
// otherwise the account the deposit came from, as the chain worker does.
func refundDestination(o *store.Order) string {
	if o.RefundAddress != "" {
		return o.RefundAddress
	}
	return o.DepositFrom
}

func (r *run) checkRefund(o *store.Order) error {
	to := refundDestination(o)
	item := store.ReconciliationItem{
		OrderID: o.ID, CheckName: CheckRefundTx, Account: to, TxHash: o.RefundTxHash,
		Expected: fmt.Sprintf("a successful refund of %s USDC to the payer, %s", usdc(o.AmountUSDC), short(to)),
	}
	tx, err := r.e.Ledger.LedgerTransaction(o.RefundTxHash)
	if err != nil {
		return r.unanswered(item, err)
	}
	if p := txProblem(tx); p != "" {
		return r.mismatch(item, p)
	}
	out := usdcOutOf(tx, o.DepositAddress)
	refunded := out[to]
	if math.Abs(refunded-o.AmountUSDC) > money.DustUSDC {
		actual := fmt.Sprintf("%s USDC reached the payer", usdc(refunded))
		for dest, v := range out {
			if dest != to {
				actual += fmt.Sprintf("; %s USDC went to %s", usdc(v), short(dest))
			}
		}
		return r.mismatch(item, actual)
	}
	return r.ok(item, fmt.Sprintf("a successful refund of %s USDC to the payer", usdc(refunded)))
}

func (r *run) checkClosed(o *store.Order) error {
	item := store.ReconciliationItem{
		OrderID: o.ID, CheckName: CheckAccountClosed, Account: o.DepositAddress,
		Expected: fmt.Sprintf("the deposit account is emptied and merged away (%s)", o.Status),
	}
	state, err := r.e.Ledger.AccountState(o.DepositAddress)
	r.cycle.AccountsChecked++
	if err != nil {
		return r.unanswered(item, err)
	}
	if state.Exists {
		return r.mismatch(item, fmt.Sprintf("the account still exists, holding %s USDC", usdc(state.USDC)))
	}
	return r.ok(item, "the account is no longer on the ledger")
}

// checkPayout compares the Linq backend's record of the NGN payout with ours.
//
// It asks about every reference the payout worker sent for the order (see
// payoutRefs). On a paid order the last one must be paid and every earlier one
// must not be: an earlier reference that paid too is the merchant paid twice.
// On a refunded order none may be paid or still going.
func (r *run) checkPayout(o *store.Order) error {
	attempts, err := store.PayoutAttempts(r.e.DB, o.ID)
	if err != nil {
		return fmt.Errorf("load payout attempts for %s: %w", o.ID, err)
	}
	refs, induced := payoutRefs(o.ID, attempts)
	if !r.spendPayouts(len(refs)) {
		return nil
	}
	refunded := o.FinancialOutcome == "refunded"
	last, earlier := refs[len(refs)-1], refs[:len(refs)-1]

	item := store.ReconciliationItem{OrderID: o.ID, CheckName: CheckPayout}
	switch {
	case refunded && len(refs) == 1:
		item.Expected = "no NGN paid: the payout failed and the USDC was refunded"
	case refunded:
		item.Expected = fmt.Sprintf("no NGN paid under %s: the payout failed and the USDC was refunded",
			strings.Join(refs, ", "))
	case len(refs) == 1:
		item.Expected = fmt.Sprintf("paid ₦%s, payout %s via %s",
			naira(o.AmountNGN), orUnknown(o.PayoutReference), orUnknown(o.PayoutProvider))
	default:
		item.Expected = fmt.Sprintf("paid ₦%s under %s, payout %s via %s; nothing paid under %s",
			naira(o.AmountNGN), last, orUnknown(o.PayoutReference), orUnknown(o.PayoutProvider),
			strings.Join(earlier, ", "))
	}
	if induced > 0 {
		item.Expected += fmt.Sprintf("; %d induced %s, never sent", induced, plural(induced, "attempt", "attempts"))
	}

	answers := make([]payoutAnswer, len(refs))
	for i, ref := range refs {
		res, err := r.e.Payouts.Status(ref)
		if err != nil && !errors.Is(err, payout.ErrNotFound) {
			if len(refs) > 1 {
				err = fmt.Errorf("%s: %w", ref, err)
			}
			return r.unanswered(item, err)
		}
		answers[i] = payoutAnswer{ref: ref, found: err == nil, result: res}
	}
	actual := describeAnswers(answers)

	var problems []string
	if refunded {
		for _, a := range answers {
			// Paid, or still going: either way the merchant may be paid on top
			// of a refund that has already gone out on-chain.
			if p := a.paidOrGoing(); p != "" {
				problems = append(problems, a.name(len(refs))+p+", but the USDC was refunded")
			}
		}
	} else {
		final := answers[len(answers)-1]
		if !final.found {
			if len(refs) == 1 {
				return r.mismatch(item, "the Linq backend has no payout for this order")
			}
			problems = append(problems, "no payout under "+last)
		} else {
			res := final.result
			if res.Status != payout.StatusPaid {
				problems = append(problems, "not paid")
			}
			if o.PayoutReference != "" && res.Reference != o.PayoutReference {
				problems = append(problems, "a different payout ID")
			}
			if o.PayoutProvider != "" && res.Provider != o.PayoutProvider {
				problems = append(problems, "a different provider")
			}
			if res.AmountNGN != nil && money.RoundNGN(*res.AmountNGN) != money.RoundNGN(o.AmountNGN) {
				problems = append(problems, "a different amount")
			}
		}
		for _, a := range answers[:len(answers)-1] {
			switch a.paidOrGoing() {
			case "paid":
				problems = append(problems, "also paid under "+a.ref+": the merchant may have been paid twice")
			case "still in progress":
				problems = append(problems, a.ref+" still in progress")
			}
		}
	}
	if len(problems) > 0 {
		return r.mismatch(item, actual+" ("+strings.Join(problems, ", ")+")")
	}
	return r.ok(item, actual)
}

// payoutRefs lists the references the payout worker sent the Linq backend for
// an order, oldest first, with the reference of its last attempt at the end,
// and counts the induced attempts, which never reached the backend.
//
// With PAYOUT_NEW_REFERENCE_ON_RETRY off every attempt reuses the order ID, so
// that is one reference. An order with no attempt on record, paid before
// attempts were recorded, or with only induced ones, is asked about by its ID:
// the reference every payout used before retries had their own.
func payoutRefs(orderID string, attempts []store.PayoutAttempt) (refs []string, induced int) {
	for _, a := range attempts {
		if a.Induced {
			induced++
			continue
		}
		for i, ref := range refs {
			if ref == a.Reference {
				refs = append(refs[:i], refs[i+1:]...)
				break
			}
		}
		refs = append(refs, a.Reference)
	}
	if len(refs) == 0 {
		refs = []string{orderID}
	}
	return refs, induced
}

// payoutAnswer is what the Linq backend said about one reference.
type payoutAnswer struct {
	ref    string
	found  bool
	result payout.Result
}

// paidOrGoing says whether a reference paid, or may yet: "paid", "still in
// progress", or "" when it failed or never reached the backend.
func (a payoutAnswer) paidOrGoing() string {
	switch {
	case !a.found || a.result.Status == payout.StatusFailed:
		return ""
	case a.result.Status == payout.StatusPaid:
		return "paid"
	default:
		return "still in progress"
	}
}

// name prefixes a problem with the reference, when there is more than one.
func (a payoutAnswer) name(refs int) string {
	if refs == 1 {
		return ""
	}
	return a.ref + " "
}

// describeAnswers says what the Linq backend reported for each reference.
func describeAnswers(answers []payoutAnswer) string {
	if len(answers) == 1 {
		if !answers[0].found {
			return "the Linq backend has no payout for this order"
		}
		return describePayout(answers[0].result)
	}
	parts := make([]string, len(answers))
	for i, a := range answers {
		if !a.found {
			parts[i] = a.ref + ": no payout"
			continue
		}
		parts[i] = a.ref + ": " + describePayout(a.result)
	}
	return strings.Join(parts, "; ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// describePayout says what the Linq backend reported, without bank details:
// the payout record holds none, and none must reach a reviewer.
func describePayout(res payout.Result) string {
	amount := "amount not reported"
	if res.AmountNGN != nil {
		amount = "₦" + naira(*res.AmountNGN)
	}
	return fmt.Sprintf("%s, %s, payout %s via %s", res.Status, amount, orUnknown(res.Reference), orUnknown(res.Provider))
}

// --- 5. Sponsored accounts with no order --------------------------------------

func (r *run) sponsoredAccounts() error {
	if !r.e.CheckSponsored {
		return nil
	}
	cursor, err := store.LoadCursor(r.e.DB, sponsoredCursorID)
	if err != nil {
		return fmt.Errorf("load sponsor search cursor: %w", err)
	}
	for r.ctx.Err() == nil && r.spendHorizon() {
		page, err := r.e.Ledger.SponsoredAccounts(cursor, sponsoredPage)
		if err != nil {
			return r.unanswered(store.ReconciliationItem{
				CheckName: CheckSponsoredAccount,
				Expected:  "the list of accounts the sponsor pays reserves for",
			}, err)
		}
		r.cycle.AccountsChecked += len(page.Accounts)
		if err := r.flagStrangers(page.Accounts); err != nil {
			return err
		}
		// The place is saved after the page is recorded, so a crash re-reads
		// a page rather than skipping one. An empty Next starts the next cycle
		// from the beginning, so every account is looked at again in turn.
		cursor = page.Next
		if err := store.SaveCursor(r.e.DB, sponsoredCursorID, cursor); err != nil {
			return fmt.Errorf("save sponsor search cursor: %w", err)
		}
		if cursor == "" {
			return nil
		}
	}
	return nil
}

func (r *run) flagStrangers(accounts []string) error {
	known, err := store.KnownAccounts(r.e.DB, accounts)
	if err != nil {
		return fmt.Errorf("look up sponsored accounts: %w", err)
	}
	var strangers []string
	for _, a := range accounts {
		if !known[a] {
			strangers = append(strangers, a)
		}
	}
	if len(strangers) == 0 {
		return nil
	}
	// A second look: an order or pool row may have been written while the
	// page was being read, and its account only just provisioned.
	beforeSecondLook()
	known, err = store.KnownAccounts(r.e.DB, strangers)
	if err != nil {
		return fmt.Errorf("look up sponsored accounts: %w", err)
	}
	resolved, err := store.ResolvedAccounts(r.e.DB, CheckSponsoredAccount, strangers)
	if err != nil {
		return fmt.Errorf("look up resolved accounts: %w", err)
	}
	for _, a := range strangers {
		if known[a] || resolved[a] {
			continue
		}
		if err := r.mismatch(store.ReconciliationItem{
			CheckName: CheckSponsoredAccount, Account: a,
			Expected: "every account the sponsor pays reserves for belongs to an order or the pool",
		}, "no order and no pool record for this account"); err != nil {
			return err
		}
	}
	return nil
}

// --- Formatting -----------------------------------------------------------------

// usdc shows an amount as the ledger does, to seven places, without trailing
// zeros. Display only: no rounding rule is applied here.
func usdc(v float64) string {
	s := strings.TrimRight(stellar.FormatAmount(v), "0")
	return strings.TrimSuffix(s, ".")
}

// naira shows an NGN amount to the kobo.
func naira(v float64) string {
	return fmt.Sprintf("%.2f", money.RoundNGN(v))
}

// short abbreviates a Stellar account for a sentence; the full account is in
// the item's Account column.
func short(account string) string {
	if len(account) <= 12 {
		return account
	}
	return account[:6] + "…" + account[len(account)-6:]
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
