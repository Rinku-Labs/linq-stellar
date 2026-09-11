package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/Rinku-Labs/linq-stellar/internal/store"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// receiver stands in for the Linq backend's webhook endpoint.
type receiver struct {
	*httptest.Server
	status   atomic.Int32
	bodies   chan []byte
	sigs     chan string
	received atomic.Int32
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{bodies: make(chan []byte, 32), sigs: make(chan string, 32)}
	r.status.Store(http.StatusOK)
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.received.Add(1)
		select {
		case r.bodies <- body:
		default:
		}
		select {
		case r.sigs <- req.Header.Get("x-linq-signature"):
		default:
		}
		w.WriteHeader(int(r.status.Load()))
	}))
	t.Cleanup(r.Close)
	return r
}

func newWorker(t *testing.T, url, secret string) *Worker {
	t.Helper()
	return &Worker{
		DB:     newTestDB(t),
		Log:    quietLog(),
		HTTP:   &http.Client{Timeout: 2 * time.Second},
		URL:    url,
		Secret: secret,
	}
}

// seedOrder creates the order a transition belongs to.
func seedOrder(t *testing.T, db *gorm.DB, id, status string) {
	t.Helper()
	o := store.Order{ID: id, IdempotencyKey: "idem_" + id, Status: status, AmountNGN: 1000}
	if err := db.Create(&o).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
}

// move records a transition the way the store does.
func move(t *testing.T, db *gorm.DB, id, from, to string) {
	t.Helper()
	if err := db.Model(&store.Order{}).Where("id = ?", id).Update("status", to).Error; err != nil {
		t.Fatalf("move order: %v", err)
	}
	if ok, err := store.ClaimStatus(db, id, from, to); err != nil {
		t.Fatalf("claim: %v", err)
	} else if !ok {
		// The order was already advanced above, so record directly — this
		// helper exists to build histories, not to exercise claiming.
		db.Create(&store.StatusEvent{
			OrderID: id, Kind: store.KindTransition, From: from, To: to, At: time.Now().UTC(),
		})
	}
}

func pending(t *testing.T, db *gorm.DB, id string) int {
	t.Helper()
	var n int64
	db.Model(&store.StatusEvent{}).
		Where("order_id = ? AND kind = ? AND notified_at IS NULL", id, store.KindTransition).
		Count(&n)
	return int(n)
}

// The regression that prompted this design.
//
// An order that failed its payout and was queued for refund inside one poll
// interval reported neither: keyed off the order's current status, the loop
// saw refund_processing — not notifiable — and the merchant was never told the
// payout failed at all, hearing only "refunded" a minute later.
func TestFastTransitionIsStillReported(t *testing.T) {
	r := newReceiver(t)
	w := newWorker(t, r.URL, "")
	seedOrder(t, w.DB, "ord_fast", store.StatePayoutProcessing)

	// Races through a reportable state before the notifier ever runs.
	move(t, w.DB, "ord_fast", store.StatePayoutProcessing, store.StateRefundQueued)
	move(t, w.DB, "ord_fast", store.StateRefundQueued, store.StateRefundProcessing)
	move(t, w.DB, "ord_fast", store.StateRefundProcessing, store.StateRefunded)

	w.sweep(context.Background())

	var reported []string
	for len(r.bodies) > 0 {
		var e Event
		if err := json.Unmarshal(<-r.bodies, &e); err != nil {
			t.Fatal(err)
		}
		reported = append(reported, e.Status)
	}

	var sawRefundQueued, sawRefunded bool
	for _, s := range reported {
		if s == store.StateRefundQueued {
			sawRefundQueued = true
		}
		if s == store.StateRefunded {
			sawRefunded = true
		}
	}
	if !sawRefundQueued {
		t.Error("refund_queued was never reported — the merchant is not told the payout failed")
	}
	if !sawRefunded {
		t.Error("refunded was never reported")
	}
	// refund_processing is machinery; nobody gets mail about it.
	for _, s := range reported {
		if s == store.StateRefundProcessing {
			t.Error("refund_processing was reported; it is internal")
		}
	}
}

// States are reported in the order they happened, so the merchant reads the
// story forwards rather than learning the ending first.
func TestReportsInChronologicalOrder(t *testing.T) {
	r := newReceiver(t)
	w := newWorker(t, r.URL, "")
	seedOrder(t, w.DB, "ord_seq", store.StateAwaitingDeposit)

	move(t, w.DB, "ord_seq", store.StateAwaitingDeposit, store.StateDepositDetected)
	move(t, w.DB, "ord_seq", store.StateDepositDetected, store.StatePayoutQueued)
	move(t, w.DB, "ord_seq", store.StatePayoutQueued, store.StatePayoutProcessing)
	move(t, w.DB, "ord_seq", store.StatePayoutProcessing, store.StateDisbursed)

	w.sweep(context.Background())

	var got []string
	for len(r.bodies) > 0 {
		var e Event
		_ = json.Unmarshal(<-r.bodies, &e)
		got = append(got, e.Status)
	}
	want := []string{store.StateDepositDetected, store.StateDisbursed}
	if len(got) != len(want) {
		t.Fatalf("reported %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d reported %q, want %q", i, got[i], want[i])
		}
	}
}

// A delivered transition is not delivered again.
func TestDeliveredOnce(t *testing.T) {
	r := newReceiver(t)
	w := newWorker(t, r.URL, "")
	seedOrder(t, w.DB, "ord_once", store.StateDepositDetected)
	move(t, w.DB, "ord_once", store.StateDepositDetected, store.StateDisbursed)

	w.sweep(context.Background())
	first := r.received.Load()
	w.sweep(context.Background())

	if r.received.Load() != first {
		t.Errorf("re-sent an already delivered transition (%d then %d)", first, r.received.Load())
	}
	if pending(t, w.DB, "ord_once") != 0 {
		t.Error("a delivered transition is still marked owed")
	}
}

// A failing receiver leaves the transition owed and schedules a retry.
func TestFailedDeliveryStaysOwed(t *testing.T) {
	r := newReceiver(t)
	r.status.Store(http.StatusInternalServerError)
	w := newWorker(t, r.URL, "")
	seedOrder(t, w.DB, "ord_fail", store.StateDepositDetected)
	move(t, w.DB, "ord_fail", store.StateDepositDetected, store.StateDisbursed)

	w.sweep(context.Background())

	if pending(t, w.DB, "ord_fail") == 0 {
		t.Fatal("a failed delivery was marked as done")
	}

	// Backoff holds it back rather than spinning.
	before := r.received.Load()
	w.sweep(context.Background())
	if r.received.Load() != before {
		t.Error("retried immediately; backoff was not honoured")
	}

	// Once the receiver recovers and the wait passes, it goes out.
	r.status.Store(http.StatusOK)
	w.DB.Model(&store.StatusEvent{}).
		Where("order_id = ?", "ord_fail").
		Update("notify_after", time.Now().UTC().Add(-time.Second))
	w.sweep(context.Background())

	if pending(t, w.DB, "ord_fail") != 0 {
		t.Error("still owed after the receiver recovered")
	}
}

// The signature must match what the backend verifies.
func TestSignatureMatchesReceiverScheme(t *testing.T) {
	r := newReceiver(t)
	w := newWorker(t, r.URL, "top-secret")
	seedOrder(t, w.DB, "ord_sig", store.StateDepositDetected)
	move(t, w.DB, "ord_sig", store.StateDepositDetected, store.StateDisbursed)

	w.sweep(context.Background())

	body := <-r.bodies
	sig := <-r.sigs
	mac := hmac.New(sha256.New, []byte("top-secret"))
	mac.Write(body)
	if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); sig != want {
		t.Errorf("signature = %q, want %q", sig, want)
	}
}

// The event carries when it happened, not just when it was sent. A receiver
// that treats the send time as the event time misreports how long things took.
func TestEventCarriesOccurredAt(t *testing.T) {
	r := newReceiver(t)
	w := newWorker(t, r.URL, "")
	seedOrder(t, w.DB, "ord_when", store.StateDepositDetected)
	move(t, w.DB, "ord_when", store.StateDepositDetected, store.StateDisbursed)

	w.sweep(context.Background())

	var e Event
	if err := json.Unmarshal(<-r.bodies, &e); err != nil {
		t.Fatal(err)
	}
	if e.OccurredAt == "" {
		t.Error("occurredAt is empty, so the receiver cannot tell when this happened")
	}
	if _, err := time.Parse(time.RFC3339, e.OccurredAt); err != nil {
		t.Errorf("occurredAt %q does not parse: %v", e.OccurredAt, err)
	}
}

// The failure states a merchant must hear about, pinned so none can quietly
// stop being reported.
func TestFailureStatesAreReportable(t *testing.T) {
	for _, must := range []string{
		store.StateRefundQueued, store.StateRefunded,
		store.StateFailed, store.StateExpired,
	} {
		if !notifiable[must] {
			t.Errorf("%q is not notifiable, so a merchant would never be told", must)
		}
	}
	for _, internal := range []string{
		store.StatePayoutQueued, store.StatePayoutProcessing,
		store.StateSweepQueued, store.StateSweepProcessing,
		store.StateRefundProcessing, store.StateInitiated,
	} {
		if notifiable[internal] {
			t.Errorf("%q is notifiable, but it is internal machinery", internal)
		}
	}
}

// With no URL the worker stands down rather than spinning.
func TestDisabledWithoutURL(t *testing.T) {
	w := newWorker(t, "", "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return with no URL configured")
	}
}

// The receiver writes the emails, and it can only say what this payload tells
// it. A refund notice that does not name the destination, or a payout failure
// that does not carry the provider's reason, turns into a support ticket asking
// the question the event already had the answer to.
func TestRefundEventCarriesWhatTheEmailNeeds(t *testing.T) {
	r := newReceiver(t)
	w := newWorker(t, r.URL, "")

	order := store.Order{
		ID:             "ord_refund_detail",
		IdempotencyKey: "idem_ord_refund_detail",
		Status:         store.StatePayoutProcessing,
		AmountNGN:      9940,
		AmountUSDC:     6.1,
		QuotedNGN:      10000,
		QuotedUSDC:     6.135,
		DepositAddress: "GDEPOSIT",
		DepositFrom:    "GPAYER",
		DepositTxHash:  "deposit-tx",
	}
	if err := w.DB.Create(&order).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := store.ClaimStatusBecause(w.DB, order.ID,
		store.StatePayoutProcessing, store.StateRefundQueued,
		"bank declined: account name mismatch"); err != nil {
		t.Fatalf("queue refund: %v", err)
	}

	w.sweep(context.Background())

	var e Event
	if err := json.Unmarshal(<-r.bodies, &e); err != nil {
		t.Fatal(err)
	}
	if e.Status != store.StateRefundQueued {
		t.Fatalf("status = %q, want %q", e.Status, store.StateRefundQueued)
	}
	if e.Reason != "bank declined: account name mismatch" {
		t.Errorf("reason = %q; the merchant is owed the provider's own words", e.Reason)
	}
	if e.RefundDestination != "GPAYER" {
		t.Errorf("refundDestination = %q, want the payer's own account", e.RefundDestination)
	}
	if e.QuotedNGN != 10000 {
		t.Errorf("quotedNgn = %v, want the invoice the order was struck at", e.QuotedNGN)
	}
	if e.DepositAddress != "GDEPOSIT" {
		t.Errorf("depositAddress = %q, want the account the payer paid into", e.DepositAddress)
	}
}

// A payer who named a refund address gets their refund there, so that is the
// destination the notice has to quote — not the account they happened to send
// from.
func TestRefundDestinationPrefersTheAddressThePayerGave(t *testing.T) {
	r := newReceiver(t)
	w := newWorker(t, r.URL, "")

	order := store.Order{
		ID:             "ord_refund_named",
		IdempotencyKey: "idem_ord_refund_named",
		Status:         store.StatePayoutProcessing,
		RefundAddress:  "GNAMED",
		DepositFrom:    "GPAYER",
	}
	if err := w.DB.Create(&order).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := store.ClaimStatus(w.DB, order.ID,
		store.StatePayoutProcessing, store.StateRefundQueued); err != nil {
		t.Fatalf("queue refund: %v", err)
	}

	w.sweep(context.Background())

	var e Event
	if err := json.Unmarshal(<-r.bodies, &e); err != nil {
		t.Fatal(err)
	}
	if e.RefundDestination != "GNAMED" {
		t.Errorf("refundDestination = %q, want the address the payer supplied", e.RefundDestination)
	}
}
