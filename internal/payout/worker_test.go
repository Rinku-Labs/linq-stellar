package payout

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/Rinku-Labs/linq-stellar/internal/store"
)

// fakeProvider answers per reference. A reference with no script is unknown to
// it: Pay errors as a lost response would, and Status errors as a 404 would.
type fakeProvider struct {
	pay    map[string]Result
	status map[string]Result
	paid   []string // references Pay was called with, in order
	asked  []string // references Status was called with, in order
}

func newFake() *fakeProvider {
	return &fakeProvider{pay: map[string]Result{}, status: map[string]Result{}}
}

func (f *fakeProvider) Pay(r Request) (Result, error) {
	f.paid = append(f.paid, r.Reference)
	result, ok := f.pay[r.Reference]
	if !ok {
		return Result{}, errors.New("payout: connection reset")
	}
	return result, nil
}

func (f *fakeProvider) Status(reference string) (Result, error) {
	f.asked = append(f.asked, reference)
	result, ok := f.status[reference]
	if !ok {
		return Result{}, errors.New("payout: 404 no payout for that reference")
	}
	return result, nil
}

// both scripts the same answer for Pay and Status.
func (f *fakeProvider) both(reference string, r Result) {
	f.pay[reference] = r
	f.status[reference] = r
}

var (
	paid           = Result{Status: StatusPaid, Reference: "nomba-1", Provider: "nomba"}
	queued         = Result{Status: StatusQueued}
	failedSafe     = Result{Status: StatusFailed, FailureReason: "failed", RetrySafe: true}
	failedUnproven = Result{Status: StatusFailed, FailureReason: "failed"}
)

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:payout_%s?mode=memory&cache=shared", name)),
		&gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		if sql, err := db.DB(); err == nil {
			_ = sql.Close()
		}
	})
	return db
}

// queuedOrder seeds an order whose deposit has landed, for an integrator that
// may or may not allow induced failures.
func queuedOrder(t *testing.T, db *gorm.DB, allowInjection bool) store.Order {
	t.Helper()
	integrator := store.Integrator{ID: "int_test", Name: "Linq test", AllowFailureInjection: allowInjection}
	if err := db.Create(&integrator).Error; err != nil {
		t.Fatalf("create integrator: %v", err)
	}
	now := time.Now().UTC()
	order := store.Order{
		ID: "ord_1", IntegratorID: integrator.ID, IdempotencyKey: "idem-1",
		Status: store.StatePayoutQueued, AmountNGN: 1500, AmountUSDC: 1, Currency: "NGN",
		BankCode: "058", BankAccount: "0123456789", AccountName: "Test",
		DepositDeadline: now.Add(time.Hour), DepositedAt: &now,
	}
	if err := db.Create(&order).Error; err != nil {
		t.Fatalf("create order: %v", err)
	}
	return order
}

func newWorker(db *gorm.DB, p Provider, newRefs bool) *Worker {
	return &Worker{
		DB: db, Provider: p, NewReferenceOnRetry: newRefs,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// pass runs one sweep, ignoring any backoff the last one set: the tests are
// about what each pass decides, not how long it waits.
func pass(t *testing.T, w *Worker) {
	t.Helper()
	if err := w.DB.Model(&store.Order{}).Where("1 = 1").Update("payout_after", nil).Error; err != nil {
		t.Fatalf("clear backoff: %v", err)
	}
	w.sweep(context.Background())
}

func reload(t *testing.T, db *gorm.DB, id string) store.Order {
	t.Helper()
	var order store.Order
	if err := db.First(&order, "id = ?", id).Error; err != nil {
		t.Fatalf("reload order: %v", err)
	}
	return order
}

func attempts(t *testing.T, db *gorm.DB, id string) []store.PayoutAttempt {
	t.Helper()
	list, err := store.PayoutAttempts(db, id)
	if err != nil {
		t.Fatalf("load attempts: %v", err)
	}
	return list
}

func reasons(t *testing.T, db *gorm.DB, id string) []string {
	t.Helper()
	events, err := store.StatusHistory(db, id)
	if err != nil {
		t.Fatalf("load history: %v", err)
	}
	var out []string
	for _, e := range events {
		if e.Reason != "" {
			out = append(out, e.To+": "+e.Reason)
		}
	}
	return out
}

func equal(a, b []string) bool {
	return strings.Join(a, ",") == strings.Join(b, ",")
}

func TestPayoutReferenceSequence(t *testing.T) {
	cases := []struct {
		n       int
		newRefs bool
		want    string
	}{
		{1, true, "ord_1"},
		{2, true, "ord_1-p2"},
		{3, true, "ord_1-p3"},
		{1, false, "ord_1"},
		{2, false, "ord_1"},
		{3, false, "ord_1"},
	}
	for _, c := range cases {
		if got := store.PayoutReference("ord_1", c.n, c.newRefs); got != c.want {
			t.Errorf("PayoutReference(ord_1, %d, %v) = %q, want %q", c.n, c.newRefs, got, c.want)
		}
	}
}

func TestARetryAfterAConfirmedRetrySafeFailureSendsANewReferenceAndPays(t *testing.T) {
	db := newTestDB(t)
	order := queuedOrder(t, db, false)
	p := newFake()
	p.both("ord_1", failedSafe)
	p.both("ord_1-p2", paid)
	w := newWorker(db, p, true)

	pass(t, w)
	if got := reload(t, db, order.ID).Status; got != store.StatePayoutQueued {
		t.Fatalf("after the failure the order is %q, want payout_queued", got)
	}
	pass(t, w)

	if !equal(p.paid, []string{"ord_1", "ord_1-p2"}) {
		t.Errorf("Pay was sent %v, want [ord_1 ord_1-p2]", p.paid)
	}
	got := reload(t, db, order.ID)
	if got.Status != store.StateSweepQueued || got.FinancialOutcome != "disbursed" {
		t.Errorf("order ended %q / %q, want sweep_queued / disbursed", got.Status, got.FinancialOutcome)
	}
	list := attempts(t, db, order.ID)
	if len(list) != 2 ||
		list[0].Reference != "ord_1" || list[0].Outcome != store.PayoutFailed ||
		list[1].Reference != "ord_1-p2" || list[1].Outcome != store.PayoutPaid {
		t.Errorf("attempts = %+v, want ord_1 failed then ord_1-p2 paid", list)
	}
}

func TestAFailureFromPayAloneIsCheckedBeforeANewReference(t *testing.T) {
	db := newTestDB(t)
	order := queuedOrder(t, db, false)
	p := newFake()
	// The submission says failed, but the provider's own record says the
	// payout is still moving.
	p.pay["ord_1"] = failedSafe
	p.status["ord_1"] = queued
	w := newWorker(db, p, true)

	pass(t, w)
	pass(t, w)

	if !equal(p.paid, []string{"ord_1", "ord_1"}) {
		t.Errorf("Pay was sent %v, want ord_1 twice and no new reference", p.paid)
	}
	if got := reload(t, db, order.ID); got.Status != store.StatePayoutQueued || got.PayoutAttempts != 0 {
		t.Errorf("order is %q with %d failed attempts, want payout_queued with 0", got.Status, got.PayoutAttempts)
	}
	if list := attempts(t, db, order.ID); len(list) != 1 || list[0].Outcome != store.PayoutPending {
		t.Errorf("attempts = %+v, want one pending", list)
	}
}

func TestAFailureNotConfirmedRetrySafeGoesToAPersonNotARetryOrRefund(t *testing.T) {
	db := newTestDB(t)
	order := queuedOrder(t, db, false)
	p := newFake()
	p.both("ord_1", failedUnproven)
	w := newWorker(db, p, true)

	pass(t, w)
	pass(t, w)

	if !equal(p.paid, []string{"ord_1"}) {
		t.Errorf("Pay was sent %v, want only ord_1", p.paid)
	}
	got := reload(t, db, order.ID)
	if got.Status != store.StateFailed || got.FinancialOutcome != "" {
		t.Errorf("order ended %q / %q, want failed with no financial outcome", got.Status, got.FinancialOutcome)
	}
}

func TestAnUnknownOutcomeNeverRefundsOrMovesToANewReference(t *testing.T) {
	for _, newRefs := range []bool{false, true} {
		t.Run(fmt.Sprintf("newRefs=%v", newRefs), func(t *testing.T) {
			db := newTestDB(t)
			order := queuedOrder(t, db, false)
			p := newFake() // nothing scripted: every call errors
			w := newWorker(db, p, newRefs)

			for i := 0; i < maxAttempts+3; i++ {
				pass(t, w)
			}

			for _, ref := range p.paid {
				if ref != "ord_1" {
					t.Fatalf("Pay was sent %q; an unknown outcome must not start a new reference", ref)
				}
			}
			got := reload(t, db, order.ID)
			if got.Status != store.StatePayoutQueued || got.PayoutAttempts != 0 || got.FinancialOutcome != "" {
				t.Errorf("order is %q, %d failed attempts, outcome %q; want payout_queued, 0, none",
					got.Status, got.PayoutAttempts, got.FinancialOutcome)
			}
		})
	}
}

func TestAnUnknownOutcomeBacksOff(t *testing.T) {
	db := newTestDB(t)
	order := queuedOrder(t, db, false)
	p := newFake()
	w := newWorker(db, p, true)

	pass(t, w)
	w.sweep(context.Background()) // no backoff cleared: must not be picked up

	if len(p.paid) != 1 {
		t.Errorf("Pay was called %d times, want 1 while the order is held back", len(p.paid))
	}
	if got := reload(t, db, order.ID); got.PayoutAfter == nil || !got.PayoutAfter.After(time.Now().UTC()) {
		t.Errorf("payout_after = %v, want a time in the future", got.PayoutAfter)
	}
}

func TestWithTheSwitchOffEveryAttemptReusesTheOrderIDAndThenRefunds(t *testing.T) {
	db := newTestDB(t)
	order := queuedOrder(t, db, false)
	p := newFake()
	p.both("ord_1", failedUnproven)
	w := newWorker(db, p, false)

	for i := 0; i < maxAttempts; i++ {
		pass(t, w)
	}

	if !equal(p.paid, []string{"ord_1", "ord_1", "ord_1"}) {
		t.Errorf("Pay was sent %v, want ord_1 three times", p.paid)
	}
	if got := reload(t, db, order.ID); got.Status != store.StateRefundQueued || got.PayoutAttempts != maxAttempts {
		t.Errorf("order is %q with %d attempts, want refund_queued with %d", got.Status, got.PayoutAttempts, maxAttempts)
	}
}

func TestATransientInducedFailureFailsOnceWithoutTheProviderThenPays(t *testing.T) {
	db := newTestDB(t)
	order := queuedOrder(t, db, true)
	if err := store.InducePayoutFailure(db, order.ID, store.InducedTransient); err != nil {
		t.Fatalf("induce: %v", err)
	}
	p := newFake()
	p.both("ord_1-p2", paid)
	w := newWorker(db, p, true)

	pass(t, w)
	if len(p.paid) != 0 || len(p.asked) != 0 {
		t.Fatalf("the provider was called (%v, %v); an induced attempt must not reach it", p.paid, p.asked)
	}
	pass(t, w)

	if !equal(p.paid, []string{"ord_1-p2"}) {
		t.Errorf("Pay was sent %v, want [ord_1-p2]", p.paid)
	}
	if got := reload(t, db, order.ID).Status; got != store.StateSweepQueued {
		t.Errorf("order ended %q, want sweep_queued", got)
	}
	list := attempts(t, db, order.ID)
	if len(list) != 2 || !list[0].Induced || list[0].Outcome != store.PayoutFailed || list[1].Induced {
		t.Errorf("attempts = %+v, want an induced failure then a real attempt", list)
	}
	if want := []string{"payout_queued: " + store.InducedReason}; !equal(reasons(t, db, order.ID), want) {
		t.Errorf("history reasons = %v, want %v", reasons(t, db, order.ID), want)
	}
}

func TestATerminalInducedFailureRefundsWithoutTheProvider(t *testing.T) {
	db := newTestDB(t)
	order := queuedOrder(t, db, true)
	if err := store.InducePayoutFailure(db, order.ID, store.InducedTerminal); err != nil {
		t.Fatalf("induce: %v", err)
	}
	p := newFake()
	w := newWorker(db, p, true)

	for i := 0; i < maxAttempts+1; i++ {
		pass(t, w)
	}

	if len(p.paid) != 0 || len(p.asked) != 0 {
		t.Errorf("the provider was called (%v, %v); induced attempts must not reach it", p.paid, p.asked)
	}
	if got := reload(t, db, order.ID).Status; got != store.StateRefundQueued {
		t.Errorf("order ended %q, want refund_queued", got)
	}
	if list := attempts(t, db, order.ID); len(list) != maxAttempts {
		t.Errorf("%d attempts recorded, want %d", len(list), maxAttempts)
	}
	want := []string{
		"payout_queued: " + store.InducedReason,
		"payout_queued: " + store.InducedReason,
		"refund_queued: " + store.InducedReason,
	}
	if got := reasons(t, db, order.ID); !equal(got, want) {
		t.Errorf("history reasons = %v, want %v", got, want)
	}
}

func TestInducingAFailureIsGated(t *testing.T) {
	t.Run("integrator without the flag", func(t *testing.T) {
		db := newTestDB(t)
		order := queuedOrder(t, db, false)
		if err := store.InducePayoutFailure(db, order.ID, store.InducedTerminal); !errors.Is(err, store.ErrFailureInjectionOff) {
			t.Errorf("err = %v, want ErrFailureInjectionOff", err)
		}
	})
	t.Run("unknown kind", func(t *testing.T) {
		db := newTestDB(t)
		order := queuedOrder(t, db, true)
		if err := store.InducePayoutFailure(db, order.ID, "sometimes"); !errors.Is(err, store.ErrUnknownInducedKind) {
			t.Errorf("err = %v, want ErrUnknownInducedKind", err)
		}
	})
	t.Run("unknown order", func(t *testing.T) {
		db := newTestDB(t)
		queuedOrder(t, db, true)
		if err := store.InducePayoutFailure(db, "ord_nope", store.InducedTerminal); !errors.Is(err, store.ErrOrderNotFound) {
			t.Errorf("err = %v, want ErrOrderNotFound", err)
		}
	})
	t.Run("only once", func(t *testing.T) {
		db := newTestDB(t)
		order := queuedOrder(t, db, true)
		if err := store.InducePayoutFailure(db, order.ID, store.InducedTransient); err != nil {
			t.Fatalf("first: %v", err)
		}
		if err := store.InducePayoutFailure(db, order.ID, store.InducedTerminal); !errors.Is(err, store.ErrFailureInjectionTooLate) {
			t.Errorf("second: err = %v, want ErrFailureInjectionTooLate", err)
		}
	})
	t.Run("not once a payout has started", func(t *testing.T) {
		db := newTestDB(t)
		order := queuedOrder(t, db, true)
		p := newFake()
		p.both("ord_1", queued)
		pass(t, newWorker(db, p, true))
		if err := store.InducePayoutFailure(db, order.ID, store.InducedTerminal); !errors.Is(err, store.ErrFailureInjectionTooLate) {
			t.Errorf("err = %v, want ErrFailureInjectionTooLate", err)
		}
	})
	t.Run("not on an order paid before attempts were recorded", func(t *testing.T) {
		db := newTestDB(t)
		order := queuedOrder(t, db, true)
		db.Model(&store.Order{}).Where("id = ?", order.ID).Update("payout_attempts", 1)
		if err := store.InducePayoutFailure(db, order.ID, store.InducedTerminal); !errors.Is(err, store.ErrFailureInjectionTooLate) {
			t.Errorf("err = %v, want ErrFailureInjectionTooLate", err)
		}
	})
}

func TestTurningTheSwitchOnMidRetryDoesNotSendANewReferenceForAnUnprovenFailure(t *testing.T) {
	db := newTestDB(t)
	order := queuedOrder(t, db, false)
	p := newFake()
	p.both("ord_1", failedUnproven)

	pass(t, newWorker(db, p, false)) // fails once with the switch off, requeued
	if got := reload(t, db, order.ID).Status; got != store.StatePayoutQueued {
		t.Fatalf("after one failure with the switch off the order is %q, want payout_queued", got)
	}
	pass(t, newWorker(db, p, true))

	if !equal(p.paid, []string{"ord_1"}) {
		t.Errorf("Pay was sent %v, want only ord_1", p.paid)
	}
	if got := reload(t, db, order.ID).Status; got != store.StateFailed {
		t.Errorf("order is %q, want failed for review", got)
	}
}
