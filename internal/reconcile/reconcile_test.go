package reconcile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Rinku-Labs/linq-stellar/internal/payout"
	"github.com/Rinku-Labs/linq-stellar/internal/stellar"
	"github.com/Rinku-Labs/linq-stellar/internal/store"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Accounts used throughout. Real-looking but made up: nothing here touches a
// network.
const (
	treasury = "GTREASURY0000000000000000000000000000000000000000000000"
	payer    = "GPAYER0000000000000000000000000000000000000000000000000"
	stranger = "GSTRANGER000000000000000000000000000000000000000000000"
)

func depositAccount(n int) string { return fmt.Sprintf("GDEPOSIT%047d", n) }

// fakeLedger is Horizon as a map. An address it does not know is an account
// that does not exist; a hash it does not know is a transaction that was never
// on the ledger.
type fakeLedger struct {
	accounts  map[string]stellar.AccountState
	txs       map[string]stellar.LedgerTx
	sponsored []string
	// fail makes a lookup of this address or hash return the error.
	fail map[string]error
	// before runs ahead of each lookup, so a test can move an order while it
	// is being checked.
	before func(key string)
	calls  int
}

func newLedger() *fakeLedger {
	return &fakeLedger{accounts: map[string]stellar.AccountState{}, txs: map[string]stellar.LedgerTx{}, fail: map[string]error{}}
}

func (f *fakeLedger) lookup(key string) error {
	f.calls++
	if f.before != nil {
		f.before(key)
	}
	return f.fail[key]
}

func (f *fakeLedger) AccountState(address string) (stellar.AccountState, error) {
	if err := f.lookup(address); err != nil {
		return stellar.AccountState{}, err
	}
	return f.accounts[address], nil
}

func (f *fakeLedger) LedgerTransaction(hash string) (stellar.LedgerTx, error) {
	if err := f.lookup(hash); err != nil {
		return stellar.LedgerTx{}, err
	}
	return f.txs[hash], nil
}

func (f *fakeLedger) SponsoredAccounts(cursor string, limit int) (stellar.SponsoredPage, error) {
	if err := f.lookup("sponsored"); err != nil {
		return stellar.SponsoredPage{}, err
	}
	start := 0
	if cursor != "" {
		start, _ = strconv.Atoi(cursor)
	}
	end := min(start+limit, len(f.sponsored))
	page := stellar.SponsoredPage{Accounts: f.sponsored[start:end]}
	if end < len(f.sponsored) {
		page.Next = strconv.Itoa(end)
	}
	return page, nil
}

func (f *fakeLedger) holds(address string, usdc float64) {
	f.accounts[address] = stellar.AccountState{Exists: true, TrustsUSDC: true, USDC: usdc}
}

// pays records a successful transaction moving USDC.
func (f *fakeLedger) pays(hash string, moves ...stellar.Movement) {
	f.txs[hash] = stellar.LedgerTx{Found: true, Successful: true, Movements: moves}
}

func move(from, to string, amount float64) stellar.Movement {
	return stellar.Movement{Kind: "payment", From: from, To: to, USDC: true, Amount: amount}
}

type fakePayouts struct {
	results map[string]payout.Result
	fail    map[string]error
	calls   int
	asked   []string
	panicOn string
}

func newPayouts() *fakePayouts {
	return &fakePayouts{results: map[string]payout.Result{}, fail: map[string]error{}}
}

func (f *fakePayouts) Status(reference string) (payout.Result, error) {
	f.calls++
	f.asked = append(f.asked, reference)
	if reference == f.panicOn {
		panic("payout client blew up")
	}
	if err := f.fail[reference]; err != nil {
		return payout.Result{}, err
	}
	res, ok := f.results[reference]
	if !ok {
		return payout.Result{}, fmt.Errorf("%w: %s", payout.ErrNotFound, reference)
	}
	return res, nil
}

func ngn(v float64) *float64 { return &v }

type harness struct {
	t       *testing.T
	db      *gorm.DB
	ledger  *fakeLedger
	payouts *fakePayouts
	engine  *Engine
	clock   time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dsn := fmt.Sprintf("file:reconcile_%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		if sql, err := db.DB(); err == nil {
			_ = sql.Close()
		}
	})
	h := &harness{t: t, db: db, ledger: newLedger(), payouts: newPayouts(),
		clock: time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)}
	h.engine = &Engine{
		DB: db, Ledger: h.ledger, Payouts: h.payouts, Treasury: treasury,
		HorizonCalls: 100, PayoutCalls: 100, CheckSponsored: true,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return h.clock },
	}
	return h
}

// order saves an order. n orders the orders by age: a higher n is newer.
func (h *harness) order(n int, o store.Order) store.Order {
	h.t.Helper()
	if o.ID == "" {
		o.ID = fmt.Sprintf("ord_%03d", n)
	}
	o.IdempotencyKey = o.ID
	if o.DepositAddress == "" {
		o.DepositAddress = depositAccount(n)
	}
	o.CreatedAt = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Minute)
	o.UpdatedAt = o.CreatedAt
	if err := h.db.Create(&o).Error; err != nil {
		h.t.Fatalf("create order: %v", err)
	}
	return o
}

// settled is an order that went all the way: deposit, payout, sweep.
func settled(amountUSDC, amountNGN float64) store.Order {
	return store.Order{
		Status: store.StateSettledInTreasury, FinancialOutcome: "disbursed",
		AmountUSDC: amountUSDC, AmountNGN: amountNGN,
		DepositTxHash: "dep", SweepTxHash: "swp", DepositFrom: payer,
		PayoutReference: "lq_payout_1", PayoutProvider: "nomba",
	}
}

func (h *harness) cycle() (store.ReconciliationCycle, map[string]store.ReconciliationItem) {
	h.t.Helper()
	c, err := h.engine.RunCycle(context.Background())
	if err != nil {
		h.t.Fatalf("run cycle: %v", err)
	}
	h.clock = h.clock.Add(15 * time.Minute)
	return c, h.items(c.ID)
}

// items keys a cycle's items by "order/check", or "account/check" for an item
// with no order.
func (h *harness) items(cycleID int64) map[string]store.ReconciliationItem {
	h.t.Helper()
	var rows []store.ReconciliationItem
	if err := h.db.Where("cycle_id = ?", cycleID).Find(&rows).Error; err != nil {
		h.t.Fatalf("load items: %v", err)
	}
	out := map[string]store.ReconciliationItem{}
	for _, row := range rows {
		key := row.OrderID
		if key == "" {
			key = row.Account
		}
		key += "/" + row.CheckName
		if _, dup := out[key]; dup {
			h.t.Fatalf("two items for %s in one cycle", key)
		}
		out[key] = row
	}
	return out
}

func expect(t *testing.T, items map[string]store.ReconciliationItem, key, result string) store.ReconciliationItem {
	t.Helper()
	item, ok := items[key]
	if !ok {
		keys := make([]string, 0, len(items))
		for k := range items {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		t.Fatalf("no item %s; have %v", key, keys)
	}
	if item.Result != result {
		t.Errorf("%s: %s (%q, expected %q), want %s", key, item.Result, item.Actual, item.Expected, result)
	}
	return item
}

func absent(t *testing.T, items map[string]store.ReconciliationItem, key string) {
	t.Helper()
	if item, ok := items[key]; ok {
		t.Errorf("%s should not be checked, got %s (%q)", key, item.Result, item.Actual)
	}
}

// 1. Every open deposit account holds what its order expects.
func TestOpenAccountsHoldWhatTheirOrderExpects(t *testing.T) {
	h := newHarness(t)
	empty := h.order(1, store.Order{Status: store.StateAwaitingDeposit})
	h.ledger.holds(empty.DepositAddress, 0)
	unnoticed := h.order(2, store.Order{Status: store.StateAwaitingDeposit})
	h.ledger.holds(unnoticed.DepositAddress, 5)
	holding := h.order(3, store.Order{Status: store.StatePayoutQueued, AmountUSDC: 0.08})
	h.ledger.holds(holding.DepositAddress, 0.08)
	short := h.order(4, store.Order{Status: store.StateDisbursed, AmountUSDC: 0.08})
	h.ledger.holds(short.DepositAddress, 0.05)
	gone := h.order(5, store.Order{Status: store.StateSweepQueued, AmountUSDC: 1})
	inFlight := h.order(6, store.Order{Status: store.StateSweepProcessing, AmountUSDC: 1})

	_, items := h.cycle()
	expect(t, items, empty.ID+"/open_account", store.ReconcileOK)
	m := expect(t, items, unnoticed.ID+"/open_account", store.ReconcileMismatch)
	if m.Actual != "holds 5 USDC, but the order is not marked as deposited" || m.Account != unnoticed.DepositAddress {
		t.Errorf("unnoticed deposit: %+v", m)
	}
	expect(t, items, holding.ID+"/open_account", store.ReconcileOK)
	if m := expect(t, items, short.ID+"/open_account", store.ReconcileMismatch); m.Actual != "holds 0.05 USDC" {
		t.Errorf("short balance: %q", m.Actual)
	}
	if m := expect(t, items, gone.ID+"/open_account", store.ReconcileMismatch); m.Actual != "the account is not on the ledger" {
		t.Errorf("missing account: %q", m.Actual)
	}
	absent(t, items, inFlight.ID+"/open_account")
}

// A deposit noticed a moment after the order was read is the order moving, not
// a mismatch.
func TestAnOrderThatMovesDuringTheCheckIsNotFlagged(t *testing.T) {
	h := newHarness(t)
	o := h.order(1, store.Order{Status: store.StateAwaitingDeposit})
	h.ledger.holds(o.DepositAddress, 5)
	h.ledger.before = func(key string) {
		if key == o.DepositAddress {
			h.db.Model(&store.Order{}).Where("id = ?", o.ID).Update("status", store.StateDepositDetected)
		}
	}
	_, items := h.cycle()
	absent(t, items, o.ID+"/open_account")
}

// 2. Every settled, refunded or reclaimed order's account is merged away.
func TestFinishedAccountsAreClosed(t *testing.T) {
	h := newHarness(t)
	closed := h.order(1, store.Order{Status: store.StateSettledInTreasury})
	lingering := h.order(2, store.Order{Status: store.StateSettledInTreasury})
	h.ledger.holds(lingering.DepositAddress, 0)
	reclaimed := h.order(3, store.Order{Status: store.StateExpired, ReservesReclaimed: true})
	notYet := h.order(4, store.Order{Status: store.StateExpired})
	h.ledger.holds(notYet.DepositAddress, 0)
	refunded := h.order(5, store.Order{Status: store.StateRefunded})

	_, items := h.cycle()
	expect(t, items, closed.ID+"/account_closed", store.ReconcileOK)
	// Settled and refunded with no transaction recorded: the books disagree
	// with themselves.
	if m := expect(t, items, closed.ID+"/sweep_tx", store.ReconcileMismatch); m.Actual != "the order is settled, but no sweep transaction is recorded" {
		t.Errorf("settled without a sweep: %q", m.Actual)
	}
	expect(t, items, refunded.ID+"/refund_tx", store.ReconcileMismatch)
	if m := expect(t, items, lingering.ID+"/account_closed", store.ReconcileMismatch); m.Actual != "the account still exists, holding 0 USDC" {
		t.Errorf("lingering: %q", m.Actual)
	}
	expect(t, items, reclaimed.ID+"/account_closed", store.ReconcileOK)
	absent(t, items, notYet.ID+"/account_closed")
	expect(t, items, refunded.ID+"/account_closed", store.ReconcileOK)
}

// 3. Recorded transactions are on the ledger, succeeded, and moved the
// recorded amount to the right place. 4. A sweep's USDC reached the treasury.
func TestRecordedTransactionsMatchTheLedger(t *testing.T) {
	h := newHarness(t)
	acct := func(n int) string { return depositAccount(n) }

	good := h.order(1, store.Order{Status: store.StateSettledInTreasury, AmountUSDC: 0.08, DepositTxHash: "dep1", SweepTxHash: "swp1"})
	h.ledger.pays("dep1", move(payer, acct(1), 0.08))
	h.ledger.pays("swp1", move(acct(1), treasury, 0.08), stellar.Movement{Kind: "account_merge", From: acct(1), To: "GSPONSOR"})

	missing := h.order(2, store.Order{Status: store.StateDisbursed, AmountUSDC: 0.08, DepositTxHash: "nowhere"})
	h.ledger.holds(acct(2), 0.08)

	failed := h.order(3, store.Order{Status: store.StateDisbursed, AmountUSDC: 0.08, DepositTxHash: "dep3"})
	h.ledger.holds(acct(3), 0.08)
	h.ledger.txs["dep3"] = stellar.LedgerTx{Found: true, Successful: false, Movements: []stellar.Movement{move(payer, acct(3), 0.08)}}

	wrongAmount := h.order(4, store.Order{Status: store.StateDisbursed, AmountUSDC: 0.08, DepositTxHash: "dep4"})
	h.ledger.holds(acct(4), 0.08)
	h.ledger.pays("dep4", move(payer, acct(4), 0.07))
	// More than recorded is no split deposit, so no hint.
	tooMuch := h.order(11, store.Order{Status: store.StateDisbursed, AmountUSDC: 0.08, DepositTxHash: "dep11"})
	h.ledger.holds(acct(11), 0.08)
	h.ledger.pays("dep11", move(payer, acct(11), 0.09))

	elsewhere := h.order(5, store.Order{Status: store.StateSettledInTreasury, AmountUSDC: 1, SweepTxHash: "swp5"})
	h.ledger.pays("swp5", move(acct(5), stranger, 1))

	refundGiven := h.order(6, store.Order{Status: store.StateRefunded, AmountUSDC: 2, RefundAddress: payer, RefundTxHash: "ref6"})
	h.ledger.pays("ref6", move(acct(6), payer, 2))

	refundFromDeposit := h.order(7, store.Order{Status: store.StateRefunded, AmountUSDC: 2, DepositFrom: "GFROM", RefundTxHash: "ref7"})
	h.ledger.pays("ref7", move(acct(7), "GFROM", 2))

	refundAstray := h.order(8, store.Order{Status: store.StateRefunded, AmountUSDC: 2, RefundAddress: payer, RefundTxHash: "ref8"})
	h.ledger.pays("ref8", move(acct(8), stranger, 2))

	// The recorded deposit paid someone else's account.
	misdirected := h.order(9, store.Order{Status: store.StateDisbursed, AmountUSDC: 0.08, DepositTxHash: "dep9"})
	h.ledger.holds(acct(9), 0.08)
	h.ledger.pays("dep9", move(payer, stranger, 0.08))

	_, items := h.cycle()
	if m := expect(t, items, misdirected.ID+"/deposit_tx", store.ReconcileMismatch); m.Actual != "the transaction paid no USDC into the deposit account" {
		t.Errorf("misdirected deposit: %q", m.Actual)
	}
	if ok := expect(t, items, good.ID+"/deposit_tx", store.ReconcileOK); ok.TxHash != "dep1" || ok.Account != acct(1) {
		t.Errorf("deposit item links: %+v", ok)
	}
	expect(t, items, good.ID+"/sweep_tx", store.ReconcileOK)
	expect(t, items, good.ID+"/sweep_to_treasury", store.ReconcileOK)

	if m := expect(t, items, missing.ID+"/deposit_tx", store.ReconcileMismatch); m.Actual != "the transaction is not on the ledger" {
		t.Errorf("missing tx: %q", m.Actual)
	}
	if m := expect(t, items, failed.ID+"/deposit_tx", store.ReconcileMismatch); m.Actual != "the transaction is on the ledger but failed, so it moved nothing" {
		t.Errorf("failed tx: %q", m.Actual)
	}
	if m := expect(t, items, tooMuch.ID+"/deposit_tx", store.ReconcileMismatch); m.Actual != "the transaction paid 0.09 USDC into the deposit account" {
		t.Errorf("more than recorded: %q", m.Actual)
	}
	if m := expect(t, items, wrongAmount.ID+"/deposit_tx", store.ReconcileMismatch); m.Actual != "the transaction paid 0.07 USDC into the deposit account; if the payer sent the deposit in more than one payment, the rest is in a transaction this order does not record" {
		t.Errorf("wrong amount: %q", m.Actual)
	}
	expect(t, items, elsewhere.ID+"/sweep_tx", store.ReconcileOK)
	if m := expect(t, items, elsewhere.ID+"/sweep_to_treasury", store.ReconcileMismatch); m.Actual != "0 USDC reached the treasury; 1 USDC to GSTRAN…000000" {
		t.Errorf("sweep elsewhere: %q", m.Actual)
	}
	expect(t, items, refundGiven.ID+"/refund_tx", store.ReconcileOK)
	expect(t, items, refundFromDeposit.ID+"/refund_tx", store.ReconcileOK)
	expect(t, items, refundAstray.ID+"/refund_tx", store.ReconcileMismatch)
}

// 5. No account sponsored by our sponsor exists without an order behind it.
func TestSponsoredAccountsWithNoOrderAreFlagged(t *testing.T) {
	h := newHarness(t)
	o := h.order(1, store.Order{Status: store.StateSettledInTreasury})
	if err := h.db.Create(&store.PoolAccount{Address: "GPOOL", Ready: true}).Error; err != nil {
		t.Fatal(err)
	}
	late := "GLATE"
	h.ledger.sponsored = []string{o.DepositAddress, "GPOOL", stranger, late}
	// GLATE's order is written after the first look missed it: the second
	// look finds it.
	saved := beforeSecondLook
	beforeSecondLook = func() {
		if err := h.db.Create(&store.Order{ID: "ord_late", IdempotencyKey: "ord_late", DepositAddress: late,
			Status: store.StateAwaitingDeposit}).Error; err != nil {
			t.Fatalf("create late order: %v", err)
		}
		beforeSecondLook = func() {}
	}
	t.Cleanup(func() { beforeSecondLook = saved })

	c, items := h.cycle()
	if m := expect(t, items, stranger+"/sponsored_account", store.ReconcileMismatch); m.Account != stranger || m.OrderID != "" {
		t.Errorf("stranger: %+v", m)
	}
	absent(t, items, o.DepositAddress+"/sponsored_account")
	absent(t, items, "GPOOL/sponsored_account")
	absent(t, items, late+"/sponsored_account")
	if c.AccountsChecked < 4 {
		t.Errorf("accounts checked %d, want the four sponsored ones counted", c.AccountsChecked)
	}

	// Once someone explains it, it stops being flagged.
	if _, err := store.ResolveReconciliationItem(h.db, items[stranger+"/sponsored_account"].ID, "Opened by hand to test the sponsor", "ify", h.clock); err != nil {
		t.Fatal(err)
	}
	_, items = h.cycle()
	absent(t, items, stranger+"/sponsored_account")
}

func TestTheSponsorSearchIsOffUnlessSwitchedOn(t *testing.T) {
	h := newHarness(t)
	h.engine.CheckSponsored = false
	h.ledger.sponsored = []string{stranger}
	_, items := h.cycle()
	absent(t, items, stranger+"/sponsored_account")
	if h.ledger.calls != 0 {
		t.Errorf("%d Horizon calls with nothing to check and the search off", h.ledger.calls)
	}
}

func TestTheSponsorSearchKeepsItsPlaceAcrossCycles(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 450; i++ {
		h.ledger.sponsored = append(h.ledger.sponsored, fmt.Sprintf("GS%05d", i))
	}
	h.engine.HorizonCalls = 2 // two pages of 200 a cycle
	c, items := h.cycle()
	if c.AccountsChecked != 400 || len(items) != 400 {
		t.Fatalf("first cycle: %d accounts, %d items, want 400", c.AccountsChecked, len(items))
	}
	c, items = h.cycle()
	if c.AccountsChecked != 50 || len(items) != 50 {
		t.Fatalf("second cycle should finish the list: %d accounts, %d items", c.AccountsChecked, len(items))
	}
	expect(t, items, "GS00449/sponsored_account", store.ReconcileMismatch)
	// With the list finished, the next cycle starts again from the top.
	c, _ = h.cycle()
	if c.AccountsChecked != 400 {
		t.Errorf("third cycle: %d accounts, want to start over", c.AccountsChecked)
	}
}

// 6. Every disbursed order's NGN payout matches the Linq backend's record.
func TestPayoutsMatchTheLinqBackend(t *testing.T) {
	h := newHarness(t)
	paid := func(n int, ref, provider string, amount *float64) store.Order {
		o := h.order(n, store.Order{Status: store.StateSettledInTreasury, FinancialOutcome: "disbursed",
			AmountNGN: 10000, PayoutReference: "lq_" + strconv.Itoa(n), PayoutProvider: "nomba"})
		h.payouts.results[o.ID] = payout.Result{Status: payout.StatusPaid, Reference: ref, Provider: provider, AmountNGN: amount}
		return o
	}
	agrees := paid(1, "lq_1", "nomba", ngn(10000))
	noAmount := paid(2, "lq_2", "nomba", nil)
	otherAmount := paid(3, "lq_3", "nomba", ngn(9500))
	otherID := paid(4, "lq_other", "nomba", ngn(10000))
	notPaid := h.order(5, store.Order{Status: store.StateDisbursed, FinancialOutcome: "disbursed", AmountNGN: 10000, PayoutReference: "lq_5"})
	h.ledger.holds(notPaid.DepositAddress, 0)
	h.payouts.results[notPaid.ID] = payout.Result{Status: payout.StatusFailed, Reference: "lq_5"}
	unknown := h.order(6, store.Order{Status: store.StateSettledInTreasury, FinancialOutcome: "disbursed", AmountNGN: 10000})

	refundedFailed := h.order(7, store.Order{Status: store.StateRefunded, FinancialOutcome: "refunded"})
	h.payouts.results[refundedFailed.ID] = payout.Result{Status: payout.StatusFailed}
	refundedNever := h.order(8, store.Order{Status: store.StateRefunded, FinancialOutcome: "refunded"})
	refundedButPaid := h.order(9, store.Order{Status: store.StateRefunded, FinancialOutcome: "refunded"})
	h.payouts.results[refundedButPaid.ID] = payout.Result{Status: payout.StatusPaid, Reference: "lq_9", Provider: "nomba"}
	unreachable := h.order(10, store.Order{Status: store.StateSettledInTreasury, FinancialOutcome: "disbursed"})
	h.payouts.fail[unreachable.ID] = errors.New("payout: connection refused")
	inProgress := h.order(11, store.Order{Status: store.StatePayoutProcessing})
	h.ledger.holds(inProgress.DepositAddress, 0)

	_, items := h.cycle()
	if ok := expect(t, items, agrees.ID+"/payout", store.ReconcileOK); ok.Actual != "paid, ₦10000.00, payout lq_1 via nomba" || ok.Expected != "paid ₦10000.00, payout lq_1 via nomba" {
		t.Errorf("agrees: %+v", ok)
	}
	if ok := expect(t, items, noAmount.ID+"/payout", store.ReconcileOK); ok.Actual != "paid, amount not reported, payout lq_2 via nomba" {
		t.Errorf("no amount: %q", ok.Actual)
	}
	if m := expect(t, items, otherAmount.ID+"/payout", store.ReconcileMismatch); m.Actual != "paid, ₦9500.00, payout lq_3 via nomba (a different amount)" {
		t.Errorf("other amount: %q", m.Actual)
	}
	expect(t, items, otherID.ID+"/payout", store.ReconcileMismatch)
	expect(t, items, notPaid.ID+"/payout", store.ReconcileMismatch)
	if m := expect(t, items, unknown.ID+"/payout", store.ReconcileMismatch); m.Actual != "the Linq backend has no payout for this order" {
		t.Errorf("unknown: %q", m.Actual)
	}
	expect(t, items, refundedFailed.ID+"/payout", store.ReconcileOK)
	expect(t, items, refundedNever.ID+"/payout", store.ReconcileOK)
	expect(t, items, refundedButPaid.ID+"/payout", store.ReconcileMismatch)
	if e := expect(t, items, unreachable.ID+"/payout", store.ReconcileError); e.Actual != "could not check: payout: connection refused" {
		t.Errorf("unreachable: %q", e.Actual)
	}
	absent(t, items, inProgress.ID+"/payout")

	// No item ever carries bank details.
	var leaked int64
	h.db.Model(&store.ReconciliationItem{}).Where("expected LIKE ? OR actual LIKE ?", "%0123456789%", "%0123456789%").Count(&leaked)
	if leaked != 0 {
		t.Errorf("%d items mention a bank account", leaked)
	}
}

func TestWithoutAPayoutProviderThePayoutCheckDoesNotRun(t *testing.T) {
	h := newHarness(t)
	h.engine.Payouts = nil
	o := h.order(1, store.Order{Status: store.StateSettledInTreasury, FinancialOutcome: "disbursed"})
	_, items := h.cycle()
	absent(t, items, o.ID+"/payout")
}

// attempts records an order's payout attempts, numbered from 1, as the payout
// worker writes them.
func (h *harness) attempts(orderID string, list ...store.PayoutAttempt) {
	h.t.Helper()
	for i := range list {
		list[i].OrderID, list[i].Number = orderID, i+1
		if err := h.db.Create(&list[i]).Error; err != nil {
			h.t.Fatalf("create attempt: %v", err)
		}
	}
}

func sent(ref, outcome string) store.PayoutAttempt {
	return store.PayoutAttempt{Reference: ref, Outcome: outcome, RetrySafe: outcome == store.PayoutFailed}
}

func inducedAttempt(ref string) store.PayoutAttempt {
	return store.PayoutAttempt{Reference: ref, Outcome: store.PayoutFailed, RetrySafe: true, Induced: true, Reason: store.InducedReason}
}

// With PAYOUT_NEW_REFERENCE_ON_RETRY a retry is a new payout under its own
// reference, so every reference the order sent is asked about. An earlier one
// that paid as well is the merchant paid twice.
func TestRetriedPayoutsAreCheckedUnderEveryReference(t *testing.T) {
	h := newHarness(t)
	paidOrder := func(n int) store.Order {
		return h.order(n, store.Order{Status: store.StateSettledInTreasury, FinancialOutcome: "disbursed",
			AmountNGN: 10000, PayoutReference: "lq_" + strconv.Itoa(n), PayoutProvider: "nomba"})
	}
	refundedOrder := func(n int) store.Order {
		return h.order(n, store.Order{Status: store.StateRefunded, FinancialOutcome: "refunded"})
	}
	failedAt := func(ref string) payout.Result {
		return payout.Result{Status: payout.StatusFailed, Reference: ref, Provider: "nomba"}
	}
	paidAt := func(ref string) payout.Result {
		return payout.Result{Status: payout.StatusPaid, Reference: ref, Provider: "nomba", AmountNGN: ngn(10000)}
	}

	retried := paidOrder(1)
	h.attempts(retried.ID, sent(retried.ID, store.PayoutFailed), sent(retried.ID+"-p2", store.PayoutPaid))
	h.payouts.results[retried.ID] = failedAt("lq_1a")
	h.payouts.results[retried.ID+"-p2"] = paidAt("lq_1")

	paidTwice := paidOrder(2)
	h.attempts(paidTwice.ID, sent(paidTwice.ID, store.PayoutFailed), sent(paidTwice.ID+"-p2", store.PayoutPaid))
	h.payouts.results[paidTwice.ID] = paidAt("lq_2a")
	h.payouts.results[paidTwice.ID+"-p2"] = paidAt("lq_2")

	earlierGoing := paidOrder(3)
	h.attempts(earlierGoing.ID, sent(earlierGoing.ID, store.PayoutFailed), sent(earlierGoing.ID+"-p2", store.PayoutPaid))
	h.payouts.results[earlierGoing.ID] = payout.Result{Status: payout.StatusQueued, Reference: "lq_3a", Provider: "nomba"}
	h.payouts.results[earlierGoing.ID+"-p2"] = paidAt("lq_3")

	// A transient induced failure: attempt 1 never reached Linq.
	inducedThenPaid := paidOrder(4)
	h.attempts(inducedThenPaid.ID, inducedAttempt(inducedThenPaid.ID), sent(inducedThenPaid.ID+"-p2", store.PayoutPaid))
	h.payouts.results[inducedThenPaid.ID+"-p2"] = paidAt("lq_4")

	// The switch off: every attempt reused the order ID, which is one payout.
	switchOff := refundedOrder(5)
	h.attempts(switchOff.ID, sent(switchOff.ID, store.PayoutFailed), sent(switchOff.ID, store.PayoutFailed), sent(switchOff.ID, store.PayoutFailed))
	h.payouts.results[switchOff.ID] = failedAt("lq_5")

	// A terminal induced failure: nothing was ever sent.
	terminal := refundedOrder(6)
	h.attempts(terminal.ID, inducedAttempt(terminal.ID), inducedAttempt(terminal.ID+"-p2"), inducedAttempt(terminal.ID+"-p3"))

	refundedRetries := refundedOrder(7)
	h.attempts(refundedRetries.ID, sent(refundedRetries.ID, store.PayoutFailed),
		sent(refundedRetries.ID+"-p2", store.PayoutFailed), sent(refundedRetries.ID+"-p3", store.PayoutFailed))
	for _, ref := range []string{refundedRetries.ID, refundedRetries.ID + "-p2", refundedRetries.ID + "-p3"} {
		h.payouts.results[ref] = failedAt("lq_7")
	}

	refundedButPaid := refundedOrder(8)
	h.attempts(refundedButPaid.ID, sent(refundedButPaid.ID, store.PayoutFailed),
		sent(refundedButPaid.ID+"-p2", store.PayoutFailed), sent(refundedButPaid.ID+"-p3", store.PayoutFailed))
	h.payouts.results[refundedButPaid.ID] = failedAt("lq_8a")
	h.payouts.results[refundedButPaid.ID+"-p2"] = paidAt("lq_8b")
	h.payouts.results[refundedButPaid.ID+"-p3"] = failedAt("lq_8c")

	unreachable := paidOrder(9)
	h.attempts(unreachable.ID, sent(unreachable.ID, store.PayoutFailed), sent(unreachable.ID+"-p2", store.PayoutPaid))
	h.payouts.results[unreachable.ID] = failedAt("lq_9a")
	h.payouts.fail[unreachable.ID+"-p2"] = errors.New("payout: connection refused")

	lastMissing := paidOrder(10)
	h.attempts(lastMissing.ID, sent(lastMissing.ID, store.PayoutFailed), sent(lastMissing.ID+"-p2", store.PayoutPaid))
	h.payouts.results[lastMissing.ID] = failedAt("lq_10a")

	_, items := h.cycle()

	ok := expect(t, items, retried.ID+"/payout", store.ReconcileOK)
	if want := "paid ₦10000.00 under ord_001-p2, payout lq_1 via nomba; nothing paid under ord_001"; ok.Expected != want {
		t.Errorf("retried expected %q, want %q", ok.Expected, want)
	}
	if want := "ord_001: failed, amount not reported, payout lq_1a via nomba; ord_001-p2: paid, ₦10000.00, payout lq_1 via nomba"; ok.Actual != want {
		t.Errorf("retried actual %q, want %q", ok.Actual, want)
	}
	if m := expect(t, items, paidTwice.ID+"/payout", store.ReconcileMismatch); !strings.Contains(m.Actual, "(also paid under ord_002: the merchant may have been paid twice)") {
		t.Errorf("paid twice: %q", m.Actual)
	}
	if m := expect(t, items, earlierGoing.ID+"/payout", store.ReconcileMismatch); !strings.Contains(m.Actual, "(ord_003 still in progress)") {
		t.Errorf("earlier still going: %q", m.Actual)
	}
	if ok := expect(t, items, inducedThenPaid.ID+"/payout", store.ReconcileOK); ok.Expected != "paid ₦10000.00, payout lq_4 via nomba; 1 induced attempt, never sent" {
		t.Errorf("induced then paid: %q", ok.Expected)
	}
	if ok := expect(t, items, switchOff.ID+"/payout", store.ReconcileOK); ok.Expected != "no NGN paid: the payout failed and the USDC was refunded" {
		t.Errorf("switch off: %q", ok.Expected)
	}
	if ok := expect(t, items, terminal.ID+"/payout", store.ReconcileOK); ok.Expected != "no NGN paid: the payout failed and the USDC was refunded; 3 induced attempts, never sent" ||
		ok.Actual != "the Linq backend has no payout for this order" {
		t.Errorf("terminal induced: %+v", ok)
	}
	if ok := expect(t, items, refundedRetries.ID+"/payout", store.ReconcileOK); ok.Expected != "no NGN paid under ord_007, ord_007-p2, ord_007-p3: the payout failed and the USDC was refunded" {
		t.Errorf("refunded retries: %q", ok.Expected)
	}
	if m := expect(t, items, refundedButPaid.ID+"/payout", store.ReconcileMismatch); !strings.HasSuffix(m.Actual, "(ord_008-p2 paid, but the USDC was refunded)") {
		t.Errorf("refunded but paid: %q", m.Actual)
	}
	if e := expect(t, items, unreachable.ID+"/payout", store.ReconcileError); e.Actual != "could not check: ord_009-p2: payout: connection refused" {
		t.Errorf("unreachable: %q", e.Actual)
	}
	if m := expect(t, items, lastMissing.ID+"/payout", store.ReconcileMismatch); m.Actual != "ord_010: failed, amount not reported, payout lq_10a via nomba; ord_010-p2: no payout (no payout under ord_010-p2)" {
		t.Errorf("last reference unknown to Linq: %q", m.Actual)
	}

	// Each reference sent is asked about once; an induced attempt was never
	// sent, so it is not asked about, unless nothing else was sent at all.
	asked := map[string]int{}
	for _, ref := range h.payouts.asked {
		asked[ref]++
	}
	for _, ref := range []string{inducedThenPaid.ID, terminal.ID + "-p2", terminal.ID + "-p3"} {
		if asked[ref] != 0 {
			t.Errorf("asked about %s, which was never sent", ref)
		}
	}
	if asked[switchOff.ID] != 1 || asked[terminal.ID] != 1 || asked[refundedRetries.ID+"-p3"] != 1 {
		t.Errorf("asked: %v", asked)
	}
}

// An order's payout is checked whole or not at all, and never over budget.
func TestAPayoutIsCheckedWholeOrNotAtAll(t *testing.T) {
	h := newHarness(t)
	h.engine.PayoutCalls = 2
	older := h.order(1, store.Order{Status: store.StateRefunded, FinancialOutcome: "refunded"})
	newer := h.order(2, store.Order{Status: store.StateRefunded, FinancialOutcome: "refunded"})
	h.attempts(newer.ID, sent(newer.ID, store.PayoutFailed), sent(newer.ID+"-p2", store.PayoutFailed), sent(newer.ID+"-p3", store.PayoutFailed))

	_, first := h.cycle()
	absent(t, first, newer.ID+"/payout")
	expect(t, first, older.ID+"/payout", store.ReconcileOK)
	if h.payouts.calls != 1 {
		t.Errorf("first cycle asked %d times, want 1: the three-reference order waits", h.payouts.calls)
	}

	h.engine.PayoutCalls = 3
	_, second := h.cycle()
	expect(t, second, newer.ID+"/payout", store.ReconcileOK)
	if h.payouts.calls != 4 {
		t.Errorf("after the second cycle %d calls, want 4", h.payouts.calls)
	}
}

// resolve writes a note on a cycle's item, as an admin would.
func (h *harness) resolve(item store.ReconciliationItem, note string) {
	h.t.Helper()
	if _, err := store.ResolveReconciliationItem(h.db, item.ID, note, "ify", h.clock); err != nil {
		h.t.Fatalf("resolve %s/%s: %v", item.OrderID, item.CheckName, err)
	}
}

// A note covers the answer it explained. A check whose answer can change keeps
// running, records nothing while the answer stays the one explained, and
// records a new mismatch when it changes. (Tanta's review of #6.)
func TestAResolutionCoversOnlyTheAnswerItExplained(t *testing.T) {
	h := newHarness(t)

	// A refunded order whose payout is still going, explained as pending.
	pending := h.order(1, store.Order{Status: store.StateRefunded, FinancialOutcome: "refunded"})
	h.payouts.results[pending.ID] = payout.Result{Status: payout.StatusQueued, Reference: "lq_1", Provider: "nomba"}
	// A refunded order's account still open, explained as awaiting a reclaim.
	lingering := h.order(2, store.Order{Status: store.StateRefunded, FinancialOutcome: "refunded"})
	h.payouts.results[lingering.ID] = payout.Result{Status: payout.StatusFailed}
	h.ledger.holds(lingering.DepositAddress, 0)
	// A payout the Linq backend could not be asked about.
	unasked := h.order(3, store.Order{Status: store.StateRefunded, FinancialOutcome: "refunded"})
	h.payouts.fail[unasked.ID] = errors.New("payout: connection refused")
	// An open account holding funds its order does not know about.
	open := h.order(4, store.Order{Status: store.StateAwaitingDeposit})
	h.ledger.holds(open.DepositAddress, 0.05)

	_, first := h.cycle()
	h.resolve(expect(t, first, pending.ID+"/payout", store.ReconcileMismatch), "Nomba pending, watching")
	h.resolve(expect(t, first, lingering.ID+"/account_closed", store.ReconcileMismatch), "Reclaim queued")
	h.resolve(expect(t, first, unasked.ID+"/payout", store.ReconcileError), "Linq was down")
	h.resolve(expect(t, first, open.ID+"/open_account", store.ReconcileMismatch), "Payer testing the address")

	// The same answers: each check runs again and records nothing, except the
	// error, which was never an answer and is asked again.
	calls := h.payouts.calls
	h.payouts.fail = map[string]error{}
	h.payouts.results[unasked.ID] = payout.Result{Status: payout.StatusFailed}
	_, second := h.cycle()
	absent(t, second, pending.ID+"/payout")
	absent(t, second, lingering.ID+"/account_closed")
	absent(t, second, open.ID+"/open_account")
	expect(t, second, unasked.ID+"/payout", store.ReconcileOK)
	if h.payouts.calls-calls != 2 {
		t.Errorf("payout calls in the second cycle: %d, want 2: the resolved payout is still asked about", h.payouts.calls-calls)
	}
	for key, item := range second {
		// These refunded orders record no refund transaction, which is its own
		// unexplained mismatch every cycle.
		if item.Result == store.ReconcileMismatch && item.CheckName != CheckRefundTx {
			t.Errorf("%s recorded again: %q", key, item.Actual)
		}
	}

	// Nomba pays after all, and the open account receives more: new answers,
	// so new mismatches, needing their own notes. The account is merged, which
	// is a pass and settles that check.
	h.payouts.results[pending.ID] = payout.Result{Status: payout.StatusPaid, Reference: "lq_1", Provider: "nomba"}
	h.ledger.holds(open.DepositAddress, 0.08)
	delete(h.ledger.accounts, lingering.DepositAddress)
	_, third := h.cycle()
	paid := expect(t, third, pending.ID+"/payout", store.ReconcileMismatch)
	if paid.ResolvedAt != nil || !strings.Contains(paid.Actual, "paid, but the USDC was refunded") {
		t.Errorf("the payout that paid after its note: %+v", paid)
	}
	if m := expect(t, third, open.ID+"/open_account", store.ReconcileMismatch); m.ResolvedAt != nil || !strings.Contains(m.Actual, "0.08") {
		t.Errorf("the open account's new answer: %+v", m)
	}
	expect(t, third, lingering.ID+"/account_closed", store.ReconcileOK)

	_, fourth := h.cycle()
	absent(t, fourth, lingering.ID+"/account_closed")
	absent(t, fourth, unasked.ID+"/payout")
}

// Open accounts get half the Horizon budget first, then whatever the cycle
// leaves over, and carry on from where the last cycle stopped. A long list of
// them can't starve the finished orders, and every one gets its turn. (Tanta's
// review of #6.)
func TestOpenAccountsShareTheBudgetAndTakeTurns(t *testing.T) {
	h := newHarness(t)
	h.engine.CheckSponsored = false
	h.engine.HorizonCalls = 4
	for n := 1; n <= 6; n++ {
		o := h.order(n, store.Order{Status: store.StateAwaitingDeposit})
		h.ledger.holds(o.DepositAddress, 0)
	}
	done := h.order(10, settled(0.08, 10000))
	h.ledger.pays("dep", move(payer, done.DepositAddress, 0.08))
	h.ledger.pays("swp", move(done.DepositAddress, treasury, 0.08))
	h.payouts.results[done.ID] = payout.Result{Status: payout.StatusPaid, Reference: "lq_payout_1", Provider: "nomba"}

	openChecked := func(items map[string]store.ReconciliationItem) string {
		var ids []string
		for _, item := range items {
			if item.CheckName == CheckOpenAccount {
				ids = append(ids, strings.TrimPrefix(item.OrderID, "ord_"))
			}
		}
		sort.Strings(ids)
		return strings.Join(ids, ",")
	}
	cycle := func() map[string]store.ReconciliationItem {
		before := h.ledger.calls
		_, items := h.cycle()
		if spent := h.ledger.calls - before; spent > 4 {
			t.Errorf("a cycle spent %d Horizon calls, budget 4", spent)
		}
		return items
	}

	// Half the budget to open accounts; the finished order takes the rest.
	first := cycle()
	if got := openChecked(first); got != "001,002" {
		t.Errorf("first cycle checked open accounts %s, want 001,002", got)
	}
	expect(t, first, done.ID+"/deposit_tx", store.ReconcileOK)
	expect(t, first, done.ID+"/sweep_tx", store.ReconcileOK)

	// The next ones, and one more with what the finished order left over.
	second := cycle()
	if got := openChecked(second); got != "003,004,005" {
		t.Errorf("second cycle checked open accounts %s, want 003,004,005", got)
	}
	expect(t, second, done.ID+"/account_closed", store.ReconcileOK)

	// Nothing else needs the budget, so open accounts get all of it, wrapping
	// round to the start.
	third := cycle()
	if got := openChecked(third); got != "001,002,003,006" {
		t.Errorf("third cycle checked open accounts %s, want 001,002,003,006", got)
	}
}

// On the testnet deployment the payout provider is payout.Fake. Its record is
// the payout attempts, so a paid order and a refunded one both reconcile.
func TestTheFakeProviderReconciles(t *testing.T) {
	h := newHarness(t)
	h.engine.Payouts = payout.Fake{DB: h.db}
	paid := h.order(1, store.Order{Status: store.StateSettledInTreasury, FinancialOutcome: "disbursed",
		AmountNGN: 10000, PayoutReference: "fake_ord_001-p2", PayoutProvider: payout.FakeProvider})
	h.attempts(paid.ID, inducedAttempt(paid.ID), sent(paid.ID+"-p2", store.PayoutPaid))
	refunded := h.order(2, store.Order{Status: store.StateRefunded, FinancialOutcome: "refunded"})
	h.attempts(refunded.ID, inducedAttempt(refunded.ID), inducedAttempt(refunded.ID+"-p2"), inducedAttempt(refunded.ID+"-p3"))

	_, items := h.cycle()
	if ok := expect(t, items, paid.ID+"/payout", store.ReconcileOK); ok.Actual != "paid, ₦10000.00, payout fake_ord_001-p2 via fake" {
		t.Errorf("paid: %q", ok.Actual)
	}
	expect(t, items, refunded.ID+"/payout", store.ReconcileOK)
}

// A check that passed has its answer; a mismatch is looked at again until
// someone resolves it.
func TestPassedAndResolvedChecksAreNotRepeated(t *testing.T) {
	h := newHarness(t)
	o := h.order(1, settled(0.08, 10000))
	h.ledger.pays("dep", move(payer, o.DepositAddress, 0.08))
	h.ledger.pays("swp", move(o.DepositAddress, stranger, 0.08))
	h.payouts.results[o.ID] = payout.Result{Status: payout.StatusPaid, Reference: "lq_payout_1", Provider: "nomba"}
	open := h.order(2, store.Order{Status: store.StateAwaitingDeposit})
	h.ledger.holds(open.DepositAddress, 0)

	_, first := h.cycle()
	expect(t, first, o.ID+"/deposit_tx", store.ReconcileOK)
	expect(t, first, o.ID+"/sweep_tx", store.ReconcileOK)
	mismatch := expect(t, first, o.ID+"/sweep_to_treasury", store.ReconcileMismatch)
	expect(t, first, o.ID+"/account_closed", store.ReconcileOK)
	expect(t, first, o.ID+"/payout", store.ReconcileOK)

	_, second := h.cycle()
	for _, check := range []string{"deposit_tx", "sweep_tx", "account_closed", "payout"} {
		absent(t, second, o.ID+"/"+check)
	}
	expect(t, second, o.ID+"/sweep_to_treasury", store.ReconcileMismatch)
	// Open accounts are looked at every cycle: their answer can change.
	expect(t, second, open.ID+"/open_account", store.ReconcileOK)

	if _, err := store.ResolveReconciliationItem(h.db, mismatch.ID, "Induced for the demo", "ify", h.clock); err != nil {
		t.Fatal(err)
	}
	calls := h.ledger.calls
	_, third := h.cycle()
	absent(t, third, o.ID+"/sweep_to_treasury")
	// A resolved transaction check is done: not even asked about again. The
	// two calls are the open account and the sponsor's account list.
	if h.ledger.calls-calls != 2 {
		t.Errorf("third cycle made %d Horizon calls, want 2", h.ledger.calls-calls)
	}
}

// A backlog bigger than one cycle's budget is worked through over several,
// newest first, without ever going over the budget.
func TestABacklogIsSpreadOverCycles(t *testing.T) {
	h := newHarness(t)
	h.engine.HorizonCalls = 4
	for n := 1; n <= 10; n++ {
		hash := "dep" + strconv.Itoa(n)
		h.order(n, store.Order{Status: store.StateDisbursed, FinancialOutcome: "disbursed", AmountUSDC: 1, DepositTxHash: hash, DepositAddress: depositAccount(n)})
		h.ledger.pays(hash, move(payer, depositAccount(n), 1))
	}
	h.engine.Payouts = nil
	// Disbursed orders' accounts are open, and checked first: give them no
	// budget by making none open.
	h.db.Model(&store.Order{}).Where("1 = 1").Update("status", store.StateSettledInTreasury)
	h.db.Model(&store.Order{}).Where("1 = 1").Update("reserves_reclaimed", false)

	var order []string
	for i := 0; i < 6; i++ {
		before := h.ledger.calls
		_, items := h.cycle()
		if spent := h.ledger.calls - before; spent > 4 {
			t.Fatalf("cycle %d made %d Horizon calls, over its budget of 4", i+1, spent)
		}
		var ids []string
		for key, item := range items {
			if item.CheckName == CheckDepositTx {
				ids = append(ids, key)
			}
		}
		sort.Sort(sort.Reverse(sort.StringSlice(ids)))
		order = append(order, ids...)
	}
	var deposits int64
	h.db.Model(&store.ReconciliationItem{}).Where("check_name = ?", CheckDepositTx).Count(&deposits)
	if deposits != 10 {
		t.Errorf("%d deposit checks after six cycles, want all 10", deposits)
	}
	if len(order) == 0 || order[0] != "ord_010/deposit_tx" {
		t.Errorf("newest first: %v", order)
	}
}

// Horizon refusing for its rate limit stops the cycle's Horizon checks, and
// the cycle says so; the payout checks still run.
func TestARateLimitStopsHorizonChecksForTheCycle(t *testing.T) {
	h := newHarness(t)
	a := h.order(2, store.Order{Status: store.StateSettledInTreasury, FinancialOutcome: "disbursed", DepositTxHash: "dep2", AmountUSDC: 1})
	b := h.order(1, store.Order{Status: store.StateSettledInTreasury, FinancialOutcome: "disbursed", DepositTxHash: "dep1", AmountUSDC: 1})
	h.ledger.fail["dep2"] = fmt.Errorf("%w: Rate Limit Exceeded", stellar.ErrRateLimited)
	h.payouts.results[a.ID] = payout.Result{Status: payout.StatusPaid}
	h.payouts.results[b.ID] = payout.Result{Status: payout.StatusPaid}

	c, items := h.cycle()
	expect(t, items, a.ID+"/deposit_tx", store.ReconcileError)
	absent(t, items, b.ID+"/deposit_tx")
	absent(t, items, b.ID+"/account_closed")
	expect(t, items, a.ID+"/payout", store.ReconcileOK)
	expect(t, items, b.ID+"/payout", store.ReconcileOK)
	if h.ledger.calls != 1 {
		t.Errorf("%d Horizon calls, want none after the refusal", h.ledger.calls)
	}
	if c.Error == "" || c.FinishedAt == nil {
		t.Errorf("cycle should finish and say it stopped early: %+v", c)
	}
}

// A rate-limit refusal on an open account stops the open accounts too, in
// both of the cycle's passes over them.
func TestARateLimitStopsOpenAccountChecks(t *testing.T) {
	h := newHarness(t)
	h.engine.CheckSponsored = false
	for n := 1; n <= 4; n++ {
		o := h.order(n, store.Order{Status: store.StateAwaitingDeposit})
		h.ledger.holds(o.DepositAddress, 0)
	}
	h.ledger.fail[depositAccount(1)] = fmt.Errorf("%w: Rate Limit Exceeded", stellar.ErrRateLimited)

	c, items := h.cycle()
	expect(t, items, "ord_001/open_account", store.ReconcileError)
	for n := 2; n <= 4; n++ {
		absent(t, items, fmt.Sprintf("ord_%03d/open_account", n))
	}
	if h.ledger.calls != 1 || c.Error == "" {
		t.Errorf("%d Horizon calls, want 1; cycle error %q", h.ledger.calls, c.Error)
	}
}

// A cycle that fails partway keeps what it checked and records why it stopped.
func TestACycleThatFailsPartwayIsStillRecorded(t *testing.T) {
	h := newHarness(t)
	checked := h.order(2, store.Order{Status: store.StateSettledInTreasury, FinancialOutcome: "disbursed"})
	h.payouts.results[checked.ID] = payout.Result{Status: payout.StatusPaid}
	boom := h.order(1, store.Order{Status: store.StateSettledInTreasury, FinancialOutcome: "disbursed"})
	h.payouts.panicOn = boom.ID

	c, err := h.engine.RunCycle(context.Background())
	if err != nil {
		t.Fatalf("a failing cycle should still be recorded: %v", err)
	}
	var saved store.ReconciliationCycle
	h.db.First(&saved, "id = ?", c.ID)
	if saved.FinishedAt == nil || saved.Error != "panic: payout client blew up" {
		t.Errorf("cycle record: %+v", saved)
	}
	items := h.items(c.ID)
	expect(t, items, checked.ID+"/payout", store.ReconcileOK)
	if saved.Matched == 0 || saved.OrdersChecked == 0 {
		t.Errorf("counts lost: %+v", saved)
	}
}

func TestACycleStopsCleanlyWhenTheWorkerStops(t *testing.T) {
	h := newHarness(t)
	h.order(1, store.Order{Status: store.StateAwaitingDeposit})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, err := h.engine.RunCycle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c.Error != "stopped early: the worker is shutting down" || c.FinishedAt == nil {
		t.Errorf("cycle: %+v", c)
	}
}

func TestCycleCountsAddUp(t *testing.T) {
	h := newHarness(t)
	o := h.order(1, settled(0.08, 10000))
	h.ledger.pays("dep", move(payer, o.DepositAddress, 0.08))
	h.ledger.pays("swp", move(o.DepositAddress, treasury, 0.08))
	h.payouts.results[o.ID] = payout.Result{Status: payout.StatusPaid, Reference: "lq_payout_1", Provider: "nomba"}
	open := h.order(2, store.Order{Status: store.StateAwaitingDeposit})
	h.ledger.holds(open.DepositAddress, 3)
	h.engine.CheckSponsored = false

	c, items := h.cycle()
	var saved store.ReconciliationCycle
	h.db.First(&saved, "id = ?", c.ID)
	if saved.OrdersChecked != 2 || saved.AccountsChecked != 2 || saved.Matched != 5 || saved.Mismatches != 1 || len(items) != 6 {
		t.Errorf("counts %+v from %d items", saved, len(items))
	}
	if !saved.StartedAt.Equal(time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)) || saved.FinishedAt == nil || saved.Error != "" {
		t.Errorf("times %+v", saved)
	}
	if n, _ := store.CompletedCycles(h.db); n != 1 {
		t.Errorf("completed cycles %d", n)
	}
}

// The engine only reads: a cycle full of mismatches leaves every order exactly
// as it was.
func TestACycleChangesNoOrder(t *testing.T) {
	h := newHarness(t)
	h.order(1, store.Order{Status: store.StateAwaitingDeposit})
	h.ledger.holds(depositAccount(1), 9)
	h.order(2, store.Order{Status: store.StateSettledInTreasury, FinancialOutcome: "disbursed", DepositTxHash: "nowhere", SweepTxHash: "nope", AmountUSDC: 1})
	h.ledger.holds(depositAccount(2), 1)
	h.order(3, store.Order{Status: store.StateRefunded, FinancialOutcome: "refunded", RefundTxHash: "gone", AmountUSDC: 1})
	h.payouts.results["ord_003"] = payout.Result{Status: payout.StatusPaid}

	var before []store.Order
	h.db.Order("id").Find(&before)
	_, items := h.cycle()
	var after []store.Order
	h.db.Order("id").Find(&after)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("orders changed:\nbefore %+v\nafter  %+v", before, after)
	}
	var mismatches int
	for _, item := range items {
		if item.Result == store.ReconcileMismatch {
			mismatches++
		}
	}
	if mismatches < 5 {
		t.Errorf("expected plenty of mismatches, got %d", mismatches)
	}
}
