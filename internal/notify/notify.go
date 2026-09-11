// Package notify delivers order status changes to the Linq backend.
//
// This service settles Stellar payments on its own: it watches Horizon, pays
// the merchant and sweeps the deposit, without anything upstream driving it.
// That independence has a cost — nothing downstream learns an order moved
// unless this service tells it. Before this package existed the only thing
// that ever noticed was the checkout page polling for a status, which meant a
// merchant found out their payout failed if, and only if, a payer happened to
// still have the tab open. Closing the tab is the normal thing to do after
// paying, so in practice the notice was never sent.
//
// Delivery is an outbox rather than a call made at the moment the status
// changes. A webhook fired inline is lost to a restart, a deploy, or the
// receiver being briefly down, and those are exactly the moments when an order
// is most likely to be in a state someone needs to hear about. Instead the
// status transition is already committed to the order row, and this package
// notices the gap between what an order is and what has been reported, then
// closes it. A delivery that fails is simply a gap that is still open.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"gorm.io/gorm"

	"github.com/Rinku-Labs/linq-stellar/internal/pace"
	"github.com/Rinku-Labs/linq-stellar/internal/store"
)

const (
	defaultInterval = 5 * time.Second
	defaultBatch    = 50

	// retryBase and retryCap bound the wait after a failed delivery. The wait
	// doubles per consecutive failure, so a receiver that is down for an hour
	// costs a handful of attempts rather than seven hundred.
	retryBase = 10 * time.Second
	retryCap  = 10 * time.Minute

	// deliveryTimeout is per attempt. Short: the receiver's job is to record
	// the event and send mail asynchronously, not to finish that work while
	// this connection is held open.
	deliveryTimeout = 15 * time.Second
)

// notifiable is the set of states worth telling the Linq backend about.
//
// Intermediate machinery — claiming, processing, queueing — is deliberately
// absent. Each state here is one a merchant or payer can act on, and each maps
// to a receipt on the other side.
var notifiable = map[string]bool{
	store.StateDepositDetected:   true, // payer's money arrived
	store.StateDisbursed:         true, // merchant has been paid
	store.StateSettledInTreasury: true, // crypto leg finished
	store.StateRefundQueued:      true, // payout failed for good; money going back
	store.StateRefunded:          true,
	store.StateExpired:           true, // deposit window closed unpaid
	store.StateFailed:            true, // needs a human
}

// Notifiable reports whether a state is delivered downstream.
func Notifiable(state string) bool { return notifiable[state] }

// Event is the payload posted to the Linq backend.
//
// Field names match what the backend's webhook receiver already reads from
// Linq's native offramp, so the two paths parse identically.
type Event struct {
	Event     string  `json:"event"`
	OrderID   string  `json:"orderId"`
	Status    string  `json:"status"`
	AmountNGN float64 `json:"amountNGN"`
	AmountUSD float64 `json:"amountStableCoin"`
	TxHash    string  `json:"txHash,omitempty"`
	Underpaid bool    `json:"underpaid,omitempty"`
	Shortfall float64 `json:"shortfallNgn,omitempty"`
	// Reason carries why, for the states where "why" is the message — a
	// payout rejected by the provider, say. The receiving side shows it to
	// the merchant rather than making them ask.
	Reason string `json:"reason,omitempty"`
	// OccurredAt is when the transition happened; Timestamp is when it was
	// sent. They differ after a retry or an outage, and a receiver that
	// treats the send time as the event time will misreport how long
	// something took.
	OccurredAt string `json:"occurredAt"`
	Timestamp  string `json:"timestamp"`
}

// Worker delivers pending status changes.
type Worker struct {
	DB   *gorm.DB
	Log  *slog.Logger
	HTTP *http.Client

	// URL is the Linq backend's Stellar webhook endpoint. Empty disables
	// delivery entirely, which is what a local or test deployment wants.
	URL string
	// Secret signs the body. Empty means unsigned, which the receiver is free
	// to reject; it is left possible only so a development receiver can run
	// without one.
	Secret string

	Interval time.Duration
	Batch    int

	idle pace.Backoff
}

// Run delivers pending notifications until the context is cancelled.
func (w *Worker) Run(ctx context.Context) {
	if w.URL == "" {
		w.Log.Warn("merchant notifier disabled: no webhook url configured")
		return
	}
	if w.HTTP == nil {
		w.HTTP = &http.Client{Timeout: deliveryTimeout}
	}
	interval := w.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	w.Log.Info("merchant notifier started", "interval", interval, "url", w.URL)

	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		w.idle.Next(w.sweep(ctx), t, interval)
		select {
		case <-ctx.Done():
			w.Log.Info("merchant notifier stopped")
			return
		case <-t.C:
		}
	}
}

func (w *Worker) sweep(ctx context.Context) (found bool) {
	defer func() {
		if rec := recover(); rec != nil {
			w.Log.Error("notify sweep panicked", "panic", rec)
		}
	}()

	batch := w.Batch
	if batch <= 0 {
		batch = defaultBatch
	}

	states := make([]string, 0, len(notifiable))
	for s := range notifiable {
		states = append(states, s)
	}

	// Owed transitions, oldest first — not orders whose current status differs
	// from what was reported. The difference matters: an order that passes
	// through a reportable state faster than this loop runs still owes that
	// report, and ordering by when the move happened means the merchant reads
	// the story in the order it occurred rather than only its ending.
	var events []store.StatusEvent
	if err := w.DB.
		Where("kind = ?", store.KindTransition).
		// Quoted: "to" is a reserved word, and the column is named for the
		// struct field rather than around SQL's vocabulary.
		Where(`"to" IN ?`, states).
		Where("notified_at IS NULL").
		Where("notify_after IS NULL OR notify_after <= ?", time.Now().UTC()).
		Order("at ASC, id ASC").
		Limit(batch).
		Find(&events).Error; err != nil {
		w.Log.Error("notify sweep query failed", "error", err)
		return false
	}

	for i := range events {
		if ctx.Err() != nil {
			return len(events) > 0
		}
		w.deliverEvent(ctx, &events[i])
	}
	return len(events) > 0
}

// deliverEvent posts one recorded transition and records the outcome.
func (w *Worker) deliverEvent(ctx context.Context, ev *store.StatusEvent) {
	status := ev.To

	// The order is read for the figures — amounts, hashes — but the status
	// reported is the transition's, not the order's current one. An order that
	// has moved on since still owes this report: "your payout failed" is not
	// made untrue by a refund landing afterwards, and a merchant who only ever
	// hears the ending cannot tell a payment that worked from one that was
	// undone.
	var order store.Order
	if err := w.DB.First(&order, "id = ?", ev.OrderID).Error; err != nil {
		w.Log.Error("could not load order for notification", "order", ev.OrderID, "error", err)
		w.backOff(ev)
		return
	}

	event := Event{
		Event:     "order." + status,
		OrderID:   ev.OrderID,
		Status:    status,
		AmountNGN: order.AmountNGN,
		AmountUSD: order.AmountUSDC,
		TxHash:    order.DepositTxHash,
		Underpaid: order.Underpaid,
		Shortfall: order.ShortfallNGN,
		Reason:    ev.Reason,
		OccurredAt: ev.At.Format(time.RFC3339),
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	body, err := json.Marshal(event)
	if err != nil {
		w.Log.Error("could not encode notification", "order", ev.OrderID, "error", err)
		w.backOff(ev)
		return
	}

	// Logged before the attempt, not only after it. A delivery that hangs or
	// dies mid-flight leaves no success line and no failure line, so without
	// this the log simply has a gap where the webhook was — and a gap is
	// indistinguishable from never having tried.
	w.Log.Info("sending merchant webhook",
		"order", ev.OrderID,
		"event", event.Event,
		"status", status,
		"url", w.URL,
		"occurred_at", ev.At.Format(time.RFC3339),
		"attempt", ev.NotifyAttempts+1,
		"signed", w.Secret != "",
		"bytes", len(body))

	start := time.Now()
	code, err := w.post(ctx, body)
	if err != nil {
		w.Log.Warn("merchant webhook failed, will retry",
			"order", ev.OrderID,
			"status", status,
			"url", w.URL,
			"http_status", code,
			"attempt", ev.NotifyAttempts+1,
			"elapsed_ms", time.Since(start).Milliseconds(),
			"error", err)
		store.RecordNotification(w.DB, ev.OrderID, status,
			fmt.Sprintf("attempt %d failed after %dms: %v",
				ev.NotifyAttempts+1, time.Since(start).Milliseconds(), err))
		w.backOff(ev)
		return
	}

	elapsed := time.Since(start)
	now := time.Now().UTC()
	if err := w.DB.Model(&store.StatusEvent{}).
		Where("id = ?", ev.ID).
		Updates(map[string]any{
			"notified_at":     now,
			"notify_attempts": 0,
			"notify_after":    nil,
		}).Error; err != nil {
		// The delivery landed but the record of it did not. The receiver is
		// idempotent per order and status, so the duplicate this causes on the
		// next pass is harmless; losing the notification would not be.
		w.Log.Error("could not record notification", "order", ev.OrderID, "error", err)
		return
	}

	store.RecordNotification(w.DB, ev.OrderID, status,
		fmt.Sprintf("delivered in %dms (%s lag from the event)",
			elapsed.Milliseconds(), now.Sub(ev.At).Round(time.Millisecond)))

	w.Log.Info("merchant webhook delivered",
		"order", ev.OrderID,
		"event", event.Event,
		"status", status,
		"url", w.URL,
		"http_status", code,
		"elapsed_ms", elapsed.Milliseconds(),
		// How long the merchant actually waited: from the thing happening to
		// them being told. The number the whole pipeline is judged on.
		"lag_ms", now.Sub(ev.At).Milliseconds())
}

// post sends one signed delivery, returning the receiver's status code.
//
// The code comes back even on the error path: "the receiver said 401" and "the
// receiver never answered" are different problems with different fixes, and a
// log line that flattens both into "failed" sends you looking in the wrong
// place.
func (w *Worker) post(ctx context.Context, body []byte) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, deliveryTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if w.Secret != "" {
		mac := hmac.New(sha256.New, []byte(w.Secret))
		mac.Write(body)
		req.Header.Set("x-linq-signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}

	resp, err := w.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	// Drained so the connection can be reused rather than dropped after every
	// delivery.
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("receiver returned %d: %s", resp.StatusCode, bytes.TrimSpace(snippet))
	}
	return resp.StatusCode, nil
}

// backOff records a failed attempt and schedules the next one.
func (w *Worker) backOff(ev *store.StatusEvent) {
	attempts := ev.NotifyAttempts + 1

	wait := retryBase << min(attempts-1, 16)
	if wait > retryCap || wait <= 0 {
		wait = retryCap
	}
	next := time.Now().UTC().Add(wait)

	if err := w.DB.Model(&store.StatusEvent{}).
		Where("id = ?", ev.ID).
		Updates(map[string]any{
			"notify_attempts": attempts,
			"notify_after":    next,
		}).Error; err != nil {
		w.Log.Error("could not record notification failure", "order", ev.OrderID, "error", err)
	}
}
