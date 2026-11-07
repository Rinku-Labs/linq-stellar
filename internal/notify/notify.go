// Package notify delivers order status changes to whoever created the order: the
// Linq backend for Linq-B2B's orders, and each integrator's own webhook for theirs.
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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gorm.io/gorm"

	"github.com/Rinku-Labs/linq-stellar/internal/store"
	"github.com/Rinku-Labs/linq-stellar/internal/wake"
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

	// maxConcurrentIntegrators bounds the lanes running at once, and so the
	// database connections a sweep can hold.
	maxConcurrentIntegrators = 8

	// maxErrorLength bounds what a receiver can put in the delivery log.
	maxErrorLength = 500
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

	// What the order was struck at, alongside what actually moved. The
	// receiving side writes emails from this payload, and "you invoiced
	// ₦10,000 and were paid ₦9,940" is a sentence it cannot form from the
	// running amounts alone.
	QuotedNGN float64 `json:"quotedNgn,omitempty"`
	QuotedUSD float64 `json:"quotedStableCoin,omitempty"`

	// Where the money went, per leg. A merchant asking "did it arrive?" is
	// asking for PayoutReference; a payer asking "where is my refund?" is
	// asking for RefundTxHash. Sending the status without them means both
	// questions come back as support tickets.
	PayoutReference string `json:"payoutReference,omitempty"`
	PayoutProvider  string `json:"payoutProvider,omitempty"`
	RefundTxHash    string `json:"refundTxHash,omitempty"`
	SweepTxHash     string `json:"sweepTxHash,omitempty"`

	// DepositAddress is the account the payer sent to; RefundDestination is
	// where a refund is going, which is the payer's own account unless they
	// named another.
	DepositAddress    string `json:"depositAddress,omitempty"`
	RefundDestination string `json:"refundDestination,omitempty"`

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

	// URL is the Linq backend's Stellar webhook endpoint, for Linq-B2B's orders.
	// Empty leaves those undelivered and still owed, which is what a local or
	// test deployment wants.
	URL string
	// Secret signs those deliveries. Empty means unsigned, which the receiver is
	// free to reject; it is left possible only so a development receiver can run
	// without one. An integrator's own deliveries are always signed.
	Secret string

	// IntegratorHTTP sends to integrators' URLs. Nil means NewIntegratorClient.
	// It is separate from HTTP because those URLs are not ours.
	IntegratorHTTP *http.Client
	// OpenSecret decrypts a stored webhook secret.
	OpenSecret func(sealed string) (string, error)

	Interval time.Duration
	Batch    int

	// Bus ends a wait the moment a transition is recorded. Without it a
	// merchant's "your payout failed" waited out whatever interval this loop
	// had drifted to while the service was quiet — a minute and a quarter, in
	// the run that prompted this.
	Bus *wake.Bus

	// pausedUntil holds off integrators whose endpoint just failed. In memory on
	// purpose: a restart costs one extra attempt each.
	pausedMu    sync.Mutex
	pausedUntil map[string]time.Time
}

// target is where one integrator's events go.
type target struct {
	integratorID string
	url          string
	// logURL is what may be logged. An integrator's path can carry a token, so
	// that is the host alone.
	logURL string
	host   string
	secret string
	client *http.Client
	// legacy is Linq-B2B, whose deliveries must not change.
	legacy bool
}

// Run delivers pending notifications until the context is cancelled.
func (w *Worker) Run(ctx context.Context) {
	if w.URL == "" {
		w.Log.Warn("no merchant webhook url configured: linq-b2b orders are not being reported")
	}
	if w.HTTP == nil {
		w.HTTP = &http.Client{Timeout: deliveryTimeout}
	}
	if w.IntegratorHTTP == nil {
		w.IntegratorHTTP = NewIntegratorClient()
	}
	interval := w.Interval
	if interval <= 0 {
		interval = defaultInterval
	}

	wake.Loop{
		Name:     "merchant notifier",
		Topic:    wake.Notify,
		Interval: interval,
		Bus:      w.Bus,
		Log:      w.Log,
		Work:     w.sweep,
	}.Run(ctx)
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

	now := time.Now().UTC()
	integrators, err := store.OwedIntegrators(w.DB, now)
	if err != nil {
		w.Log.Error("notify sweep query failed", "error", err)
		return false
	}

	// One lane per integrator, so a slow or dead endpoint holds up nobody else.
	// An endpoint that hangs would otherwise cost every other integrator, and
	// Linq-B2B, fifteen seconds per event in the queue ahead of them.
	var lanes sync.WaitGroup
	var worked atomic.Bool
	slots := make(chan struct{}, maxConcurrentIntegrators)
	for _, id := range integrators {
		if w.isPaused(id, now) || (id == store.LegacyIntegratorID && w.URL == "") {
			continue
		}
		lanes.Add(1)
		slots <- struct{}{}
		go func() {
			defer lanes.Done()
			defer func() { <-slots }()
			defer func() {
				if rec := recover(); rec != nil {
					w.Log.Error("notify lane panicked", "integrator", id, "panic", rec)
				}
			}()
			if w.deliverFor(ctx, id, batch) {
				worked.Store(true)
			}
		}()
	}
	lanes.Wait()
	return worked.Load()
}

// deliverFor works through one integrator's owed transitions, oldest first —
// not orders whose current status differs from what was reported. The difference
// matters: an order that passes through a reportable state faster than this loop
// runs still owes that report, and ordering by when the move happened means the
// merchant reads the story in the order it occurred rather than only its ending.
func (w *Worker) deliverFor(ctx context.Context, integratorID string, batch int) (found bool) {
	dest, ok := w.targetFor(integratorID)
	if !ok {
		return false
	}

	events, err := store.OwedEventsFor(w.DB, integratorID, time.Now().UTC(), batch)
	if err != nil {
		w.Log.Error("notify lane query failed", "integrator", integratorID, "error", err)
		return false
	}
	for i := range events {
		if ctx.Err() != nil {
			break
		}
		wait, delivered := w.deliverEvent(ctx, &events[i], dest)
		// A failing endpoint gets one attempt per backoff, not one per event. A
		// backlog would otherwise write a delivery row every few seconds for as
		// long as the endpoint stays down.
		if !delivered && !dest.legacy {
			w.pause(integratorID, wait)
			break
		}
	}
	return len(events) > 0
}

// targetFor resolves where an integrator's events go. ok is false when there is
// nothing to send to.
func (w *Worker) targetFor(integratorID string) (target, bool) {
	if integratorID == store.LegacyIntegratorID {
		return target{
			integratorID: integratorID,
			url:          w.URL,
			logURL:       w.URL,
			host:         hostOf(w.URL),
			secret:       w.Secret,
			client:       w.HTTP,
			legacy:       true,
		}, true
	}

	webhookURL, sealed, err := store.LoadWebhook(w.DB, integratorID)
	if err != nil {
		w.Log.Error("could not load an integrator's webhook", "integrator", integratorID, "error", err)
		w.pause(integratorID, retryBase)
		return target{}, false
	}
	if webhookURL == "" {
		if err := store.SkipOwedEvents(w.DB, integratorID, time.Now().UTC()); err != nil {
			w.Log.Error("could not close reports with no webhook", "integrator", integratorID, "error", err)
		}
		return target{}, false
	}

	// Never unsigned: without its secret an integrator's delivery does not go.
	secret, err := w.openSecret(sealed)
	if err != nil {
		w.Log.Error("could not open an integrator's webhook secret", "integrator", integratorID, "error", err)
		w.pause(integratorID, retryCap)
		return target{}, false
	}
	host := hostOf(webhookURL)
	return target{
		integratorID: integratorID,
		url:          webhookURL,
		logURL:       host,
		host:         host,
		secret:       secret,
		client:       w.IntegratorHTTP,
	}, true
}

func (w *Worker) openSecret(sealed string) (string, error) {
	if w.OpenSecret == nil {
		return "", errors.New("no way to open webhook secrets is configured")
	}
	secret, err := w.OpenSecret(sealed)
	if err == nil && secret == "" {
		return "", errors.New("the webhook secret is empty")
	}
	return secret, err
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

func (w *Worker) isPaused(integratorID string, now time.Time) bool {
	w.pausedMu.Lock()
	defer w.pausedMu.Unlock()
	return now.Before(w.pausedUntil[integratorID])
}

func (w *Worker) pause(integratorID string, wait time.Duration) {
	w.pausedMu.Lock()
	defer w.pausedMu.Unlock()
	if w.pausedUntil == nil {
		w.pausedUntil = make(map[string]time.Time)
	}
	w.pausedUntil[integratorID] = time.Now().UTC().Add(wait)
}

// cleanError makes a delivery error safe to store and show. net/http's errors
// quote the whole URL, and a receiver's reply is whatever it chose to send: a
// NUL byte in it would stop Postgres storing the row at all.
func cleanError(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	message := strings.ToValidUTF8(err.Error(), "?")
	message = strings.ReplaceAll(message, "\x00", "?")
	if len(message) > maxErrorLength {
		message = strings.ToValidUTF8(message[:maxErrorLength], "")
	}
	return message
}

// logDelivery writes one attempt to the delivery log. Best-effort, like the
// status history: the delivery has already happened, and failing it now would
// only send it again.
func (w *Worker) logDelivery(ev *store.StatusEvent, dest target, code int, elapsed time.Duration, deliveryErr error) {
	row := store.WebhookDelivery{
		OrderID:      ev.OrderID,
		IntegratorID: dest.integratorID,
		Status:       ev.To,
		Attempt:      ev.NotifyAttempts + 1,
		URLHost:      dest.host,
		ResponseCode: code,
		LatencyMS:    elapsed.Milliseconds(),
		At:           time.Now().UTC(),
	}
	if deliveryErr != nil {
		row.Error = cleanError(deliveryErr)
	}
	if err := store.RecordWebhookDelivery(w.DB, row); err != nil {
		w.Log.Error("could not write the delivery log", "order", ev.OrderID, "error", err)
	}
}

// deliverEvent posts one recorded transition and records the outcome. It reports
// whether it was delivered, and how long this event now waits if not.
func (w *Worker) deliverEvent(ctx context.Context, ev *store.StatusEvent, dest target) (wait time.Duration, delivered bool) {
	status := ev.To

	// The order is read for the figures — amounts, hashes — but the status
	// reported is the transition's, not the order's current one. An order that
	// has moved on since still owes this report: "your payout failed" is not
	// made untrue by a refund landing afterwards, and a merchant who only ever
	// hears the ending cannot tell a payment that worked from one that was
	// undone.
	var order store.Order
	if err := w.DB.Omit("encrypted_seed").First(&order, "id = ?", ev.OrderID).Error; err != nil {
		w.Log.Error("could not load order for notification", "order", ev.OrderID, "error", err)
		return w.backOff(ev), false
	}

	refundTo := order.RefundAddress
	if refundTo == "" {
		// What the chain worker will actually use: the account the payer sent
		// from, which is the only destination known to hold a USDC trustline.
		refundTo = order.DepositFrom
	}

	event := Event{
		Event:             "order." + status,
		OrderID:           ev.OrderID,
		Status:            status,
		AmountNGN:         order.AmountNGN,
		AmountUSD:         order.AmountUSDC,
		TxHash:            order.DepositTxHash,
		Underpaid:         order.Underpaid,
		Shortfall:         order.ShortfallNGN,
		QuotedNGN:         order.QuotedNGN,
		QuotedUSD:         order.QuotedUSDC,
		PayoutReference:   order.PayoutReference,
		PayoutProvider:    order.PayoutProvider,
		RefundTxHash:      order.RefundTxHash,
		SweepTxHash:       order.SweepTxHash,
		DepositAddress:    order.DepositAddress,
		RefundDestination: refundTo,
		Reason:            ev.Reason,
		OccurredAt:        ev.At.Format(time.RFC3339),
		Timestamp:         time.Now().UTC().Format(time.RFC3339),
	}
	// Which bank rail paid, and how it failed, is Linq's business. Linq-B2B shows
	// these to its own merchants; GET /v1/orders shows neither.
	if !dest.legacy {
		event.PayoutProvider = ""
		event.Reason = store.PublicReason(ev.To, ev.Reason)
	}

	body, err := json.Marshal(event)
	if err != nil {
		w.Log.Error("could not encode notification", "order", ev.OrderID, "error", err)
		return w.backOff(ev), false
	}

	// Logged before the attempt, not only after it. A delivery that hangs or
	// dies mid-flight leaves no success line and no failure line, so without
	// this the log simply has a gap where the webhook was — and a gap is
	// indistinguishable from never having tried.
	w.Log.Info("sending merchant webhook",
		"order", ev.OrderID,
		"event", event.Event,
		"status", status,
		"integrator", dest.integratorID,
		"url", dest.logURL,
		"occurred_at", ev.At.Format(time.RFC3339),
		"attempt", ev.NotifyAttempts+1,
		"signed", dest.secret != "",
		"bytes", len(body))

	start := time.Now()
	code, err := w.post(ctx, dest, body)
	elapsed := time.Since(start)
	w.logDelivery(ev, dest, code, elapsed, err)
	if err != nil {
		reason := cleanError(err)
		w.Log.Warn("merchant webhook failed, will retry",
			"order", ev.OrderID,
			"status", status,
			"integrator", dest.integratorID,
			"url", dest.logURL,
			"http_status", code,
			"attempt", ev.NotifyAttempts+1,
			"elapsed_ms", elapsed.Milliseconds(),
			"error", reason)
		store.RecordNotification(w.DB, ev.OrderID, status,
			fmt.Sprintf("attempt %d failed after %dms: %s",
				ev.NotifyAttempts+1, elapsed.Milliseconds(), reason))
		return w.backOff(ev), false
	}

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
		return 0, true
	}

	store.RecordNotification(w.DB, ev.OrderID, status,
		fmt.Sprintf("delivered in %dms (%s lag from the event)",
			elapsed.Milliseconds(), now.Sub(ev.At).Round(time.Millisecond)))

	w.Log.Info("merchant webhook delivered",
		"order", ev.OrderID,
		"event", event.Event,
		"status", status,
		"integrator", dest.integratorID,
		"url", dest.logURL,
		"http_status", code,
		"elapsed_ms", elapsed.Milliseconds(),
		// How long the merchant actually waited: from the thing happening to
		// them being told. The number the whole pipeline is judged on.
		"lag_ms", now.Sub(ev.At).Milliseconds())
	return 0, true
}

// post sends one signed delivery, returning the receiver's status code.
//
// The code comes back even on the error path: "the receiver said 401" and "the
// receiver never answered" are different problems with different fixes, and a
// log line that flattens both into "failed" sends you looking in the wrong
// place.
func (w *Worker) post(ctx context.Context, dest target, body []byte) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, deliveryTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dest.url, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if dest.secret != "" {
		mac := hmac.New(sha256.New, []byte(dest.secret))
		mac.Write(body)
		req.Header.Set("x-linq-signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}

	resp, err := dest.client.Do(req)
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

// backOff records a failed attempt, schedules the next one and returns the wait.
func (w *Worker) backOff(ev *store.StatusEvent) time.Duration {
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
	return wait
}
