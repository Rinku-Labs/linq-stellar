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
