package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Rinku-Labs/linq-stellar/internal/store"
)

func (p *publicAPI) countEvents(t *testing.T, keyID, action string) int64 {
	t.Helper()
	var n int64
	if err := p.db.Model(&store.CredentialEvent{}).Where("key_id = ? AND action = ?", keyID, action).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// credential_events is append-only and a dead key skips the rate limit, so a row
// per request would be permanent growth at the caller's pace. Every use still
// reaches the log.
func TestRejectionsAreRecordedOncePerKeyPerMinute(t *testing.T) {
	p := newPublicAPI(t)
	id, key := p.issue(t, p.integrator(t, "Acme"))
	p.asAdmin(http.MethodPost, "/admin/keys/"+id+"/revoke", "")

	for i := 0; i < 5; i++ {
		if w := p.asKey(key, http.MethodGet, "/v1/orders/none", ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
	}
	if got := p.countEvents(t, id, store.CredentialRejectedRevoked); got != 1 {
		t.Errorf("%d rows in one minute, want 1", got)
	}

	p.clock = p.clock.Add(time.Minute)
	p.asKey(key, http.MethodGet, "/v1/orders/none", "")
	if got := p.countEvents(t, id, store.CredentialRejectedRevoked); got != 2 {
		t.Errorf("%d rows after the next minute, want 2", got)
	}
	if got := strings.Count(p.logs.String(), "rejected key used"); got != 6 {
		t.Errorf("%d log lines, want one per use", got)
	}
}

// Refusing an expired key must not scan for every other lapsed key: that made
// each request from a dead key a table scan. The worker writes the expired rows.
func TestRefusingAnExpiredKeyLeavesExpiryToTheWorker(t *testing.T) {
	p := newPublicAPI(t)
	integrator := p.integrator(t, "Acme")
	id, key := p.issue(t, integrator)
	otherID, _ := p.issue(t, integrator)
	expired := p.clock.Add(time.Hour)
	p.clock = p.clock.Add(2 * time.Hour)
	p.db.Model(&store.APIKey{}).Where("id IN ?", []string{id, otherID}).Update("expires_at", expired)

	if w := p.asKey(key, http.MethodGet, "/v1/orders/none", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if got := p.countEvents(t, id, store.CredentialExpired) + p.countEvents(t, otherID, store.CredentialExpired); got != 0 {
		t.Errorf("%d expired rows written on the request path", got)
	}

	if _, err := store.RecordExpiredKeys(p.db, p.clock); err != nil {
		t.Fatalf("worker pass: %v", err)
	}
	// However late the worker runs, the proof reads expired, then rejected.
	events, err := store.ListCredentialEvents(p.db, store.CredentialEventFilter{KeyID: id, Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var newestFirst []string
	for _, event := range events {
		newestFirst = append(newestFirst, event.Action)
	}
	wantActions(t, newestFirst, store.CredentialRejectedExpired, store.CredentialExpired, store.CredentialIssued)
}

// Whoever holds a leaked key can rotate it. Revoking only the key named would
// leave them holding its successor.
func TestRevokingALeakedKeyRevokesWhatItRotatedTo(t *testing.T) {
	p := newPublicAPI(t)
	leakedID, leaked := p.issue(t, p.integrator(t, "Acme"))

	first := decode(t, p.asKey(leaked, http.MethodPost, "/v1/keys/rotate", ""))
	second := decode(t, p.asKey(first["key"].(string), http.MethodPost, "/v1/keys/rotate", ""))

	w := p.asAdmin(http.MethodPost, "/admin/keys/"+leakedID+"/revoke", "")
	if w.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
	}
	also, _ := decode(t, w)["alsoRevoked"].([]any)
	if len(also) != 2 || also[0] != first["id"] || also[1] != second["id"] {
		t.Errorf("alsoRevoked = %v", also)
	}

	for name, key := range map[string]string{"leaked": leaked, "first": first["key"].(string), "second": second["key"].(string)} {
		if got := p.asKey(key, http.MethodGet, "/v1/orders/none", "").Code; got != http.StatusUnauthorized {
			t.Errorf("%s key: %d, want 401", name, got)
		}
	}
	for _, id := range []string{leakedID, first["id"].(string), second["id"].(string)} {
		if got := p.countEvents(t, id, store.CredentialRevoked); got != 1 {
			t.Errorf("%s has %d revoked rows, want 1", id, got)
		}
	}
}

// A key revoked before it was followed, or revoked twice, still shuts the chain.
func TestRevokingAgainStillReachesTheSuccessor(t *testing.T) {
	p := newPublicAPI(t)
	leakedID, leaked := p.issue(t, p.integrator(t, "Acme"))
	next := decode(t, p.asKey(leaked, http.MethodPost, "/v1/keys/rotate", ""))
	p.db.Model(&store.APIKey{}).Where("id = ?", leakedID).Update("revoked_at", p.clock)

	p.asAdmin(http.MethodPost, "/admin/keys/"+leakedID+"/revoke", "")
	if got := p.asKey(next["key"].(string), http.MethodGet, "/v1/orders/none", "").Code; got != http.StatusUnauthorized {
		t.Errorf("successor: %d, want 401", got)
	}
}

// Linq-B2B's orders belong to this integrator, so a key for it would read all of
// them and share their idempotency keys.
func TestLinqB2BCannotHoldAnIssuedKey(t *testing.T) {
	p := newPublicAPI(t)
	w := p.asAdmin(http.MethodPost, "/admin/integrators/"+store.LegacyIntegratorID+"/keys", "")
	if w.Code != http.StatusConflict {
		t.Errorf("issue: %d, want 409", w.Code)
	}
	var keys int64
	p.db.Model(&store.APIKey{}).Count(&keys)
	if keys != 0 {
		t.Errorf("%d keys exist", keys)
	}

	// A row put there some other way cannot be rotated into a working key.
	p.db.Create(&store.APIKey{ID: "key_x", IntegratorID: store.LegacyIntegratorID, KeyPrefix: "aaaaaaaa", SecretHash: "h"})
	if w := p.asAdmin(http.MethodPost, "/admin/keys/key_x/rotate", ""); w.Code != http.StatusConflict {
		t.Errorf("rotate: %d, want 409", w.Code)
	}
}

// A manual order pays for whatever arrives at the locked rate, so on /v1 it would
// be a free option of any size against Linq for the whole deposit window.
func TestManualDepositIsLegacyOnly(t *testing.T) {
	p := newPublicAPI(t)
	p.seedPool(t, 1)
	_, key := p.issue(t, p.integrator(t, "Acme"))
	const body = `{"idempotencyKey":"m","amountNgn":10000,"manualDeposit":true,"bankCode":"033","bankAccount":"1234567890"}`

	if w := p.asKey(key, http.MethodPost, "/v1/orders", body); w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	p.assertNothingProvisioned(t)

	legacy := `{"idempotencyKey":"m","amountNgn":10000,"rate":1655,"manualDeposit":true,"bankCode":"033","bankAccount":"1234567890"}`
	if w := p.send(http.MethodPost, "/orders", legacy, map[string]string{"X-API-Key": p.OrdersAPIKey}); w.Code != http.StatusCreated {
		t.Errorf("legacy: %d, want 201", w.Code)
	}
}

// Linq's own backend already offers manual deposits to its businesses, and
// routes their Stellar orders here. An integrator allowed by hand, and only that
// one, may create them on /v1. Everyone else is still refused, and the allowed
// one still may not set the rate.
func TestManualDepositOnlyForAnIntegratorAllowedByHand(t *testing.T) {
	p := newPublicAPI(t)
	p.seedPool(t, 2)
	allowed := p.integrator(t, "linq-v2-b2b")
	other := p.integrator(t, "Acme")
	if err := p.db.Model(&store.Integrator{}).Where("id = ?", allowed).Update("allow_manual_deposit", true).Error; err != nil {
		t.Fatal(err)
	}
	_, allowedKey := p.issue(t, allowed)
	_, otherKey := p.issue(t, other)
	const body = `{"idempotencyKey":"m1","amountNgn":10000,"manualDeposit":true,"bankCode":"033","bankAccount":"1234567890"}`

	// Another integrator: refused, and nothing provisioned.
	if w := p.asKey(otherKey, http.MethodPost, "/v1/orders", body); w.Code != http.StatusBadRequest {
		t.Fatalf("other integrator: status %d, want 400", w.Code)
	}
	p.assertNothingProvisioned(t)

	// The allowed integrator still may not choose the rate.
	withRate := `{"idempotencyKey":"m2","amountNgn":10000,"rate":5000,"manualDeposit":true,"bankCode":"033","bankAccount":"1234567890"}`
	if w := p.asKey(allowedKey, http.MethodPost, "/v1/orders", withRate); w.Code != http.StatusBadRequest {
		t.Fatalf("allowed integrator with a rate: status %d, want 400", w.Code)
	}
	p.assertNothingProvisioned(t)

	// The allowed integrator: created, as a manual order, at this service's rate.
	w := p.asKey(allowedKey, http.MethodPost, "/v1/orders", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("allowed integrator: status %d %s, want 201", w.Code, w.Body.String())
	}
	id := decode(t, w)["id"].(string)
	var order store.Order
	if err := p.db.First(&order, "id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	if !order.ManualDeposit || order.IntegratorID != allowed {
		t.Fatalf("order manualDeposit=%v integrator=%s; want true, %s", order.ManualDeposit, order.IntegratorID, allowed)
	}
	if order.Rate <= 0 || order.Rate == 5000 {
		t.Errorf("rate %v: want this service's own rate", order.Rate)
	}

	// A retry returns the same order rather than taking another account.
	if w := p.asKey(allowedKey, http.MethodPost, "/v1/orders", body); w.Code != http.StatusOK || decode(t, w)["id"] != id {
		t.Fatalf("retry: status %d, want 200 and the same order", w.Code)
	}
	var orders int64
	p.db.Model(&store.Order{}).Count(&orders)
	if orders != 1 {
		t.Fatalf("%d orders, want 1", orders)
	}

	// Taking the permission away refuses new manual orders at once.
	p.db.Model(&store.Integrator{}).Where("id = ?", allowed).Update("allow_manual_deposit", false)
	again := `{"idempotencyKey":"m3","amountNgn":10000,"manualDeposit":true,"bankCode":"033","bankAccount":"1234567890"}`
	if w := p.asKey(allowedKey, http.MethodPost, "/v1/orders", again); w.Code != http.StatusBadRequest {
		t.Fatalf("after the permission is removed: status %d, want 400", w.Code)
	}
}

// The permission is never shown or set through the API.
func TestManualDepositPermissionHasNoRoute(t *testing.T) {
	p := newPublicAPI(t)
	id := p.integrator(t, "Acme")
	p.db.Model(&store.Integrator{}).Where("id = ?", id).Update("allow_manual_deposit", true)
	for _, path := range []string{"/admin/integrators", "/admin/integrators/" + id} {
		w := p.asAdmin(http.MethodGet, path, "")
		if strings.Contains(w.Body.String(), "manual") {
			t.Errorf("GET %s shows the permission: %s", path, w.Body.String())
		}
	}
}
