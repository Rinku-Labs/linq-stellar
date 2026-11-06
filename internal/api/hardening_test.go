package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Rinku-Labs/linq-stellar/internal/store"
)

// The body cap is the difference between one read and holding whatever a caller
// chooses to send in memory.
func TestOversizedBodiesAreRefused(t *testing.T) {
	p := newPublicAPI(t)
	_, key := p.issue(t, p.integrator(t, "A"))
	padding := strings.Repeat("x", maxBodyBytes+1)
	big := `{"name":"` + padding + `"}`
	bigOrder := strings.TrimSuffix(orderBody("big"), "}") + `,"customerRef":"` + padding + `"}`

	if w := p.asKey(key, http.MethodPost, "/v1/orders", bigOrder); w.Code != http.StatusBadRequest {
		t.Errorf("/v1/orders: %d, want 400", w.Code)
	}
	if w := p.asAdmin(http.MethodPost, "/admin/integrators", big); w.Code != http.StatusBadRequest {
		t.Errorf("/admin/integrators: %d, want 400", w.Code)
	}
}

// Over-long keys used to fail at the INSERT, after a deposit account had been
// taken or paid for. Negative amounts make a quote any deposit covers.
func TestCreateRefusesWhatCouldNeverBeSaved(t *testing.T) {
	p := newPublicAPI(t)
	p.seedPool(t, 1)
	_, key := p.issue(t, p.integrator(t, "A"))

	for name, body := range map[string]string{
		"long idempotency key": fmt.Sprintf(`{"idempotencyKey":%q,"amountNgn":100,"bankCode":"033","bankAccount":"1"}`, strings.Repeat("k", 256)),
		"negative usdc":        `{"idempotencyKey":"a","amountNgn":100,"amountUsdc":-1,"bankCode":"033","bankAccount":"1"}`,
		"negative ngn":         `{"idempotencyKey":"b","amountNgn":-100,"amountUsdc":1,"bankCode":"033","bankAccount":"1"}`,
	} {
		if w := p.asKey(key, http.MethodPost, "/v1/orders", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, w.Code)
		}
	}
	var claimed int64
	p.db.Model(&store.PoolAccount{}).Where("claimed_at IS NOT NULL").Count(&claimed)
	if claimed != 0 {
		t.Errorf("%d accounts claimed by refused orders", claimed)
	}
}

func TestIntegratorNamesAreBounded(t *testing.T) {
	p := newPublicAPI(t)
	body := fmt.Sprintf(`{"name":%q}`, strings.Repeat("n", maxNameLength+1))
	if w := p.asAdmin(http.MethodPost, "/admin/integrators", body); w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// Authenticating a request has no use for the integrator's webhook secret.
func TestAKeyLookupDoesNotLoadWebhookSecrets(t *testing.T) {
	p := newPublicAPI(t)
	integrator := p.integrator(t, "A")
	p.issue(t, integrator)
	p.db.Model(&store.Integrator{}).Where("id = ?", integrator).
		Update("webhook_secret_encrypted", "sealed")

	var key store.APIKey
	if err := p.db.First(&key, "integrator_id = ?", integrator).Error; err != nil {
		t.Fatalf("load: %v", err)
	}
	found, err := store.FindKey(p.db, key.KeyPrefix)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if found.Integrator.WebhookSecretEncrypted != "" {
		t.Error("authenticating a request loaded the webhook secret")
	}
	if found.Integrator.Status != store.IntegratorActive || found.ID != key.ID {
		t.Errorf("found = %+v", found)
	}
}

// statusReason is the latest transition that gave a reason, and a failed
// delivery's reason is never mistaken for one.
func TestStatusReasonComesFromTheLatestExplainedTransition(t *testing.T) {
	p := newPublicAPI(t)
	p.db.Create(&store.Order{ID: "o", IdempotencyKey: "k", Status: store.StatePayoutQueued})
	for i, event := range []store.StatusEvent{
		{Kind: store.KindTransition, To: "a", Reason: "older"},
		{Kind: store.KindTransition, To: "b", Reason: "newest"},
		{Kind: store.KindTransition, To: "c"},
		{Kind: store.KindNotification, To: "c", Reason: "webhook failed"},
	} {
		event.OrderID, event.At = "o", p.clock.Add(time.Duration(i)*time.Second)
		if err := p.db.Create(&event).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	w := p.send(http.MethodGet, "/orders/o", "", map[string]string{"X-API-Key": p.OrdersAPIKey})
	if got := decode(t, w)["statusReason"]; got != "newest" {
		t.Errorf("statusReason = %v, want newest", got)
	}
}
