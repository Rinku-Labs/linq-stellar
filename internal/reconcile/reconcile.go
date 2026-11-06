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
	// 6. The Linq backend's record of the NGN payout agrees with ours.
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
	orders   map[string]bool
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
		orders: map[string]bool{},
	}
	defer func() {
		if p := recover(); p != nil {
			r.fail(fmt.Errorf("panic: %v", p))
		}
		r.finish(started)
		result = r.cycle
	}()

	for _, step := range []func() error{r.openAccounts, r.finishedOrders, r.sponsoredAccounts} {
		if ctx.Err() != nil {
			r.fail(errors.New("stopped early: the worker is shutting down"))
			break
		}
		if err := step(); err != nil {
			r.fail(err)
			break
		}
	}
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

func (r *run) spendPayout() bool {
	if r.payoutsLeft <= 0 {
		r.deferred++
		return false
	}
	r.payoutsLeft--
	return true
}

func (r *run) outOfBudget() bool {
	horizonDone := r.limited != nil || r.horizonLeft <= 0
	payoutsDone := r.e.Payouts == nil || r.payoutsLeft <= 0
	return horizonDone && payoutsDone
}

// record writes one item and counts it.
func (r *run) record(item store.ReconciliationItem) error {
	item.CycleID = r.cycle.ID
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

func (r *run) openAccounts() error {
	var open []store.Order
	err := r.e.DB.Select("id", "status", "deposit_address", "amount_usdc").
		Where("status IN ? AND deposit_address <> ''", openStates).
		Order("updated_at, id").Find(&open).Error
	if err != nil {
		return fmt.Errorf("load open orders: %w", err)
	}
	for i := range open {
		if r.ctx.Err() != nil {
			return nil
		}
		if !r.spendHorizon() {
			continue
		}
		if err := r.checkOpenAccount(&open[i]); err != nil {
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

// finishedOrders runs the checks whose answer never changes once given: the
// transactions an order recorded, whether its account was closed, and the
// payout. Newest orders first, so a demo order is checked in the next cycle
// whatever backlog sits behind it; a check that has passed, or whose mismatch
// someone resolved, is not run again.
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
		settled, err := store.SettledChecks(r.e.DB, ids)
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
	if r.e.Payouts != nil && payoutDue(o) && due(CheckPayout) && r.spendPayout() {
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
		return r.mismatch(item, fmt.Sprintf("the transaction paid %s USDC into the deposit account", usdc(paid)))
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
// The reference is the order ID, which is what the payout worker sends. When
// retries start using their own references (SD-4), this must ask about the
// last one the order used.
func (r *run) checkPayout(o *store.Order) error {
	item := store.ReconciliationItem{OrderID: o.ID, CheckName: CheckPayout}
	refunded := o.FinancialOutcome == "refunded"
	if refunded {
		item.Expected = "no NGN paid: the payout failed and the USDC was refunded"
	} else {
		item.Expected = fmt.Sprintf("paid ₦%s, payout %s via %s",
			naira(o.AmountNGN), orUnknown(o.PayoutReference), orUnknown(o.PayoutProvider))
	}

	result, err := r.e.Payouts.Status(o.ID)
	if errors.Is(err, payout.ErrNotFound) {
		if refunded {
			return r.ok(item, "the Linq backend has no payout for this order")
		}
		return r.mismatch(item, "the Linq backend has no payout for this order")
	}
	if err != nil {
		return r.unanswered(item, err)
	}
	actual := describePayout(result)

	if refunded {
		if result.Status == payout.StatusFailed {
			return r.ok(item, actual)
		}
		// Paid, or still going: either way the merchant may be paid on top of
		// a refund that has already gone out on-chain.
		return r.mismatch(item, actual)
	}

	var problems []string
	if result.Status != payout.StatusPaid {
		problems = append(problems, "not paid")
	}
	if o.PayoutReference != "" && result.Reference != o.PayoutReference {
		problems = append(problems, "a different payout ID")
	}
	if o.PayoutProvider != "" && result.Provider != o.PayoutProvider {
		problems = append(problems, "a different provider")
	}
	if result.AmountNGN != nil && money.RoundNGN(*result.AmountNGN) != money.RoundNGN(o.AmountNGN) {
		problems = append(problems, "a different amount")
	}
	if len(problems) > 0 {
		return r.mismatch(item, actual+" ("+strings.Join(problems, ", ")+")")
	}
	return r.ok(item, actual)
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
