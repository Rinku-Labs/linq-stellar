package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Rinku-Labs/linq-stellar/internal/store"
)

func failingOrderBody(idempotencyKey, kind string) string {
	return fmt.Sprintf(`{"idempotencyKey":%q,"amountNgn":10000,"bankCode":"033","bankAccount":"1234567890","testFailure":%q}`, idempotencyKey, kind)
}

func (p *publicAPI) allowFailures(t *testing.T, integrator string) {
	t.Helper()
	if err := p.db.Model(&store.Integrator{}).Where("id = ?", integrator).Update("allow_failure_injection", true).Error; err != nil {
		t.Fatal(err)
	}
}

// A reviewer on the test deployment, holding only a key, can try the failure
// paths: the order is created already set to fail.
func TestATestAccountCanAskForAFailure(t *testing.T) {
	p := newPublicAPI(t)
	p.seedPool(t, 3)
	id := p.integrator(t, "Reviewer")
	p.allowFailures(t, id)
	_, key := p.issue(t, id)

	for _, kind := range []string{store.InducedTransient, store.InducedTerminal, ""} {
		w := p.asKey(key, http.MethodPost, "/v1/orders", failingOrderBody("order-"+kind, kind))
		if w.Code != http.StatusCreated {
			t.Fatalf("%q: %d %s", kind, w.Code, w.Body.String())
		}
		var row store.Order
		p.db.First(&row, "id = ?", decode(t, w)["id"])
		if row.InducedFailure != kind {
			t.Errorf("asked for %q, the order will fail %q", kind, row.InducedFailure)
		}
	}
}

// An account that isn't allowed is refused before anything is provisioned:
// an order that asked to fail is never quietly made to succeed.
func TestOnlyATestAccountCanAskForAFailure(t *testing.T) {
	p := newPublicAPI(t)
	p.seedPool(t, 2)
	partner := p.integrator(t, "Partner")
	_, key := p.issue(t, partner)

	w := p.asKey(key, http.MethodPost, "/v1/orders", failingOrderBody("one", store.InducedTransient))
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "testFailure is only for test accounts") {
		t.Errorf("not allowed: %d %s", w.Code, w.Body.String())
	}
	var orders, claimed int64
	p.db.Model(&store.Order{}).Count(&orders)
	p.db.Model(&store.PoolAccount{}).Where("claimed_at IS NOT NULL").Count(&claimed)
	if orders != 0 || claimed != 0 {
		t.Errorf("%d orders and %d claimed accounts, want none", orders, claimed)
	}

	tester := p.integrator(t, "Tester")
	p.allowFailures(t, tester)
	_, testKey := p.issue(t, tester)
	w = p.asKey(testKey, http.MethodPost, "/v1/orders", failingOrderBody("two", "sometimes"))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "testFailure must be transient or terminal") {
		t.Errorf("unknown kind: %d %s", w.Code, w.Body.String())
	}
}
