package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rinku-Labs/linq-stellar/internal/store"
	"gorm.io/gorm"
)

const sealPrefix = "sealed:"

// integratorWorker is a worker that can reach local test servers and "opens" a
// secret by removing a prefix.
func integratorWorker(t *testing.T, url, secret string) *Worker {
	t.Helper()
	w := newWorker(t, url, secret)
	w.IntegratorHTTP = &http.Client{Timeout: 2 * time.Second}
	w.OpenSecret = func(sealed string) (string, error) {
		if !strings.HasPrefix(sealed, sealPrefix) {
			return "", errors.New("cannot open this")
		}
		return strings.TrimPrefix(sealed, sealPrefix), nil
	}
	return w
}

func addIntegrator(t *testing.T, db *gorm.DB, name, url, secret string) string {
	t.Helper()
	integrator, err := store.CreateIntegrator(db, name, nil)
	if err != nil {
		t.Fatalf("create integrator: %v", err)
	}
	if url != "" {
		if err := store.SetWebhook(db, integrator.ID, url, sealPrefix+secret); err != nil {
			t.Fatalf("set webhook: %v", err)
		}
	}
	return integrator.ID
}

// owe gives an integrator an order with one report due.
func owe(t *testing.T, db *gorm.DB, integratorID, orderID string) {
	t.Helper()
	order := store.Order{ID: orderID, IntegratorID: integratorID, IdempotencyKey: "idem_" + orderID,
		Status: store.StateDisbursed, AmountNGN: 1000, PayoutProvider: "provider-a"}
	if err := db.Create(&order).Error; err != nil {
		t.Fatalf("seed order: %v", err)
	}
	event := store.StatusEvent{OrderID: orderID, Kind: store.KindTransition, To: store.StateDisbursed, At: time.Now().UTC()}
	if err := db.Create(&event).Error; err != nil {
		t.Fatalf("seed event: %v", err)
	}
}

func deliveries(t *testing.T, db *gorm.DB) []store.WebhookDelivery {
	t.Helper()
	var rows []store.WebhookDelivery
	if err := db.Order("id").Find(&rows).Error; err != nil {
		t.Fatalf("load deliveries: %v", err)
	}
	return rows
}

func orderIDOf(t *testing.T, body []byte) string {
	t.Helper()
	var event Event
	if err := json.Unmarshal(body, &event); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return event.OrderID
}

func signed(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// The issue's first test: each integrator receives only its own orders, signed
// with its own secret, and Linq-B2B keeps its URL and secret.
func TestEachIntegratorReceivesOnlyItsOwnOrders(t *testing.T) {
	legacy, a, b := newReceiver(t), newReceiver(t), newReceiver(t)
	w := integratorWorker(t, legacy.URL, "legacy-secret")
	idA := addIntegrator(t, w.DB, "A", a.URL, "secret-a")
	idB := addIntegrator(t, w.DB, "B", b.URL, "secret-b")

	owe(t, w.DB, idA, "ord_a")
	owe(t, w.DB, idB, "ord_b")
	owe(t, w.DB, store.LegacyIntegratorID, "ord_legacy")

	w.sweep(context.Background())

	for name, tc := range map[string]struct {
		r             *receiver
		order, secret string
		otherSecret   string
	}{
		"a":      {a, "ord_a", "secret-a", "secret-b"},
		"b":      {b, "ord_b", "secret-b", "secret-a"},
		"legacy": {legacy, "ord_legacy", "legacy-secret", "secret-a"},
	} {
		if got := tc.r.received.Load(); got != 1 {
			t.Fatalf("%s received %d deliveries, want 1", name, got)
		}
		body, sig := <-tc.r.bodies, <-tc.r.sigs
		if got := orderIDOf(t, body); got != tc.order {
			t.Errorf("%s received %s", name, got)
		}
		if sig != signed(tc.secret, body) {
			t.Errorf("%s: signature does not match its own secret", name)
		}
		if sig == signed(tc.otherSecret, body) {
			t.Errorf("%s: signed with another integrator's secret", name)
		}
	}
}

func TestEveryAttemptIsLogged(t *testing.T) {
	r := newReceiver(t)
	w := integratorWorker(t, "", "")
	id := addIntegrator(t, w.DB, "A", r.URL+"/hooks/private-token", "secret-a")
	owe(t, w.DB, id, "ord_a")

	r.status.Store(http.StatusInternalServerError)
	w.sweep(context.Background())

	// Let the backoff pass, as time would.
	w.pausedUntil = nil
	w.DB.Model(&store.StatusEvent{}).Where("order_id = ?", "ord_a").Update("notify_after", nil)
	r.status.Store(http.StatusOK)
	w.sweep(context.Background())

	rows := deliveries(t, w.DB)
	if len(rows) != 2 {
		t.Fatalf("%d rows, want one per attempt: %+v", len(rows), rows)
	}
	failed, ok := rows[0], rows[1]
	if failed.Attempt != 1 || failed.ResponseCode != 500 || failed.Error == "" {
		t.Errorf("failed attempt = %+v", failed)
	}
	if ok.Attempt != 2 || ok.ResponseCode != 200 || ok.Error != "" {
		t.Errorf("good attempt = %+v", ok)
	}
	host := strings.TrimPrefix(r.URL, "http://")
	for _, row := range rows {
		if row.OrderID != "ord_a" || row.IntegratorID != id || row.Status != store.StateDisbursed {
			t.Errorf("row = %+v", row)
		}
		if row.URLHost != host {
			t.Errorf("url_host = %q, want %q", row.URLHost, host)
		}
		if row.LatencyMS < 0 || row.At.IsZero() {
			t.Errorf("row = %+v", row)
		}
	}
}

func TestLegacyAttemptsAreLoggedToo(t *testing.T) {
	r := newReceiver(t)
	w := integratorWorker(t, r.URL, "s")
	owe(t, w.DB, store.LegacyIntegratorID, "ord_l")
	w.sweep(context.Background())
	if rows := deliveries(t, w.DB); len(rows) != 1 || rows[0].IntegratorID != store.LegacyIntegratorID {
		t.Errorf("rows = %+v", rows)
	}
}

// A path can hold the integrator's token, so neither the error text nor the
// history an order shows may repeat it.
func TestAFailedDeliveryDoesNotRepeatTheURL(t *testing.T) {
	r := newReceiver(t)
	w := integratorWorker(t, "", "")
	dead := r.URL + "/hooks/private-token"
	id := addIntegrator(t, w.DB, "A", dead, "secret-a")
	owe(t, w.DB, id, "ord_a")
	r.Close()

	w.sweep(context.Background())

	rows := deliveries(t, w.DB)
	if len(rows) != 1 || rows[0].Error == "" || rows[0].ResponseCode != 0 {
		t.Fatalf("rows = %+v", rows)
	}
	var history []store.StatusEvent
	w.DB.Find(&history, "order_id = ? AND kind = ?", "ord_a", store.KindNotification)
	if len(history) != 1 {
		t.Fatalf("history = %+v", history)
	}
	for _, text := range []string{rows[0].Error, history[0].Reason} {
		if strings.Contains(text, "private-token") {
			t.Errorf("the path leaked: %q", text)
		}
	}
}

// The reason lanes exist. Sequentially, this endpoint would cost everyone behind
// it the full timeout for each event in its backlog.
func TestAHangingEndpointDelaysNobodyElse(t *testing.T) {
	release := make(chan struct{})
	hanging := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer hanging.Close()
	defer close(release)

	legacy, b := newReceiver(t), newReceiver(t)
	w := integratorWorker(t, legacy.URL, "s")
	idA := addIntegrator(t, w.DB, "A", hanging.URL, "secret-a")
	idB := addIntegrator(t, w.DB, "B", b.URL, "secret-b")

	// The stuck integrator's backlog is the oldest thing in the table.
	for i := 0; i < 60; i++ {
		owe(t, w.DB, idA, fmt.Sprintf("ord_a%d", i))
	}
	owe(t, w.DB, idB, "ord_b")
	owe(t, w.DB, store.LegacyIntegratorID, "ord_legacy")

	done := make(chan struct{})
	go func() { w.sweep(context.Background()); close(done) }()

	for name, r := range map[string]*receiver{"legacy": legacy, "b": b} {
		select {
		case <-r.bodies:
		case <-time.After(time.Second):
			t.Errorf("%s waited behind the hanging endpoint", name)
		}
	}
	<-done
}

// Without a pause a dead endpoint with a backlog gets a fresh attempt, and a
// fresh log row, on every pass.
func TestAFailingEndpointIsTriedOncePerBackoff(t *testing.T) {
	r := newReceiver(t)
	r.status.Store(http.StatusServiceUnavailable)
	w := integratorWorker(t, "", "")
	id := addIntegrator(t, w.DB, "A", r.URL, "secret-a")
	for i := 0; i < 5; i++ {
		owe(t, w.DB, id, fmt.Sprintf("ord_%d", i))
	}

	for i := 0; i < 3; i++ {
		w.sweep(context.Background())
	}
	if got := r.received.Load(); got != 1 {
		t.Errorf("%d attempts, want 1", got)
	}
	if got := pending(t, w.DB, "ord_0"); got != 1 {
		t.Errorf("the failed report is no longer owed: %d", got)
	}
}

// Legacy keeps its old behaviour: each event is tried, whatever came before it.
func TestLegacyKeepsTryingEachEvent(t *testing.T) {
	r := newReceiver(t)
	r.status.Store(http.StatusServiceUnavailable)
	w := integratorWorker(t, r.URL, "s")
	for i := 0; i < 3; i++ {
		owe(t, w.DB, store.LegacyIntegratorID, fmt.Sprintf("ord_%d", i))
	}
	w.sweep(context.Background())
	if got := r.received.Load(); got != 3 {
		t.Errorf("%d attempts, want 3", got)
	}
}

func TestNoWebhookMeansNothingIsOwed(t *testing.T) {
	w := integratorWorker(t, "", "")
	id := addIntegrator(t, w.DB, "Poller", "", "")
	owe(t, w.DB, id, "ord_p")

	w.sweep(context.Background())

	if rows := deliveries(t, w.DB); len(rows) != 0 {
		t.Errorf("rows = %+v", rows)
	}
	owed, err := store.OwedIntegrators(w.DB, time.Now().UTC())
	if err != nil || len(owed) != 0 {
		t.Errorf("owed = %v, %v", owed, err)
	}
	// Closed, not reported: nothing may claim the integrator was told.
	var event store.StatusEvent
	w.DB.First(&event, "order_id = ?", "ord_p")
	if event.NotifiedAt != nil || event.NotifySkippedAt == nil {
		t.Errorf("event = %+v", event)
	}
}

// With no secret there is no way for the receiver to trust the delivery, so it
// must not go at all.
func TestAnIntegratorDeliveryIsNeverUnsigned(t *testing.T) {
	r := newReceiver(t)
	for name, setup := range map[string]func(*Worker, string){
		"secret cannot be opened": func(w *Worker, id string) {
			w.DB.Model(&store.Integrator{}).Where("id = ?", id).Update("webhook_secret_encrypted", "garbage")
		},
		"secret is empty": func(w *Worker, id string) {
			w.DB.Model(&store.Integrator{}).Where("id = ?", id).Update("webhook_secret_encrypted", sealPrefix)
		},
		"no way to open it": func(w *Worker, _ string) { w.OpenSecret = nil },
	} {
		t.Run(name, func(t *testing.T) {
			before := r.received.Load()
			w := integratorWorker(t, "", "")
			id := addIntegrator(t, w.DB, "A", r.URL, "secret-a")
			owe(t, w.DB, id, "ord_a")
			setup(w, id)

			w.sweep(context.Background())

			if got := r.received.Load() - before; got != 0 {
				t.Errorf("%d deliveries went out", got)
			}
			if got := pending(t, w.DB, "ord_a"); got != 1 {
				t.Errorf("the report is no longer owed: %d", got)
			}
		})
	}
}

func TestTheBankRailIsNotSentToIntegrators(t *testing.T) {
	r := newReceiver(t)
	w := integratorWorker(t, "", "")
	id := addIntegrator(t, w.DB, "A", r.URL, "secret-a")
	owe(t, w.DB, id, "ord_a")
	w.sweep(context.Background())
	if body := <-r.bodies; strings.Contains(string(body), "provider-a") {
		t.Errorf("body = %s", body)
	}
}

// A deployment with no MERCHANT_WEBHOOK_URL keeps Linq-B2B's reports owed, as it
// always has, while integrators' still go.
func TestNoLegacyURLStillServesIntegrators(t *testing.T) {
	r := newReceiver(t)
	w := integratorWorker(t, "", "")
	id := addIntegrator(t, w.DB, "A", r.URL, "secret-a")
	owe(t, w.DB, id, "ord_a")
	owe(t, w.DB, store.LegacyIntegratorID, "ord_legacy")

	w.sweep(context.Background())

	if got := r.received.Load(); got != 1 {
		t.Errorf("%d deliveries, want 1", got)
	}
	if got := pending(t, w.DB, "ord_legacy"); got != 1 {
		t.Errorf("linq-b2b's report is no longer owed: %d", got)
	}
}

// owedEvents spells the states out for its index, so it has to be held to the
// map that says what is worth reporting.
func TestOwedEventsMatchesWhatIsNotifiable(t *testing.T) {
	w := integratorWorker(t, "", "")
	all := []string{
		store.StateInitiated, store.StateAwaitingDeposit, store.StateDepositDetected,
		store.StatePayoutQueued, store.StatePayoutProcessing, store.StateDisbursed,
		store.StateSweepQueued, store.StateSweepProcessing, store.StateSettledInTreasury,
		store.StateRefundQueued, store.StateRefundProcessing, store.StateRefunded,
		store.StateExpired, store.StateFailed,
	}
	seedOrder(t, w.DB, "ord_all", store.StateFailed)
	for _, state := range all {
		event := store.StatusEvent{OrderID: "ord_all", Kind: store.KindTransition, To: state, At: time.Now().UTC()}
		if err := w.DB.Create(&event).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	owed, err := store.OwedEventsFor(w.DB, store.LegacyIntegratorID, time.Now().UTC(), 100)
	if err != nil {
		t.Fatalf("owed: %v", err)
	}
	got := map[string]bool{}
	for _, event := range owed {
		got[event.To] = true
	}
	for _, state := range all {
		if got[state] != Notifiable(state) {
			t.Errorf("%s: owed = %v, notifiable = %v", state, got[state], Notifiable(state))
		}
	}
}

func TestOnlyPublicAddressesAreAllowed(t *testing.T) {
	for address, want := range map[string]bool{
		"8.8.8.8":                true,
		"2606:4700:4700::1111":   true,
		"127.0.0.1":              false,
		"10.0.0.1":               false,
		"172.16.0.1":             false,
		"192.168.1.1":            false,
		"169.254.169.254":        false, // cloud metadata
		"100.64.0.1":             false,
		"0.0.0.0":                false,
		"224.0.0.1":              false,
		"255.255.255.255":        false,
		"::1":                    false,
		"::":                     false,
		"fc00::1":                false,
		"fe80::1":                false,
		"::ffff:127.0.0.1":       false, // IPv4 dressed as IPv6
		"::ffff:169.254.169.254": false,
		"64:ff9b::7f00:1":        false, // NAT64 for 127.0.0.1
	} {
		if got := isPublic(netip.MustParseAddr(address)); got != want {
			t.Errorf("isPublic(%s) = %v, want %v", address, got, want)
		}
	}
}

func TestValidateWebhookURL(t *testing.T) {
	for _, good := range []string{
		"https://hooks.example.com/linq",
		"https://hooks.example.com:8443/linq?token=abc",
		"https://8.8.8.8/hook",
	} {
		if err := ValidateWebhookURL(good); err != nil {
			t.Errorf("%s: %v", good, err)
		}
	}
	for _, bad := range []string{
		"http://hooks.example.com/linq",
		"ftp://hooks.example.com",
		"file:///etc/passwd",
		"https://",
		"not a url",
		"https://user:pass@hooks.example.com/",
		"https://hooks.example.com/#frag",
		"https://localhost/hook",
		"https://LOCALHOST./hook",
		"https://foo.localhost/hook",
		"https://127.0.0.1/hook",
		"https://169.254.169.254/latest/meta-data/",
		"https://[::1]/hook",
		"https://[::ffff:10.0.0.1]/hook",
		"https://hooks.example.com/" + strings.Repeat("a", maxWebhookURLLength),
	} {
		if err := ValidateWebhookURL(bad); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
}

// This is the check that holds when DNS lies: it sees the address actually being
// connected to, whatever the hostname was.
func TestTheClientRefusesToConnectToInternalAddresses(t *testing.T) {
	var hit atomic.Int32
	internal := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit.Add(1) }))
	defer internal.Close()

	// "localhost" reaches the same place by name, as a rebinding host would.
	for _, address := range []string{internal.URL, strings.Replace(internal.URL, "127.0.0.1", "localhost", 1)} {
		_, err := NewIntegratorClient().Get(address)
		if !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("%s: err = %v, want ErrBlockedAddress", address, err)
		}
	}
	if hit.Load() != 0 {
		t.Error("the internal server was reached")
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var hit atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit.Add(1) }))
	defer internal.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL, http.StatusFound)
	}))
	defer redirector.Close()

	client := NewIntegratorClient()
	client.Transport = http.DefaultTransport // the dial guard would stop this before the redirect
	res, err := client.Get(redirector.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound || hit.Load() != 0 {
		t.Errorf("status = %d, internal hits = %d", res.StatusCode, hit.Load())
	}
}

func TestCleanError(t *testing.T) {
	wrapped := &url.Error{Op: "Post", URL: "https://x.example/private-token", Err: errors.New("connection refused")}
	if got := cleanError(wrapped); got != "connection refused" {
		t.Errorf("got %q", got)
	}
	if got := cleanError(errors.New("bad\x00byte\xff")); strings.ContainsAny(got, "\x00") || !strings.HasPrefix(got, "bad") {
		t.Errorf("got %q", got)
	}
	if got := cleanError(errors.New(strings.Repeat("x", 5000))); len(got) != maxErrorLength {
		t.Errorf("length = %d", len(got))
	}
}

// The webhook says what GET /v1/orders says, and neither repeats a provider's
// message or the internal URL a failed payout quotes.
func TestAnIntegratorsWebhookCarriesNoRawFailureText(t *testing.T) {
	r := newReceiver(t)
	w := integratorWorker(t, "", "")
	id := addIntegrator(t, w.DB, "A", r.URL, "secret-a")
	seedOrderFor := store.Order{ID: "ord_f", IntegratorID: id, IdempotencyKey: "k", Status: store.StateRefundQueued}
	if err := w.DB.Create(&seedOrderFor).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	raw := "payout: https://internal.example/internal/payout returned 502: provider-a says no"
	event := store.StatusEvent{OrderID: "ord_f", Kind: store.KindTransition, To: store.StateRefundQueued, Reason: raw, At: time.Now().UTC()}
	if err := w.DB.Create(&event).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	w.sweep(context.Background())

	body := string(<-r.bodies)
	if strings.Contains(body, "provider-a") || strings.Contains(body, "internal.example") {
		t.Errorf("raw text reached the integrator: %s", body)
	}
}

// The review's case: a pass running while a webhook is cleared must not close
// what was already owed, including a report waiting on a retry.
func TestClearingAWebhookLosesNothingAlreadyOwed(t *testing.T) {
	r := newReceiver(t)
	w := integratorWorker(t, "", "")
	id := addIntegrator(t, w.DB, "A", r.URL, "secret-a")
	owe(t, w.DB, id, "ord_owed")
	owe(t, w.DB, id, "ord_retrying")
	retryAt := time.Now().UTC().Add(-time.Second)
	w.DB.Model(&store.StatusEvent{}).Where("order_id = ?", "ord_retrying").
		Updates(map[string]any{"notify_after": retryAt, "notify_attempts": 2})

	if err := store.ClearWebhook(w.DB, id, time.Now().UTC().Add(time.Millisecond)); err != nil {
		t.Fatalf("clear: %v", err)
	}
	w.sweep(context.Background())

	for _, order := range []string{"ord_owed", "ord_retrying"} {
		var event store.StatusEvent
		w.DB.First(&event, "order_id = ?", order)
		if event.NotifySkippedAt != nil {
			t.Errorf("%s was closed during the gap", order)
		}
	}

	if err := store.SetWebhook(w.DB, id, r.URL, sealPrefix+"new-secret"); err != nil {
		t.Fatalf("set: %v", err)
	}
	w.sweep(context.Background())
	if got := r.received.Load(); got != 2 {
		t.Errorf("%d delivered after the new URL, want 2", got)
	}
}

// Events that happen while there is no webhook are the polling case, and are
// closed as before.
func TestEventsDuringTheGapAreClosed(t *testing.T) {
	r := newReceiver(t)
	w := integratorWorker(t, "", "")
	id := addIntegrator(t, w.DB, "A", r.URL, "secret-a")
	if err := store.ClearWebhook(w.DB, id, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatalf("clear: %v", err)
	}
	owe(t, w.DB, id, "ord_gap")

	w.sweep(context.Background())

	var event store.StatusEvent
	w.DB.First(&event, "order_id = ?", "ord_gap")
	if event.NotifySkippedAt == nil {
		t.Error("an event from the gap stayed owed")
	}
}

// A webhook cleared for good must not leave reports open for ever.
func TestOwedReportsCloseAfterTheGrace(t *testing.T) {
	r := newReceiver(t)
	w := integratorWorker(t, "", "")
	id := addIntegrator(t, w.DB, "A", r.URL, "secret-a")
	owe(t, w.DB, id, "ord_old")
	if err := store.ClearWebhook(w.DB, id, time.Now().UTC().Add(-25*time.Hour)); err != nil {
		t.Fatalf("clear: %v", err)
	}
	w.DB.Model(&store.StatusEvent{}).Where("order_id = ?", "ord_old").Update("at", time.Now().UTC().Add(-26*time.Hour))

	w.sweep(context.Background())

	owed, err := store.OwedIntegrators(w.DB, time.Now().UTC())
	if err != nil || len(owed) != 0 {
		t.Errorf("owed = %v, %v", owed, err)
	}
}
