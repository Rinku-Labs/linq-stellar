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

	"github.com/Rinku-Labs/linq-stellar/internal/store"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

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
	r := &receiver{bodies: make(chan []byte, 16), sigs: make(chan string, 16)}
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
		HTTP:   &http.Client{Timeout: 5 * time.Second},
		URL:    url,
		Secret: secret,
	}
}

func seed(t *testing.T, w *Worker, o store.Order) store.Order {
	t.Helper()
	if o.ID == "" {
		o.ID = "ord_" + o.Status
	}
	// Unique per order: the column carries a unique index, so seeding several
	// orders in one test collides on the empty string otherwise.
	if o.IdempotencyKey == "" {
		o.IdempotencyKey = "idem_" + o.ID
	}
	if err := w.DB.Create(&o).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	return o
}

func reload(t *testing.T, w *Worker, id string) store.Order {
	t.Helper()
	var got store.Order
	if err := w.DB.First(&got, "id = ?", id).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	return got
}

// The whole point of the package: a state a merchant cares about gets
// delivered, and is then recorded as delivered so it is not sent again.
func TestDeliversAndRecordsStatus(t *testing.T) {
	r := newReceiver(t)
	w := newWorker(t, r.URL, "shhh")
	o := seed(t, w, store.Order{Status: store.StateDisbursed, AmountNGN: 1000, AmountUSDC: 0.65})

	w.sweep(context.Background())

	if got := r.received.Load(); got != 1 {
		t.Fatalf("receiver saw %d deliveries, want 1", got)
	}
	if got := reload(t, w, o.ID).NotifiedStatus; got != store.StateDisbursed {
		t.Errorf("notified_status = %q, want %q", got, store.StateDisbursed)
	}

	// A second pass must not re-send what has already been delivered.
	w.sweep(context.Background())
	if got := r.received.Load(); got != 1 {
		t.Errorf("receiver saw %d deliveries after a second sweep, want 1", got)
	}
}

// The body must be signed the way the backend verifies it, or every delivery
// is rejected at the door.
func TestSignatureMatchesReceiverScheme(t *testing.T) {
	r := newReceiver(t)
	w := newWorker(t, r.URL, "top-secret")
	seed(t, w, store.Order{Status: store.StateRefunded})

	w.sweep(context.Background())

	body := <-r.bodies
	sig := <-r.sigs

	mac := hmac.New(sha256.New, []byte("top-secret"))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if sig != want {
		t.Errorf("signature = %q, want %q", sig, want)
	}
}

// A failing receiver must leave the order undelivered and schedule a retry,
// not drop the notification on the floor.
func TestFailedDeliveryIsRetriedNotLost(t *testing.T) {
	r := newReceiver(t)
	r.status.Store(http.StatusInternalServerError)
	w := newWorker(t, r.URL, "")
	o := seed(t, w, store.Order{Status: store.StateRefundQueued})

	w.sweep(context.Background())

	got := reload(t, w, o.ID)
	if got.NotifiedStatus != "" {
		t.Errorf("notified_status = %q, want empty after a failed delivery", got.NotifiedStatus)
	}
	if got.NotifyAttempts != 1 {
		t.Errorf("notify_attempts = %d, want 1", got.NotifyAttempts)
	}
	if got.NotifyAfter == nil || !got.NotifyAfter.After(time.Now().UTC()) {
		t.Fatal("notify_after was not pushed into the future, so the retry would spin")
	}

	// Still pending, just not yet: the backoff must actually hold it back.
	w.sweep(context.Background())
	if r.received.Load() != 1 {
		t.Errorf("receiver saw %d deliveries, want 1 — backoff was not honoured", r.received.Load())
	}

	// Once the receiver recovers and the wait has passed, it goes out.
	r.status.Store(http.StatusOK)
	w.DB.Model(&store.Order{}).Where("id = ?", o.ID).Update("notify_after", time.Now().UTC().Add(-time.Second))
	w.sweep(context.Background())

	if got := reload(t, w, o.ID); got.NotifiedStatus != store.StateRefundQueued {
		t.Errorf("notified_status = %q, want %q after recovery", got.NotifiedStatus, store.StateRefundQueued)
	}
}

// Backoff must widen rather than retry at a fixed interval, and must stay
// bounded so a long outage does not schedule a retry years out.
func TestBackoffWidensAndIsCapped(t *testing.T) {
	r := newReceiver(t)
	r.status.Store(http.StatusInternalServerError)
	w := newWorker(t, r.URL, "")
	o := seed(t, w, store.Order{Status: store.StateFailed})

	var last time.Duration
	for i := 0; i < 3; i++ {
		w.DB.Model(&store.Order{}).Where("id = ?", o.ID).Update("notify_after", nil)
		before := time.Now().UTC()
		w.sweep(context.Background())
		got := reload(t, w, o.ID)
		if got.NotifyAfter == nil {
			t.Fatalf("attempt %d: notify_after not set", i+1)
		}
		wait := got.NotifyAfter.Sub(before)
		if i > 0 && wait <= last {
			t.Errorf("attempt %d: wait %v did not widen beyond %v", i+1, wait, last)
		}
		if wait > retryCap+time.Second {
			t.Errorf("attempt %d: wait %v exceeded cap %v", i+1, wait, retryCap)
		}
		last = wait
	}
}

// Intermediate plumbing states are the service's own business. Delivering them
// would mean a merchant receiving mail for "payout_processing".
func TestIntermediateStatesAreNotDelivered(t *testing.T) {
	r := newReceiver(t)
	w := newWorker(t, r.URL, "")
	for _, s := range []string{
		store.StateInitiated,
		store.StateAwaitingDeposit,
		store.StatePayoutQueued,
		store.StatePayoutProcessing,
		store.StateSweepQueued,
		store.StateSweepProcessing,
		store.StateRefundProcessing,
	} {
		seed(t, w, store.Order{ID: "ord_" + s, Status: s})
	}

	w.sweep(context.Background())

	if got := r.received.Load(); got != 0 {
		t.Errorf("receiver saw %d deliveries for intermediate states, want 0", got)
	}
}

// Every state the notifier claims to deliver must be one the backend turns
// into a receipt. A state delivered to nobody is worse than not delivering it:
// it reads as covered.
func TestNotifiableStatesAreAllTerminalOrActionable(t *testing.T) {
	for state := range notifiable {
		if !store.IsTerminal(state) &&
			state != store.StateDepositDetected &&
			state != store.StateDisbursed &&
			state != store.StateRefundQueued {
			t.Errorf("state %q is notifiable but is neither terminal nor a known actionable step", state)
		}
	}
	// The failure path a merchant must hear about, explicitly.
	for _, must := range []string{store.StateRefundQueued, store.StateRefunded, store.StateFailed, store.StateExpired} {
		if !notifiable[must] {
			t.Errorf("state %q is not notifiable, so a merchant would never be told", must)
		}
	}
}

// An order that moves on before delivery reports where it ended up.
func TestDeliversCurrentStatusNotStaleOne(t *testing.T) {
	r := newReceiver(t)
	w := newWorker(t, r.URL, "")
	o := seed(t, w, store.Order{Status: store.StateDisbursed, NotifiedStatus: store.StateDepositDetected})

	w.sweep(context.Background())

	var event Event
	if err := json.Unmarshal(<-r.bodies, &event); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if event.Status != store.StateDisbursed {
		t.Errorf("delivered status %q, want %q", event.Status, store.StateDisbursed)
	}
	if event.Event != "order."+store.StateDisbursed {
		t.Errorf("event name %q, want %q", event.Event, "order."+store.StateDisbursed)
	}
	if event.OrderID != o.ID {
		t.Errorf("order id %q, want %q", event.OrderID, o.ID)
	}
}

// With no URL configured the worker must stand down rather than spin.
func TestDisabledWithoutURL(t *testing.T) {
	w := newWorker(t, "", "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return with no URL configured")
	}
}
