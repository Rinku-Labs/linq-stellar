package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Rinku-Labs/linq-stellar/internal/store"
)

const rawReason = "payout: https://internal.example/internal/payout returned 502: provider-a says account closed"

func (p *publicAPI) seedFailedPayout(t *testing.T, integrator string) {
	t.Helper()
	p.db.Create(&store.Order{ID: "o", IntegratorID: integrator, IdempotencyKey: "k", Status: store.StateRefundQueued})
	for i, to := range []string{store.StatePayoutQueued, store.StateRefundQueued} {
		event := store.StatusEvent{OrderID: "o", Kind: store.KindTransition, To: to, Reason: rawReason, At: p.clock.Add(0)}
		event.ID = uint(i + 1)
		if err := p.db.Create(&event).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

// A raw reason names the bank rail and our internal URL. An integrator is told
// what happened, not how our plumbing failed.
func TestAnIntegratorSeesNoRawFailureText(t *testing.T) {
	p := newPublicAPI(t)
	id := p.integrator(t, "A")
	_, key := p.issue(t, id)
	p.seedFailedPayout(t, id)

	w := p.asKey(key, http.MethodGet, "/v1/orders/o", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	for _, leak := range []string{"provider-a", "internal.example", "returned 502"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Errorf("%q reached the integrator: %s", leak, w.Body.String())
		}
	}
	body := decode(t, w)
	if got := body["statusReason"]; got != store.PublicReason(store.StateRefundQueued, rawReason) || got == "" {
		t.Errorf("statusReason = %v", got)
	}
	history := body["statusHistory"].([]any)
	if got := history[0].(map[string]any)["reason"]; got != store.PublicReason(store.StatePayoutQueued, rawReason) {
		t.Errorf("history reason = %v", got)
	}
}

// Linq-B2B's contract is unchanged, and it shows these to its own merchants.
func TestTheLegacyRouteKeepsTheRawReason(t *testing.T) {
	p := newPublicAPI(t)
	p.seedFailedPayout(t, store.LegacyIntegratorID)
	w := p.send(http.MethodGet, "/orders/o", "", map[string]string{"X-API-Key": p.OrdersAPIKey})
	if got := decode(t, w)["statusReason"]; got != rawReason {
		t.Errorf("statusReason = %v", got)
	}
}

// A reason added later must stay hidden until someone writes a message for it.
func TestAnUnknownReasonIsNotPassedThrough(t *testing.T) {
	if got := store.PublicReason(store.StateFailed, rawReason); got != "" {
		t.Errorf("got %q", got)
	}
	if got := store.PublicReason(store.StateRefundQueued, ""); got != "" {
		t.Errorf("no reason became %q", got)
	}
}
