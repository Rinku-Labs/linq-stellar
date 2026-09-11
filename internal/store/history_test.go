package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func historyDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:hist_%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func seedOrder(t *testing.T, db *gorm.DB, status string) *Order {
	t.Helper()
	o := &Order{ID: "ord_hist", IdempotencyKey: "idem_hist", Status: status}
	if err := db.Create(o).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	return o
}

// A transition that actually happened must leave a record. Without this the
// history is decorative.
func TestClaimRecordsTransition(t *testing.T) {
	db := historyDB(t)
	seedOrder(t, db, StateAwaitingDeposit)

	won, err := ClaimStatus(db, "ord_hist", StateAwaitingDeposit, StateDepositDetected)
	if err != nil || !won {
		t.Fatalf("claim: won=%v err=%v", won, err)
	}

	events, err := StatusHistory(db, "ord_hist")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(events))
	}
	if events[0].From != StateAwaitingDeposit || events[0].To != StateDepositDetected {
		t.Errorf("recorded %s->%s, want %s->%s",
			events[0].From, events[0].To, StateAwaitingDeposit, StateDepositDetected)
	}
	if events[0].At.IsZero() {
		t.Error("event has no timestamp, so nothing can be timed from it")
	}
}

// A claim that lost the race changed nothing, so it must not appear to have.
// A history showing moves that did not happen is worse than no history.
func TestLostClaimRecordsNothing(t *testing.T) {
	db := historyDB(t)
	seedOrder(t, db, StateAwaitingDeposit)

	if _, err := ClaimStatus(db, "ord_hist", StateAwaitingDeposit, StateDepositDetected); err != nil {
		t.Fatal(err)
	}
	// Second caller arrives late: the order has already moved on.
	won, err := ClaimStatus(db, "ord_hist", StateAwaitingDeposit, StateDepositDetected)
	if err != nil {
		t.Fatal(err)
	}
	if won {
		t.Fatal("second claim won, which defeats the point of claiming")
	}

	events, _ := StatusHistory(db, "ord_hist")
	if len(events) != 1 {
		t.Errorf("recorded %d events, want 1 — a lost claim was recorded as a move", len(events))
	}
}

// An illegal transition is refused, and must not be recorded either.
func TestIllegalTransitionRecordsNothing(t *testing.T) {
	db := historyDB(t)
	seedOrder(t, db, StateAwaitingDeposit)

	won, err := ClaimStatus(db, "ord_hist", StateAwaitingDeposit, StateSettledInTreasury)
	if err != nil {
		t.Fatal(err)
	}
	if won {
		t.Fatal("an illegal transition was allowed")
	}
	if events, _ := StatusHistory(db, "ord_hist"); len(events) != 0 {
		t.Errorf("recorded %d events for a refused transition, want 0", len(events))
	}
}

// The reason a retry happened has to survive, or the history shows an order
// bouncing between two states for no stated cause.
func TestRetryReasonIsRecorded(t *testing.T) {
	db := historyDB(t)
	seedOrder(t, db, StatePayoutQueued)

	if _, err := ClaimStatus(db, "ord_hist", StatePayoutQueued, StatePayoutProcessing); err != nil {
		t.Fatal(err)
	}
	const reason = "provider rejected: account not found"
	if err := ReleaseStatusBecause(db, "ord_hist", StatePayoutProcessing, StatePayoutQueued, reason); err != nil {
		t.Fatal(err)
	}

	events, _ := StatusHistory(db, "ord_hist")
	last := events[len(events)-1]
	if last.Reason != reason {
		t.Errorf("reason = %q, want %q", last.Reason, reason)
	}
}

// The trail a reviewer actually reads: a payout that fails, retries, and ends
// up queued for refund — and the elapsed time between the failure and the
// recovery being measurable from the record alone.
func TestFailureToRecoveryIsTimeable(t *testing.T) {
	db := historyDB(t)
	seedOrder(t, db, StateDepositDetected)

	if _, err := ClaimStatus(db, "ord_hist", StateDepositDetected, StatePayoutQueued); err != nil {
		t.Fatal(err)
	}
	// Three attempts, as the worker would: claim, fail, release; then refund.
	for i := 0; i < 2; i++ {
		if _, err := ClaimStatus(db, "ord_hist", StatePayoutQueued, StatePayoutProcessing); err != nil {
			t.Fatal(err)
		}
		if err := ReleaseStatusBecause(db, "ord_hist", StatePayoutProcessing, StatePayoutQueued, "provider timeout"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ClaimStatus(db, "ord_hist", StatePayoutQueued, StatePayoutProcessing); err != nil {
		t.Fatal(err)
	}
	if _, err := ClaimStatusBecause(db, "ord_hist", StatePayoutProcessing, StateRefundQueued, "permanently failed"); err != nil {
		t.Fatal(err)
	}

	events, _ := StatusHistory(db, "ord_hist")

	elapsed, ok := ElapsedBetween(events, StatePayoutQueued, StateRefundQueued)
	if !ok {
		t.Fatal("could not measure payout_queued -> refund_queued from the history")
	}
	if elapsed < 0 {
		t.Errorf("elapsed = %v, which is not a duration anyone can report", elapsed)
	}

	// The retries must be visible as retries, not collapsed into one move.
	var processing int
	for _, e := range events {
		if e.To == StatePayoutProcessing {
			processing++
		}
	}
	if processing != 3 {
		t.Errorf("recorded %d payout attempts, want 3 — retries are not visible in the trail", processing)
	}
}

// Transitions and delivery attempts share one ordered timeline, so an order's
// whole story is a single fetch rather than two lists to merge by hand.
func TestTimelineHoldsTransitionsAndNotifications(t *testing.T) {
	db := historyDB(t)
	seedOrder(t, db, StateDepositDetected)

	if _, err := ClaimStatus(db, "ord_hist", StateDepositDetected, StatePayoutQueued); err != nil {
		t.Fatal(err)
	}
	RecordNotification(db, "ord_hist", StatePayoutQueued, "attempt 1 failed after 20ms: connection refused")
	RecordNotification(db, "ord_hist", StatePayoutQueued, "delivered in 412ms")

	events, err := StatusHistory(db, "ord_hist")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("timeline has %d events, want 3", len(events))
	}

	var transitions, notifications int
	for _, e := range events {
		switch e.Kind {
		case KindTransition:
			transitions++
		case KindNotification:
			notifications++
		default:
			t.Errorf("event has unknown kind %q", e.Kind)
		}
	}
	if transitions != 1 || notifications != 2 {
		t.Errorf("got %d transitions and %d notifications, want 1 and 2", transitions, notifications)
	}
	// The failed attempt must survive: a timeline showing only the delivery
	// that worked hides how long the merchant actually waited.
	if events[1].Reason == "" {
		t.Error("the failed delivery attempt lost its reason")
	}
}

// A notification names the status it reported, so it must not be mistaken for
// the order reaching that status — that would time to the announcement rather
// than to the event.
func TestTimingIgnoresNotificationRows(t *testing.T) {
	db := historyDB(t)
	seedOrder(t, db, StateDepositDetected)

	// Announce refund_queued before the order ever gets there.
	RecordNotification(db, "ord_hist", StateRefundQueued, "delivered in 5ms")
	if _, err := ClaimStatus(db, "ord_hist", StateDepositDetected, StatePayoutQueued); err != nil {
		t.Fatal(err)
	}

	events, _ := StatusHistory(db, "ord_hist")
	if _, ok := ElapsedBetween(events, StatePayoutQueued, StateRefundQueued); ok {
		t.Error("measured to refund_queued from a notification row; the order never reached it")
	}
}

// Measuring between states an order never reached must report "not measured"
// rather than a zero duration that reads as instantaneous success.
func TestElapsedRefusesToInventDurations(t *testing.T) {
	db := historyDB(t)
	seedOrder(t, db, StateAwaitingDeposit)
	if _, err := ClaimStatus(db, "ord_hist", StateAwaitingDeposit, StateDepositDetected); err != nil {
		t.Fatal(err)
	}
	events, _ := StatusHistory(db, "ord_hist")

	if d, ok := ElapsedBetween(events, StatePayoutQueued, StateRefundQueued); ok {
		t.Errorf("reported %v between states the order never reached", d)
	}
}
