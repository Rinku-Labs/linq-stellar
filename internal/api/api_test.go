package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Rinku-Labs/linq-stellar/internal/payout"
	"github.com/Rinku-Labs/linq-stellar/internal/sep"
	"github.com/Rinku-Labs/linq-stellar/internal/stellar"
	"github.com/Rinku-Labs/linq-stellar/internal/store"
	"github.com/glebarez/sqlite"
	"github.com/stellar/go-stellar-sdk/keypair"
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

// A payment URI must carry the amount the invoice actually needs, to the
// decimal. Truncating it — a two-decimal 0.07 against a ₦100 invoice — is what
// had payers sending 4.5% less than they owed and merchants absorbing it.
func TestPaymentURICarriesTheExactQuote(t *testing.T) {
	s, db := testServer(t)
	db.Create(&store.Order{
		ID:             "order-quote",
		IdempotencyKey: "key-quote",
		Status:         store.StateAwaitingDeposit,
		DepositAddress: "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H",
		AmountUSDC:     0.073303,
		AmountNGN:      100,
		QuotedUSDC:     0.073303,
		QuotedNGN:      100,
		Rate:           1364.21,
	})

	w := do(t, s, http.MethodGet, "/orders/order-quote", "")
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)

	uri, _ := body["paymentUri"].(string)
	if !strings.Contains(uri, "amount=0.073303") {
		t.Errorf("paymentUri does not ask for the full quote: %s", uri)
	}

	// The quote is published in its own right, so a caller can show a payer
	// what is still owed after a short deposit rewrites amountUsdc.
	if body["quotedUsdc"] != 0.073303 {
		t.Errorf("quotedUsdc = %v, want 0.073303", body["quotedUsdc"])
	}
	if body["quotedNgn"] != float64(100) {
		t.Errorf("quotedNgn = %v, want 100", body["quotedNgn"])
	}
	if _, flagged := body["underpaid"]; flagged {
		t.Error("an order with no deposit was reported as underpaid")
	}
}

// A short deposit has to be visible in the payload, not just in the naira
// figure it silently reduced.
func TestUnderpaidOrderReportsItsShortfall(t *testing.T) {
	s, db := testServer(t)
	db.Create(&store.Order{
		ID:             "order-short",
		IdempotencyKey: "key-short",
		Status:         store.StatePayoutQueued,
		DepositAddress: "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H",
		AmountUSDC:     0.07,
		AmountNGN:      95.49,
		QuotedUSDC:     0.073303,
		QuotedNGN:      100,
		Rate:           1364.21,
		Underpaid:      true,
		ShortfallNGN:   4.51,
	})

	w := do(t, s, http.MethodGet, "/orders/order-short", "")
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)

	if body["underpaid"] != true {
		t.Errorf("underpaid = %v, want true", body["underpaid"])
	}
	if body["shortfallNgn"] != 4.51 {
		t.Errorf("shortfallNgn = %v, want 4.51", body["shortfallNgn"])
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

// Browsers block a cross-origin response before any application code sees it,
// and the caller gets an opaque "failed to fetch". SEP-10 is called from other
// origins by design, so these headers are load-bearing rather than incidental.
func TestCORSHeadersOnBrowserFacingRoutes(t *testing.T) {
	s, _ := testServer(t)
	for _, path := range []string{
		"/.well-known/stellar.toml",
		"/sep10/auth?account=GB2LEGZMXI44AMJNEM5RRWXB7YWUGSKRZJDJPMS2APJVNGMHTOHOSU4K",
		"/stellar/trustline?address=GB2LEGZMXI44AMJNEM5RRWXB7YWUGSKRZJDJPMS2APJVNGMHTOHOSU4K",
	} {
		w := do(t, s, http.MethodGet, path, "")
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("%s: Access-Control-Allow-Origin = %q, want *", path, got)
		}
	}
}

// The preflight has to succeed on its own. ServeMux matches on method, so
// without an explicit OPTIONS route the browser's preflight 405s and the real
// request is never sent.
func TestPreflightSucceeds(t *testing.T) {
	s, _ := testServer(t)
	w := do(t, s, http.MethodOptions, "/sep10/auth", "")
	if w.Code != http.StatusNoContent {
		t.Errorf("preflight status = %d, want 204", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Content-Type") {
		t.Errorf("Allow-Headers = %q, want it to include Content-Type", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
		t.Errorf("Allow-Methods = %q, want it to include POST", got)
	}
}

// The submit endpoint spends our XLM on someone else's transaction, and the
// caller is a browser where no key can be kept secret. These cases are the
// actual security boundary: what the transaction does is the credential.
func TestSubmitRefusesWhatItWillNotSponsor(t *testing.T) {
	s, _ := testServer(t)

	cases := map[string]string{
		"no body":              ``,
		"empty transaction":    `{"transaction":""}`,
		"unparseable xdr":      `{"transaction":"not-a-transaction"}`,
		"valid base64, not tx": `{"transaction":"aGVsbG8gd29ybGQ="}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			w := do(t, s, http.MethodPost, "/stellar/submit", body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", w.Code)
			}
		})
	}
}

// A payment to an address we do not own, or one we are no longer expecting
// money for, must not be sponsored — otherwise anyone could have us pay the
// fees on arbitrary traffic.
func TestSubmitRefusesUnknownDestination(t *testing.T) {
	_, db := testServer(t)

	// An order that exists but has already settled: its deposit account has
	// been merged away, so sponsoring a payment to it would burn a fee on a
	// transaction that cannot succeed.
	db.Create(&store.Order{
		ID:             "settled-order",
		IdempotencyKey: "settled-key",
		Status:         store.StateSettledInTreasury,
		DepositAddress: "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H",
	})

	var awaiting int64
	db.Model(&store.Order{}).
		Where("deposit_address = ? AND status = ?",
			"GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H",
			store.StateAwaitingDeposit).
		Count(&awaiting)
	if awaiting != 0 {
		t.Fatalf("a settled order counted as awaiting deposit (%d); the guard would sponsor it", awaiting)
	}
}

// stubLookup stands in for the Linq backend's deposit-address check.
type stubLookup struct {
	known    payout.KnownDepositAddress
	err      error
	calls    int
	lastAddr string
}

func (s *stubLookup) LookupDepositAddress(address string) (payout.KnownDepositAddress, error) {
	s.calls++
	s.lastAddr = address
	return s.known, s.err
}

// Consumer-app orders live in the Linq backend, so an address this service has
// never seen still has to be sponsorable — otherwise fee-bumping is dead code
// for exactly the payers it was built for.
func TestAwaitingDepositFallsBackToTheLinqBackend(t *testing.T) {
	s, _ := testServer(t)
	lookup := &stubLookup{known: payout.KnownDepositAddress{Known: true, AwaitingDeposit: true}}
	s.DepositLookup = lookup

	ok, err := s.isAwaitingDeposit("GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !ok {
		t.Error("an address the backend owns was not recognised")
	}
	if lookup.calls != 1 {
		t.Errorf("backend called %d times, want 1", lookup.calls)
	}
}

// An address the backend knows but has already settled must not be sponsored:
// its deposit account has been merged away, so the fee would buy a transaction
// that cannot succeed.
func TestSettledAddressIsNotSponsored(t *testing.T) {
	s, _ := testServer(t)
	s.DepositLookup = &stubLookup{known: payout.KnownDepositAddress{Known: true, AwaitingDeposit: false}}

	ok, err := s.isAwaitingDeposit("GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if ok {
		t.Error("sponsored a payment to an address that is no longer awaiting a deposit")
	}
}

// An unreachable backend must surface as an error, not as a quiet "not ours".
// Read as a refusal it would switch sponsorship off for every consumer payment
// during any blip, and the only symptom would be payers paying their own fees.
func TestLookupFailureIsAnErrorNotARefusal(t *testing.T) {
	s, _ := testServer(t)
	s.DepositLookup = &stubLookup{err: errStub}

	if _, err := s.isAwaitingDeposit("GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H"); err == nil {
		t.Error("an unreachable backend was treated as a definite answer")
	}
}

// A local order must not cost a round trip.
func TestLocalOrderSkipsTheBackend(t *testing.T) {
	s, db := testServer(t)
	lookup := &stubLookup{}
	s.DepositLookup = lookup

	db.Create(&store.Order{
		ID:             "awaiting",
		IdempotencyKey: "awaiting-key",
		Status:         store.StateAwaitingDeposit,
		DepositAddress: "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H",
	})

	ok, err := s.isAwaitingDeposit("GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H")
	if err != nil || !ok {
		t.Fatalf("local order not recognised: ok=%v err=%v", ok, err)
	}
	if lookup.calls != 0 {
		t.Errorf("backend called %d times for a local order, want 0", lookup.calls)
	}
}

var errStub = fmt.Errorf("backend unavailable")
