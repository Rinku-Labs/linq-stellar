package api

import (
	"errors"
	"net/http"
	"testing"

	"github.com/Rinku-Labs/linq-stellar/internal/settle"
	"github.com/Rinku-Labs/linq-stellar/internal/store"
)

// stubRate is a RateSource a test can move, break and count.
type stubRate struct {
	rate  float64
	err   error
	calls int
}

func (s *stubRate) Rate() (float64, error) {
	s.calls++
	return s.rate, s.err
}

func (p *publicAPI) assertNothingProvisioned(t *testing.T) {
	t.Helper()
	var orders, claimed int64
	p.db.Model(&store.Order{}).Count(&orders)
	p.db.Model(&store.PoolAccount{}).Where("claimed_at IS NOT NULL").Count(&claimed)
	if orders != 0 || claimed != 0 {
		t.Errorf("%d orders and %d claimed accounts, want none", orders, claimed)
	}
}

// A caller who sets the rate, or both sides of the quote, sets what a deposit is
// worth. Settlement pays QuotedNGN once QuotedUSDC arrives, so any of these would
// turn one cent of USDC into whatever naira it names.
func TestAKeyCannotNameTheRate(t *testing.T) {
	const bank = `"bankCode":"033","bankAccount":"1234567890"}`
	for name, body := range map[string]string{
		"both amounts and a rate": `{"idempotencyKey":"a","amountUsdc":0.01,"amountNgn":10000000,"rate":1,` + bank,
		"both amounts":            `{"idempotencyKey":"b","amountUsdc":0.01,"amountNgn":10000000,` + bank,
		"a rate":                  `{"idempotencyKey":"c","amountNgn":10000000,"rate":1000000000,` + bank,
		"a negative rate":         `{"idempotencyKey":"d","amountNgn":10000000,"rate":-1,` + bank,
	} {
		t.Run(name, func(t *testing.T) {
			p := newPublicAPI(t)
			p.seedPool(t, 1)
			_, key := p.issue(t, p.integrator(t, "Partner"))

			w := p.asKey(key, http.MethodPost, "/v1/orders", body)
			if w.Code != http.StatusBadRequest {
				var order store.Order
				p.db.First(&order)
				paid, _ := settle.Reconcile(settle.Deposit{
					Received: order.QuotedUSDC, Rate: order.Rate, QuotedUSDC: order.QuotedUSDC, QuotedNGN: order.QuotedNGN,
				})
				t.Fatalf("status = %d, want 400; %v USDC would have paid out NGN %.2f", w.Code, order.QuotedUSDC, paid.PayoutNGN)
			}
			p.assertNothingProvisioned(t)
		})
	}
}

// Whichever amount the caller sends, the other is derived from the service's
// rate, and that is what settlement pays.
func TestAV1OrderIsPricedAtTheServiceRate(t *testing.T) {
	const bank = `"bankCode":"033","bankAccount":"1234567890"}`
	for name, tc := range map[string]struct {
		body      string
		usdc, ngn float64
	}{
		"from naira": {`{"idempotencyKey":"n","amountNgn":15000,` + bank, 10, 15000},
		"from usdc":  {`{"idempotencyKey":"u","amountUsdc":10,` + bank, 10, 15000},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPublicAPI(t)
			p.Rates = &stubRate{rate: 1500}
			p.seedPool(t, 1)
			_, key := p.issue(t, p.integrator(t, "Partner"))

			w := p.asKey(key, http.MethodPost, "/v1/orders", tc.body)
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			if got := decode(t, w)["rate"]; got != 1500.0 {
				t.Errorf("response rate = %v, want 1500", got)
			}

			var order store.Order
			if err := p.db.First(&order).Error; err != nil {
				t.Fatalf("load: %v", err)
			}
			if order.Rate != 1500 || order.QuotedUSDC != tc.usdc || order.QuotedNGN != tc.ngn {
				t.Errorf("order = rate %v, %v USDC, %v NGN", order.Rate, order.QuotedUSDC, order.QuotedNGN)
			}
			paid, _ := settle.Reconcile(settle.Deposit{
				Received: tc.usdc, Rate: order.Rate, QuotedUSDC: order.QuotedUSDC, QuotedNGN: order.QuotedNGN,
			})
			if paid.PayoutNGN != tc.ngn {
				t.Errorf("payout = %v, want %v", paid.PayoutNGN, tc.ngn)
			}
		})
	}
}

// No rate means no order. There is nothing safe to fall back to, and an order
// that cannot be priced must cost nothing.
func TestNoRateNoOrder(t *testing.T) {
	for name, source := range map[string]RateSource{
		"the source fails": &stubRate{err: errors.New("provider token abc123 refused")},
		"no source":        nil,
	} {
		t.Run(name, func(t *testing.T) {
			p := newPublicAPI(t)
			p.Rates = source
			p.seedPool(t, 1)
			_, key := p.issue(t, p.integrator(t, "Partner"))

			w := p.asKey(key, http.MethodPost, "/v1/orders", orderBody("x"))
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", w.Code)
			}
			if got := decode(t, w)["message"]; got != "could not get the current rate right now; try again shortly" {
				t.Errorf("message = %q", got)
			}
			p.assertNothingProvisioned(t)
		})
	}
}

// A retry must return the order as first quoted, whatever the rate has done
// since, and must still work while the rate source is down.
func TestARetryKeepsItsRate(t *testing.T) {
	p := newPublicAPI(t)
	rates := &stubRate{rate: 1500}
	p.Rates = rates
	p.seedPool(t, 1)
	_, key := p.issue(t, p.integrator(t, "Partner"))

	if w := p.asKey(key, http.MethodPost, "/v1/orders", orderBody("retry")); w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	rates.rate, rates.err = 1700, errors.New("down")

	w := p.asKey(key, http.MethodPost, "/v1/orders", orderBody("retry"))
	if w.Code != http.StatusOK {
		t.Fatalf("retry: %d %s", w.Code, w.Body.String())
	}
	if got := decode(t, w)["rate"]; got != 1500.0 {
		t.Errorf("rate = %v, want 1500", got)
	}
	if rates.calls != 1 {
		t.Errorf("rate fetched %d times, want 1", rates.calls)
	}
}

// Refused requests must not spend a provider call.
func TestABadRequestFetchesNoRate(t *testing.T) {
	p := newPublicAPI(t)
	rates := &stubRate{rate: 1500}
	p.Rates = rates
	_, key := p.issue(t, p.integrator(t, "Partner"))

	if w := p.asKey(key, http.MethodPost, "/v1/orders", `{"idempotencyKey":"x","amountNgn":100}`); w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if rates.calls != 0 {
		t.Errorf("rate fetched %d times", rates.calls)
	}
}

// The legacy contract is unchanged: Linq-B2B sends the rate, and it is used.
func TestTheLegacyRouteStillTakesTheCallersRate(t *testing.T) {
	p := newPublicAPI(t)
	rates := &stubRate{rate: 1500}
	p.Rates = rates
	p.seedPool(t, 1)

	w := p.send(http.MethodPost, "/orders", legacyOrderBody("legacy"),
		map[string]string{"X-API-Key": p.OrdersAPIKey})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if got := decode(t, w)["rate"]; got != 1655.0 {
		t.Errorf("rate = %v, want the caller's 1655", got)
	}
	if rates.calls != 0 {
		t.Errorf("rate fetched %d times on the legacy route", rates.calls)
	}

	missing := p.send(http.MethodPost, "/orders",
		`{"idempotencyKey":"none","amountNgn":100,"bankCode":"033","bankAccount":"1"}`,
		map[string]string{"X-API-Key": p.OrdersAPIKey})
	if missing.Code != http.StatusBadRequest {
		t.Errorf("no rate: %d, want 400", missing.Code)
	}
}
