package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/Rinku-Labs/linq-stellar/internal/sep"
	"github.com/Rinku-Labs/linq-stellar/internal/stellar"
	"github.com/Rinku-Labs/linq-stellar/internal/store"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func testServer(t *testing.T) (*Server, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:apitest?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { db.Exec("DELETE FROM stellar_orders") })

	sponsor, _ := keypair.Random()
	chain, err := stellar.New(stellar.Config{
		SponsorSeed:   sponsor.Seed(),
		EncryptionKey: []byte("0123456789abcdef0123456789abcdef"),
	})
	if err != nil {
		t.Fatalf("chain: %v", err)
	}

	return &Server{
		DB:            db,
		Chain:         chain,
		HomeDomain:    "linqswitch.xyz",
		DepositWindow: 30 * time.Minute,
		OrdersAPIKey:  "test-orders-key",
		TOML: sep.TOMLConfig{
			NetworkPassphrase: "Public Global Stellar Network ; September 2015",
			OrgName:           "Linq",
			Accounts:          []string{sponsor.Address()},
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, db
}

// do sends the request with the test server's own API key attached, which is
// what every order-route test below exercises. TestOrdersRequireAPIKey checks
// the case where that header is wrong or missing.
func do(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.Header.Set("X-API-Key", s.OrdersAPIKey)
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, r)
	return w
}

func TestHealthz(t *testing.T) {
	s, _ := testServer(t)
	if got := do(t, s, http.MethodGet, "/healthz", "").Code; got != http.StatusOK {
		t.Errorf("status = %d, want 200", got)
	}
}

// Wallets fetch the toml cross-origin; without the CORS header a browser wallet
// cannot read it and SEP-10 never starts.
func TestStellarTOMLIsServedAndCORSOpen(t *testing.T) {
	s, _ := testServer(t)
	w := do(t, s, http.MethodGet, "/.well-known/stellar.toml", "")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("CORS header = %q, want *", got)
	}
	if body := w.Body.String(); !strings.Contains(body, `VERSION="2.0.0"`) {
		t.Errorf("toml missing VERSION:\n%s", body)
	}
}

// A deployment without SEP-10 must say so plainly rather than half-answer.
func TestSEP10DisabledReportsNotImplemented(t *testing.T) {
	s, _ := testServer(t)
	for _, tc := range []struct{ method, body string }{
		{http.MethodGet, ""},
		{http.MethodPost, `{"transaction":"x"}`},
	} {
		w := do(t, s, tc.method, "/sep10/auth?account=GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H", tc.body)
		if w.Code != http.StatusNotImplemented {
			t.Errorf("%s status = %d, want 501", tc.method, w.Code)
		}
	}
}

func TestTrustlineRequiresAddress(t *testing.T) {
	s, _ := testServer(t)
	if got := do(t, s, http.MethodGet, "/stellar/trustline", "").Code; got != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", got)
	}
}

// Order creation and lookup are the only routes that cost the sponsor money or
// return account details, so they are the only ones gated on the shared key.
func TestOrdersRequireAPIKey(t *testing.T) {
	s, db := testServer(t)
	db.Create(&store.Order{
		ID:             "order-locked",
		IdempotencyKey: "key-locked",
		Status:         store.StateAwaitingDeposit,
		DepositAddress: "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H",
	})

	unauth := func(method, path, body, apiKey string) int {
		var r *http.Request
		if body == "" {
			r = httptest.NewRequest(method, path, nil)
		} else {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
		}
		if apiKey != "" {
			r.Header.Set("X-API-Key", apiKey)
		}
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, r)
		return w.Code
	}

	createBody := `{"idempotencyKey":"key-locked-2","amountNgn":2000,"rate":1655,"bankCode":"033","bankAccount":"1234567890"}`
	for name, code := range map[string]int{
		"missing key on create": unauth(http.MethodPost, "/orders", createBody, ""),
		"wrong key on create":   unauth(http.MethodPost, "/orders", createBody, "not-the-key"),
		"missing key on status": unauth(http.MethodGet, "/orders/order-locked", "", ""),
		"wrong key on status":   unauth(http.MethodGet, "/orders/order-locked", "", "not-the-key"),
	} {
		if code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, code)
		}
	}

	// Every other route stays open — this is not a blanket auth wall.
	if got := unauth(http.MethodGet, "/healthz", "", ""); got != http.StatusOK {
		t.Errorf("healthz status = %d, want 200 without a key", got)
	}
	if got := unauth(http.MethodGet, "/stellar/trustline", "", ""); got != http.StatusBadRequest {
		t.Errorf("trustline status = %d, want 400 (missing address) without a key", got)
	}
}

func TestOrderStatusNotFound(t *testing.T) {
	s, _ := testServer(t)
	if got := do(t, s, http.MethodGet, "/orders/does-not-exist", "").Code; got != http.StatusNotFound {
		t.Errorf("status = %d, want 404", got)
	}
}

// Validation runs before anything is provisioned on-chain, so a bad request
// never leaves an orphaned Stellar account behind.
func TestCreateOrderValidation(t *testing.T) {
	s, _ := testServer(t)
	cases := map[string]string{
		"no idempotency key": `{"amountNgn":2000,"rate":1655,"bankCode":"033","bankAccount":"1234567890"}`,
		"no amount":          `{"idempotencyKey":"k","rate":1655,"bankCode":"033","bankAccount":"1234567890"}`,
		"no rate":            `{"idempotencyKey":"k","amountNgn":2000,"bankCode":"033","bankAccount":"1234567890"}`,
		"no bank code":       `{"idempotencyKey":"k","amountNgn":2000,"rate":1655,"bankAccount":"1234567890"}`,
		"no bank account":    `{"idempotencyKey":"k","amountNgn":2000,"rate":1655,"bankCode":"033"}`,
		"malformed json":     `{`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if got := do(t, s, http.MethodPost, "/orders", body).Code; got != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", got)
			}
		})
	}
}

// A repeated idempotency key must return the original order rather than
// provision a second deposit account and strand the first.
func TestCreateOrderIsIdempotent(t *testing.T) {
	s, db := testServer(t)
	existing := store.Order{
		ID:             "order-1",
		IdempotencyKey: "key-1",
		Status:         store.StateAwaitingDeposit,
		DepositAddress: "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H",
		AmountUSDC:     50,
		AmountNGN:      82750,
		Rate:           1655,
	}
	if err := db.Create(&existing).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	w := do(t, s, http.MethodPost, "/orders",
		`{"idempotencyKey":"key-1","amountNgn":2000,"rate":1655,"bankCode":"033","bankAccount":"1234567890"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["id"] != "order-1" {
		t.Errorf("id = %v, want the original order", body["id"])
	}

	var count int64
	db.Model(&store.Order{}).Count(&count)
	if count != 1 {
		t.Errorf("%d orders exist, want 1", count)
	}
}

// The order payload must carry a scannable SEP-7 URI with the asset pair — the
// whole reason payers do not send XLM by mistake.
func TestOrderResponseCarriesSEP7URI(t *testing.T) {
	s, db := testServer(t)
	order := store.Order{
		ID:             "order-2",
		IdempotencyKey: "key-2",
		Status:         store.StateAwaitingDeposit,
		DepositAddress: "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H",
		AmountUSDC:     50,
		Rate:           1655,
	}
	db.Create(&order)

	w := do(t, s, http.MethodGet, "/orders/order-2", "")
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)

	uri, _ := body["paymentUri"].(string)
	if !strings.HasPrefix(uri, "web+stellar:pay?") {
		t.Fatalf("paymentUri = %q", uri)
	}
	for _, want := range []string{"asset_code=USDC", "asset_issuer=" + stellar.DefaultUSDCIssuer, "origin_domain=linqswitch.xyz"} {
		if !strings.Contains(uri, want) {
			t.Errorf("paymentUri missing %q: %s", want, uri)
		}
	}
	if body["feeUsdc"] != float64(0) {
		t.Errorf("feeUsdc = %v, want 0", body["feeUsdc"])
	}
}

// A settled order has nothing left to pay, so offering a payment QR would
// invite a second deposit into an account that is already closed.
func TestTerminalOrderHasNoPaymentURI(t *testing.T) {
	s, db := testServer(t)
	db.Create(&store.Order{
		ID:             "order-3",
		IdempotencyKey: "key-3",
		Status:         store.StateSettledInTreasury,
		DepositAddress: "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H",
	})

	w := do(t, s, http.MethodGet, "/orders/order-3", "")
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	if _, present := body["paymentUri"]; present {
		t.Error("a settled order still advertised a payment uri")
	}
}
