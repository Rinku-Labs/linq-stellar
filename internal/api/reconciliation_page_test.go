package api

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Rinku-Labs/linq-stellar/internal/stellar"
	"github.com/Rinku-Labs/linq-stellar/internal/store"
	"github.com/glebarez/sqlite"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const reviewerToken = "9f2c4b7e1a8d3f6c5b0e2a9d7c4f1b8e"

type reviewerPage struct {
	t     *testing.T
	s     *Server
	db    *gorm.DB
	clock time.Time
}

func newReviewerPage(t *testing.T, passphrase string) *reviewerPage {
	t.Helper()
	dsn := fmt.Sprintf("file:reviewer_%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		if sql, err := db.DB(); err == nil {
			_ = sql.Close()
		}
	})
	sponsor, _ := keypair.Random()
	chain, err := stellar.New(stellar.Config{
		SponsorSeed: sponsor.Seed(), EncryptionKey: []byte("0123456789abcdef0123456789abcdef"),
		NetworkPassphrase: passphrase,
	})
	if err != nil {
		t.Fatalf("chain: %v", err)
	}
	p := &reviewerPage{t: t, db: db, clock: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	p.s = &Server{
		DB: db, Chain: chain, OrdersAPIKey: "test-orders-key", ReviewerToken: reviewerToken,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return p.clock },
	}
	return p
}

func (p *reviewerPage) request(method, path string, body string, header map[string]string) *httptest.ResponseRecorder {
	p.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	p.s.Routes().ServeHTTP(w, req)
	return w
}

// signIn returns the session cookie a correct token earns.
func (p *reviewerPage) signIn() *http.Cookie {
	p.t.Helper()
	w := p.request(http.MethodPost, "/reconciliation/sign-in", "token="+url.QueryEscape(reviewerToken), nil)
	if w.Code != http.StatusSeeOther {
		p.t.Fatalf("sign in: %d %s", w.Code, w.Body)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == reviewerCookie {
			return c
		}
	}
	p.t.Fatal("no session cookie")
	return nil
}

func (p *reviewerPage) get(path string) string {
	p.t.Helper()
	c := p.signIn()
	w := p.request(http.MethodGet, path, "", map[string]string{"Cookie": c.Name + "=" + c.Value})
	if w.Code != http.StatusOK {
		p.t.Fatalf("GET %s: %d %s", path, w.Code, w.Body)
	}
	return w.Body.String()
}

// cycle records a cycle with its items. A nil finished time means it has not
// ended.
func (p *reviewerPage) cycle(started time.Time, finished *time.Time, errText string, items ...store.ReconciliationItem) store.ReconciliationCycle {
	p.t.Helper()
	c, err := store.StartCycle(p.db, started)
	if err != nil {
		p.t.Fatal(err)
	}
	for i := range items {
		items[i].CycleID = c.ID
		if err := store.AddReconciliationItem(p.db, &items[i]); err != nil {
			p.t.Fatal(err)
		}
		switch items[i].Result {
		case store.ReconcileOK:
			c.Matched++
		case store.ReconcileMismatch:
			c.Mismatches++
		}
	}
	c.Error = errText
	if finished != nil {
		if err := store.FinishCycle(p.db, c, *finished); err != nil {
			p.t.Fatal(err)
		}
	}
	got, _ := store.GetCycle(p.db, c.ID)
	return got
}

func at(t time.Time) *time.Time { return &t }

func TestThePageExistsOnlyWithAToken(t *testing.T) {
	p := newReviewerPage(t, "")
	p.s.ReviewerToken = ""
	// Not served at all: the same answer as an address this service has never
	// had.
	unknown := p.request(http.MethodGet, "/no-such-page", "", nil)
	for _, path := range []string{"/reconciliation", "/reconciliation/mismatches", "/reconciliation/cycles/1"} {
		w := p.request(http.MethodGet, path, "", nil)
		if w.Code != unknown.Code || w.Code == http.StatusOK || strings.Contains(w.Body.String(), "Reviewer token") {
			t.Errorf("%s with no REVIEWER_TOKEN: %d, want %d like any unknown address", path, w.Code, unknown.Code)
		}
	}
	if w := p.request(http.MethodPost, "/reconciliation/sign-in", "token=", nil); w.Code == http.StatusSeeOther {
		t.Error("an empty token must not sign anyone in")
	}
}

func TestOnlyTheTokenOpensThePage(t *testing.T) {
	p := newReviewerPage(t, "")
	p.cycle(p.clock.Add(-time.Hour), at(p.clock.Add(-time.Hour+time.Minute)), "",
		store.ReconciliationItem{OrderID: "ord_secret", CheckName: "deposit_tx", Result: store.ReconcileOK})

	for name, header := range map[string]map[string]string{
		"nothing":          nil,
		"a wrong token":    {"Authorization": "Bearer " + strings.Repeat("x", 32)},
		"the token prefix": {"Authorization": "Bearer " + reviewerToken[:20]},
		"a forged cookie":  {"Cookie": reviewerCookie + "=" + reviewerToken},
	} {
		for _, path := range []string{"/reconciliation", "/reconciliation/cycles/1", "/reconciliation/mismatches"} {
			w := p.request(http.MethodGet, path, "", header)
			if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "Reviewer token") || strings.Contains(w.Body.String(), "ord_secret") {
				t.Errorf("%s on %s: %d, should see only the sign-in form", name, path, w.Code)
			}
		}
	}

	w := p.request(http.MethodPost, "/reconciliation/sign-in", "token=wrong", nil)
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "That token is not right.") {
		t.Errorf("wrong token: %d", w.Code)
	}

	c := p.signIn()
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/reconciliation" || c.MaxAge != 30*24*60*60 {
		t.Errorf("cookie should be HttpOnly, Lax, scoped to the page and last 30 days: %+v", c)
	}
	if strings.Contains(c.Value, reviewerToken) {
		t.Error("the cookie must not hold the token itself")
	}
	if w := p.request(http.MethodGet, "/reconciliation", "", map[string]string{"Cookie": c.Name + "=" + c.Value}); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Cycles completed") {
		t.Errorf("signed in: %d", w.Code)
	}
	if w := p.request(http.MethodGet, "/reconciliation", "", map[string]string{"Authorization": "Bearer " + reviewerToken}); w.Code != http.StatusOK {
		t.Errorf("the token as a Bearer header, for scripts: %d", w.Code)
	}

	// Behind the platform's TLS proxy the cookie is Secure.
	w = p.request(http.MethodPost, "/reconciliation/sign-in", "token="+reviewerToken, map[string]string{"X-Forwarded-Proto": "https"})
	if cs := w.Result().Cookies(); len(cs) != 1 || !cs[0].Secure {
		t.Errorf("cookie over https: %+v", cs)
	}

	// Changing the token ends every session made with the old one.
	p.s.ReviewerToken = "a-new-reviewer-token-0123456789abcdef"
	if w := p.request(http.MethodGet, "/reconciliation", "", map[string]string{"Cookie": c.Name + "=" + c.Value}); w.Code != http.StatusUnauthorized {
		t.Errorf("old session after the token changed: %d", w.Code)
	}

	w = p.request(http.MethodPost, "/reconciliation/sign-out", "", nil)
	if cs := w.Result().Cookies(); w.Code != http.StatusSeeOther || len(cs) != 1 || cs[0].MaxAge >= 0 {
		t.Errorf("sign out should clear the cookie: %d %+v", w.Code, cs)
	}
}

// A reviewer is handed the token, so it must open the page and nothing else,
// whichever header it is put in.
func TestTheTokenOpensNothingElse(t *testing.T) {
	p := newReviewerPage(t, "")
	p.s.PublicAPIEnabled = true
	p.s.AdminAPIKey = "a-separate-admin-key-0123456789abcdef"
	session := p.signIn()

	for name, header := range map[string]map[string]string{
		"as X-Admin-Key":  {"X-Admin-Key": reviewerToken},
		"as X-API-Key":    {"X-API-Key": reviewerToken},
		"as a Bearer":     {"Authorization": "Bearer " + reviewerToken},
		"as the cookie":   {"Cookie": session.Name + "=" + session.Value},
		"as a raw cookie": {"Cookie": reviewerCookie + "=" + reviewerToken},
	} {
		for _, route := range []struct{ method, path string }{
			{http.MethodGet, "/admin/credential-events"},
			{http.MethodPost, "/admin/reconciliation/items/1/resolve"},
			{http.MethodPost, "/admin/integrators"},
			{http.MethodGet, "/orders/ord_1"},
			{http.MethodGet, "/v1/orders/ord_1"},
		} {
			if w := p.request(route.method, route.path, "", header); w.Code != http.StatusUnauthorized {
				t.Errorf("reviewer token %s on %s %s: %d, want 401", name, route.method, route.path, w.Code)
			}
		}
	}
}

func TestTheOverviewShowsEveryCycle(t *testing.T) {
	p := newReviewerPage(t, "")
	hour := time.Hour
	finished := p.cycle(p.clock.Add(-3*hour), at(p.clock.Add(-3*hour+42*time.Second)), "",
		store.ReconciliationItem{OrderID: "o1", CheckName: "deposit_tx", Result: store.ReconcileOK},
		store.ReconciliationItem{OrderID: "o2", CheckName: "payout", Result: store.ReconcileMismatch})
	stopped := p.cycle(p.clock.Add(-2*hour), at(p.clock.Add(-2*hour+time.Second)), "stopped Horizon checks early: rate limit")
	died := p.cycle(p.clock.Add(-90*time.Minute), nil, "")
	running := p.cycle(p.clock.Add(-time.Minute), nil, "")

	page := p.get("/reconciliation")
	for _, want := range []string{
		`<meta http-equiv="refresh" content="60">`,
		`<span>Cycles completed</span><b>1</b>`,
		`<span>Mismatches found</span><b>1</b>`,
		`<span>Not yet resolved</span><b>1</b>`,
		fmt.Sprintf(`<a href="/reconciliation/cycles/%d">#%d</a>`, finished.ID, finished.ID),
		"42s",
		"Finished",
		`<span class="mismatch">Stopped early</span><br><span class="muted">stopped Horizon checks early: rate limit</span>`,
		`<span class="mismatch">Did not finish</span>`,
		`<span>Finished</span>`,
		"Did not finish",
		"Running",
		fmt.Sprintf(`<a class="mismatch" href="/reconciliation/cycles/%d?only=mismatches">1</a>`, finished.ID),
		"(public network)",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("overview is missing %q", want)
		}
	}
	// Newest first.
	if strings.Index(page, fmt.Sprintf("#%d<", running.ID)) > strings.Index(page, fmt.Sprintf("#%d<", died.ID)) ||
		strings.Index(page, fmt.Sprintf("#%d<", stopped.ID)) > strings.Index(page, fmt.Sprintf("#%d<", finished.ID)) {
		t.Error("cycles should be newest first")
	}
}

func TestOlderCyclesArePaged(t *testing.T) {
	p := newReviewerPage(t, "")
	for i := 0; i < cyclesShown+5; i++ {
		start := p.clock.Add(time.Duration(i-100) * time.Minute)
		p.cycle(start, at(start.Add(time.Second)), "")
	}
	rows := regexp.MustCompile(`href="/reconciliation/cycles/\d+">#`)
	first := p.get("/reconciliation")
	if n := len(rows.FindAllString(first, -1)); n != cyclesShown {
		t.Fatalf("first page shows %d cycles, want %d", n, cyclesShown)
	}
	older := regexp.MustCompile(`href="/reconciliation\?before=(\d+)"`).FindStringSubmatch(first)
	if older == nil {
		t.Fatal("no link to older cycles")
	}
	second := p.get("/reconciliation?before=" + older[1])
	if n := len(rows.FindAllString(second, -1)); n != 5 {
		t.Errorf("second page shows %d cycles, want 5", n)
	}
	if strings.Contains(second, "?before=") {
		t.Error("the last page should have no older link")
	}
}

func TestACyclesChecksCanBeFilteredToMismatches(t *testing.T) {
	p := newReviewerPage(t, "")
	tx := "8b3c0e2f9a1d4c7b6e5f0a3d2c1b9e8f7a6d5c4b3a2f1e0d9c8b7a6f5e4d3c2b"
	acct := "GAKTRZR4GDXLB3YTTMR3XS5AHK3JGW6QVWYUAOBLY2RAKOLCSDQ2BEYL"
	c := p.cycle(p.clock.Add(-time.Hour), at(p.clock.Add(-time.Hour+time.Minute)), "",
		store.ReconciliationItem{OrderID: "ord_ok", CheckName: "deposit_tx", Result: store.ReconcileOK, TxHash: tx, Account: acct,
			Expected: "a successful payment of 0.08 USDC into the deposit account", Actual: "a successful payment of 0.08 USDC into the deposit account"},
		store.ReconciliationItem{OrderID: "ord_bad", CheckName: "sweep_to_treasury", Result: store.ReconcileMismatch, TxHash: tx,
			Expected: "1 USDC paid to the treasury", Actual: "0 USDC reached the treasury"},
		store.ReconciliationItem{CheckName: "sponsored_account", Result: store.ReconcileMismatch, Account: acct,
			Expected: "every account the sponsor pays reserves for belongs to an order or the pool", Actual: "no order and no pool record for this account"},
		store.ReconciliationItem{OrderID: "ord_down", CheckName: "payout", Result: store.ReconcileError, Actual: "could not check: timeout"})
	var bad store.ReconciliationItem
	p.db.First(&bad, "order_id = ?", "ord_bad")
	if _, err := store.ResolveReconciliationItem(p.db, bad.ID, "Induced for the SD-11 demo", "ify", p.clock); err != nil {
		t.Fatal(err)
	}

	all := p.get(fmt.Sprintf("/reconciliation/cycles/%d", c.ID))
	for _, want := range []string{
		"Deposit transaction", "Sweep reached the treasury", "Sponsored account has an order", "NGN payout matches the Linq backend",
		"https://stellar.expert/explorer/public/tx/" + tx,
		"https://stellar.expert/explorer/public/account/" + acct,
		"Induced for the SD-11 demo", "ify, 2026-11-08 12:00:00 UTC",
		"Not resolved", "No order", "could not check: timeout",
		`All checks (4)`, `Mismatches only (2)`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("cycle page is missing %q", want)
		}
	}
	only := p.get(fmt.Sprintf("/reconciliation/cycles/%d?only=mismatches", c.ID))
	if strings.Contains(only, "ord_ok") || strings.Contains(only, "ord_down") || !strings.Contains(only, "ord_bad") || !strings.Contains(only, "No order") {
		t.Error("the mismatch filter should show the two mismatches and nothing else")
	}

	for _, path := range []string{"/reconciliation/cycles/999", "/reconciliation/cycles/abc"} {
		cookie := p.signIn()
		if w := p.request(http.MethodGet, path, "", map[string]string{"Cookie": cookie.Name + "=" + cookie.Value}); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, w.Code)
		}
	}
}

func TestATestnetDeploymentLinksToTestnet(t *testing.T) {
	p := newReviewerPage(t, network.TestNetworkPassphrase)
	c := p.cycle(p.clock.Add(-time.Hour), at(p.clock), "",
		store.ReconciliationItem{OrderID: "o", CheckName: "deposit_tx", Result: store.ReconcileOK, TxHash: "abc", Account: "GABC"})
	page := p.get(fmt.Sprintf("/reconciliation/cycles/%d", c.ID))
	if !strings.Contains(page, "https://stellar.expert/explorer/testnet/tx/abc") || strings.Contains(page, "/explorer/public/") {
		t.Error("a testnet deployment should link to testnet")
	}
	if !strings.Contains(p.get("/reconciliation"), "(testnet network)") {
		t.Error("the overview should say it is testnet")
	}
}

func TestAllMismatchesAreListedTogether(t *testing.T) {
	p := newReviewerPage(t, "")
	p.cycle(p.clock.Add(-2*time.Hour), at(p.clock), "",
		store.ReconciliationItem{OrderID: "first_mismatch", CheckName: "payout", Result: store.ReconcileMismatch},
		store.ReconciliationItem{OrderID: "a_pass", CheckName: "payout", Result: store.ReconcileOK})
	p.cycle(p.clock.Add(-time.Hour), at(p.clock), "",
		store.ReconciliationItem{OrderID: "second_mismatch", CheckName: "deposit_tx", Result: store.ReconcileMismatch})
	page := p.get("/reconciliation/mismatches")
	if strings.Contains(page, "a_pass") {
		t.Error("passed checks don't belong on the mismatch list")
	}
	if a, b := strings.Index(page, "second_mismatch"), strings.Index(page, "first_mismatch"); a < 0 || b < 0 || a > b {
		t.Error("both mismatches, newest first")
	}
}

// An unexplained mismatch is found again every cycle. The counts and the list
// show it once, and a note on any copy resolves it. (Tanta's review of #7.)
func TestAProblemFoundEveryCycleIsCountedOnce(t *testing.T) {
	p := newReviewerPage(t, "")
	stuck := store.ReconciliationItem{OrderID: "ord_stuck", CheckName: "payout", Result: store.ReconcileMismatch,
		Expected: "no NGN paid", Actual: "queued, amount not reported, payout lq_1 via nomba"}
	other := store.ReconciliationItem{OrderID: "ord_other", CheckName: "account_closed", Result: store.ReconcileMismatch,
		Actual: "the account still exists, holding 0.00 USDC"}
	var copies []store.ReconciliationCycle
	for i := 3; i >= 1; i-- {
		copies = append(copies, p.cycle(p.clock.Add(-time.Duration(i)*15*time.Minute), at(p.clock), "", stuck, other))
	}
	counts := func() (int64, int64) {
		total, unresolved, err := store.MismatchCounts(p.db)
		if err != nil {
			t.Fatal(err)
		}
		return total, unresolved
	}
	if total, unresolved := counts(); total != 2 || unresolved != 2 {
		t.Errorf("two problems found three times each: %d found, %d unresolved, want 2 and 2", total, unresolved)
	}

	// Resolve the oldest copy of one: every copy is covered.
	items, _ := store.CycleItems(p.db, copies[0].ID, true)
	for _, item := range items {
		if item.OrderID == "ord_stuck" {
			if _, err := store.ResolveReconciliationItem(p.db, item.ID, "Nomba pending, watching", "ify", p.clock); err != nil {
				t.Fatal(err)
			}
		}
	}
	if total, unresolved := counts(); total != 2 || unresolved != 1 {
		t.Errorf("after one note: %d found, %d unresolved, want 2 and 1", total, unresolved)
	}

	page := p.get("/reconciliation")
	for _, want := range []string{`<span>Mismatches found</span><b>2</b>`, `<span>Not yet resolved</span><b>1</b>`} {
		if !strings.Contains(page, want) {
			t.Errorf("overview should show %s", want)
		}
	}
	list := p.get("/reconciliation/mismatches")
	if n := strings.Count(list, "ord_stuck"); n != 1 {
		t.Errorf("the stuck payout is listed %d times, want once", n)
	}
	if !strings.Contains(list, "Nomba pending, watching") {
		t.Error("the list shows the problem with its note")
	}
	// A cycle's own page still shows its copy.
	if page := p.get(fmt.Sprintf("/reconciliation/cycles/%d", copies[2].ID)); !strings.Contains(page, "ord_stuck") {
		t.Error("each cycle keeps its own record of the check")
	}
}

// The page reads only the reconciliation tables, so bank details on the orders
// they are about never reach it, nor does the token.
func TestNothingPrivateIsShown(t *testing.T) {
	p := newReviewerPage(t, "")
	p.db.Create(&store.Order{ID: "ord_private", IdempotencyKey: "ord_private", Status: store.StateSettledInTreasury,
		BankCode: "058", BankAccount: "0123456789", AccountName: "Ada Lovelace", BankName: "First Bank of Testing"})
	c := p.cycle(p.clock.Add(-time.Hour), at(p.clock), "",
		store.ReconciliationItem{OrderID: "ord_private", CheckName: "payout", Result: store.ReconcileMismatch,
			Expected: "paid ₦10000.00, payout lq_1 via nomba", Actual: "failed, amount not reported, payout lq_1 via nomba"})
	for _, path := range []string{"/reconciliation", fmt.Sprintf("/reconciliation/cycles/%d", c.ID), "/reconciliation/mismatches"} {
		page := p.get(path)
		for _, private := range []string{"0123456789", "Ada Lovelace", "First Bank of Testing", reviewerToken} {
			if strings.Contains(page, private) {
				t.Errorf("%s shows %q", path, private)
			}
		}
	}
}

func TestWhatTheEngineRecordedIsEscaped(t *testing.T) {
	p := newReviewerPage(t, "")
	c := p.cycle(p.clock.Add(-time.Hour), at(p.clock), "",
		store.ReconciliationItem{OrderID: `o"><script>alert(1)</script>`, CheckName: "payout", Result: store.ReconcileError,
			Actual: "could not check: <script>alert(2)</script>"})
	page := p.get(fmt.Sprintf("/reconciliation/cycles/%d", c.ID))
	if strings.Contains(page, "<script>") {
		t.Error("recorded text must be escaped")
	}
}

func TestThePageIsLockedDown(t *testing.T) {
	p := newReviewerPage(t, "")
	for _, w := range []*httptest.ResponseRecorder{
		p.request(http.MethodGet, "/reconciliation", "", nil),
		p.request(http.MethodGet, "/reconciliation", "", map[string]string{"Authorization": "Bearer " + reviewerToken}),
	} {
		h := w.Header()
		if !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") || h.Get("X-Frame-Options") != "DENY" ||
			h.Get("Cache-Control") != "no-store" || h.Get("Referrer-Policy") != "no-referrer" || h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("headers: %v", h)
		}
	}
}
