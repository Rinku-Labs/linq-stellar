package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Rinku-Labs/linq-stellar/internal/store"
	"github.com/stellar/go-stellar-sdk/network"
)

// The reconciliation page is how someone outside Linq, a grant reviewer say,
// sees the reconciliation engine's work: every cycle, every check, every
// mismatch and how it was explained. It is read-only and opened with
// REVIEWER_TOKEN alone.
//
// It reads the reconciliation tables, which were written without any bank
// detail. The D4 proof page (proof_page.go) also reads orders, through named
// columns that leave bank details out.

const (
	reviewerCookie = "linq_reviewer"
	// cyclesShown and mismatchesShown are how many rows a page lists before an
	// "older" link.
	cyclesShown     = 50
	mismatchesShown = 100
	// pageRefresh is how often the overview reloads itself, so its count of
	// completed cycles stays current on a screen left open.
	pageRefresh = 60
	// stillRunning is how long a cycle with no finish time is assumed to be
	// running rather than to have died partway.
	stillRunning = time.Hour
)

func (s *Server) reconciliationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /reconciliation", s.requireReviewer(s.handleReconciliation))
	mux.HandleFunc("GET /reconciliation/cycles/{id}", s.requireReviewer(s.handleReconciliationCycle))
	mux.HandleFunc("GET /reconciliation/mismatches", s.requireReviewer(s.handleReconciliationMismatches))
	mux.HandleFunc("GET /reconciliation/proof", s.requireReviewer(s.handleProof))
	mux.HandleFunc("POST /reconciliation/sign-in", s.handleReviewerSignIn)
	mux.HandleFunc("POST /reconciliation/sign-out", s.handleReviewerSignOut)
}

// sameSecret compares two secrets in constant time. Both are hashed first, so
// neither the content nor the length of the real one leaks through timing.
func sameSecret(given, want string) bool {
	if want == "" {
		return false
	}
	g, w := sha256.Sum256([]byte(given)), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(g[:], w[:]) == 1
}

// reviewerSession is what the cookie holds: a MAC of a fixed label under the
// token. It proves the browser was given the token without keeping the token
// itself, and stops working the moment REVIEWER_TOKEN is changed.
func (s *Server) reviewerSession() string {
	mac := hmac.New(sha256.New, []byte(s.ReviewerToken))
	mac.Write([]byte("linq-stellar reconciliation reviewer"))
	return hex.EncodeToString(mac.Sum(nil))
}

// isReviewer accepts the session cookie, or the token itself as a Bearer
// header for scripts.
func (s *Server) isReviewer(r *http.Request) bool {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return sameSecret(strings.TrimPrefix(auth, "Bearer "), s.ReviewerToken)
	}
	if c, err := r.Cookie(reviewerCookie); err == nil {
		return sameSecret(c.Value, s.reviewerSession())
	}
	return false
}

// pageHeaders keeps the page to itself: no scripts, no framing, nothing
// cached, and no referrer sent to the explorer it links to.
func pageHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
}

func (s *Server) requireReviewer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pageHeaders(w)
		if !s.isReviewer(r) {
			s.renderPage(w, http.StatusUnauthorized, signInPage, signInView{})
			return
		}
		next(w, r)
	}
}

func (s *Server) handleReviewerSignIn(w http.ResponseWriter, r *http.Request) {
	pageHeaders(w)
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		s.renderPage(w, http.StatusBadRequest, signInPage, signInView{Problem: "That could not be read. Paste the token and try again."})
		return
	}
	if !sameSecret(strings.TrimSpace(r.PostForm.Get("token")), s.ReviewerToken) {
		s.Log.Info("reconciliation sign-in refused")
		s.renderPage(w, http.StatusUnauthorized, signInPage, signInView{Problem: "That token is not right."})
		return
	}
	// No expiry: the cookie lasts until the browser closes.
	http.SetCookie(w, &http.Cookie{
		Name: reviewerCookie, Value: s.reviewerSession(), Path: "/reconciliation",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: overHTTPS(r),
	})
	s.Log.Info("reconciliation sign-in")
	http.Redirect(w, r, "/reconciliation", http.StatusSeeOther)
}

func (s *Server) handleReviewerSignOut(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: reviewerCookie, Value: "", Path: "/reconciliation", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: overHTTPS(r),
	})
	http.Redirect(w, r, "/reconciliation", http.StatusSeeOther)
}

// overHTTPS says whether the browser reached us over HTTPS, directly or
// through the platform's proxy.
func overHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// --- Views -------------------------------------------------------------------

type signInView struct{ Problem string }

type cycleRow struct {
	store.ReconciliationCycle
	Status string
	// Detail says why a cycle stopped early.
	Detail string
	Took   string
	// Troubled marks a cycle that did not run to its end. Mismatches are not
	// trouble: finding them is the cycle doing its job.
	Troubled bool
}

type overviewView struct {
	Refresh           int
	Completed         int64
	Mismatches        int64
	Unresolved        int64
	Cycles            []cycleRow
	OlderThan         int64
	Network           string
	LatestFinishedAt  string
	LatestFinishedRef int64
}

type itemRow struct {
	store.ReconciliationItem
	Check        string
	AccountLink  string
	TxLink       string
	ResolvedWhen string
}

type cycleView struct {
	Cycle          cycleRow
	Items          []itemRow
	MismatchesOnly bool
	All, Only      int
	Network        string
}

type mismatchesView struct {
	Items     []itemRow
	OlderThan int64
	Network   string
}

// checkLabels names each check for a reader.
var checkLabels = map[string]string{
	"open_account":      "Open deposit account holds what its order expects",
	"account_closed":    "Finished order's account is closed",
	"deposit_tx":        "Deposit transaction",
	"sweep_tx":          "Sweep transaction",
	"refund_tx":         "Refund transaction",
	"sweep_to_treasury": "Sweep reached the treasury",
	"sponsored_account": "Sponsored account has an order",
	"payout":            "NGN payout matches the Linq backend",
}

func checkLabel(name string) string {
	if label, ok := checkLabels[name]; ok {
		return label
	}
	return name
}

// explorerNetwork is Stellar Expert's name for the network this service runs
// on, so a test deployment's links open testnet rather than an empty mainnet
// page.
func (s *Server) explorerNetwork() string {
	if s.Chain != nil && s.Chain.NetworkPassphrase() == network.TestNetworkPassphrase {
		return "testnet"
	}
	return "public"
}

func explorerLink(net, kind, id string) string {
	if id == "" {
		return ""
	}
	return "https://stellar.expert/explorer/" + net + "/" + kind + "/" + url.PathEscape(id)
}

func when(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 UTC") }

func (s *Server) cycleRow(c store.ReconciliationCycle) cycleRow {
	row := cycleRow{ReconciliationCycle: c}
	switch {
	case c.FinishedAt == nil && s.now().Sub(c.StartedAt) < stillRunning:
		row.Status = "Running"
	case c.FinishedAt == nil:
		row.Status, row.Troubled = "Did not finish", true
	case c.Error != "":
		row.Status, row.Detail, row.Troubled = "Stopped early", c.Error, true
	default:
		row.Status = "Finished"
	}
	if c.FinishedAt != nil {
		row.Took = c.FinishedAt.Sub(c.StartedAt).Round(time.Second).String()
	}
	return row
}

func (s *Server) itemRows(items []store.ReconciliationItem) []itemRow {
	net := s.explorerNetwork()
	rows := make([]itemRow, len(items))
	for i, item := range items {
		rows[i] = itemRow{
			ReconciliationItem: item,
			Check:              checkLabel(item.CheckName),
			AccountLink:        explorerLink(net, "account", item.Account),
			TxLink:             explorerLink(net, "tx", item.TxHash),
		}
		if item.ResolvedAt != nil {
			rows[i].ResolvedWhen = when(*item.ResolvedAt)
		}
	}
	return rows
}

// before reads the ?before= paging parameter. Anything that is not a positive
// ID starts from the newest.
func before(r *http.Request) int64 {
	n, err := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func (s *Server) pageFailed(w http.ResponseWriter, what string, err error) {
	s.Log.Error("could not load the reconciliation page", "part", what, "error", err)
	s.renderPage(w, http.StatusInternalServerError, errorPage, nil)
}

func (s *Server) handleReconciliation(w http.ResponseWriter, r *http.Request) {
	view := overviewView{Refresh: pageRefresh, Network: s.explorerNetwork()}
	var err error
	if view.Completed, err = store.CompletedCycles(s.DB); err != nil {
		s.pageFailed(w, "completed cycles", err)
		return
	}
	if view.Mismatches, view.Unresolved, err = store.MismatchCounts(s.DB); err != nil {
		s.pageFailed(w, "mismatch counts", err)
		return
	}
	cycles, err := store.ListCycles(s.DB, before(r), cyclesShown)
	if err != nil {
		s.pageFailed(w, "cycles", err)
		return
	}
	for _, c := range cycles {
		view.Cycles = append(view.Cycles, s.cycleRow(c))
	}
	if len(cycles) == cyclesShown {
		view.OlderThan = cycles[len(cycles)-1].ID
	}
	s.renderPage(w, http.StatusOK, overviewPage, view)
}

func (s *Server) handleReconciliationCycle(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderPage(w, http.StatusNotFound, notFoundPage, nil)
		return
	}
	cycle, err := store.GetCycle(s.DB, id)
	if errors.Is(err, store.ErrCycleNotFound) {
		s.renderPage(w, http.StatusNotFound, notFoundPage, nil)
		return
	}
	if err != nil {
		s.pageFailed(w, "cycle", err)
		return
	}
	only := r.URL.Query().Get("only") == "mismatches"
	all, err := store.CycleItems(s.DB, id, false)
	if err != nil {
		s.pageFailed(w, "cycle items", err)
		return
	}
	shown := all
	mismatches := 0
	for _, item := range all {
		if item.Result == store.ReconcileMismatch {
			mismatches++
		}
	}
	if only {
		shown = nil
		for _, item := range all {
			if item.Result == store.ReconcileMismatch {
				shown = append(shown, item)
			}
		}
	}
	s.renderPage(w, http.StatusOK, cyclePage, cycleView{
		Cycle: s.cycleRow(cycle), Items: s.itemRows(shown), MismatchesOnly: only,
		All: len(all), Only: mismatches, Network: s.explorerNetwork(),
	})
}

func (s *Server) handleReconciliationMismatches(w http.ResponseWriter, r *http.Request) {
	items, err := store.ListMismatches(s.DB, before(r), mismatchesShown)
	if err != nil {
		s.pageFailed(w, "mismatches", err)
		return
	}
	view := mismatchesView{Items: s.itemRows(items), Network: s.explorerNetwork()}
	if len(items) == mismatchesShown {
		view.OlderThan = items[len(items)-1].ID
	}
	s.renderPage(w, http.StatusOK, mismatchesPage, view)
}

func (s *Server) renderPage(w http.ResponseWriter, status int, page *template.Template, data any) {
	w.WriteHeader(status)
	if err := page.ExecuteTemplate(w, "layout", data); err != nil {
		s.Log.Error("could not render the reconciliation page", "error", err)
	}
}

// --- Templates ---------------------------------------------------------------

const layoutHTML = `{{define "layout"}}<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
{{block "refresh" .}}{{end}}
<title>Linq reconciliation</title>
<style>
  :root { --ink:#16181d; --muted:#5d6472; --line:#e3e6eb; --bg:#f7f8fa; --ok:#127a3f; --bad:#b42318; --warn:#9a6700; --link:#1d4ed8; }
  * { box-sizing: border-box; }
  body { margin: 0; font: 15px/1.5 system-ui, -apple-system, "Segoe UI", Roboto, sans-serif; color: var(--ink); background: var(--bg); }
  header { background: #fff; border-bottom: 1px solid var(--line); padding: 14px 20px; display: flex; gap: 16px; align-items: center; flex-wrap: wrap; }
  header b { font-size: 17px; }
  header nav { display: flex; gap: 14px; flex-wrap: wrap; }
  header form { margin-left: auto; }
  main { max-width: 1200px; margin: 0 auto; padding: 20px; }
  h1 { font-size: 22px; margin: 0 0 6px; }
  p.lead { color: var(--muted); margin: 0 0 18px; max-width: 760px; }
  a { color: var(--link); }
  .cards { display: grid; grid-template-columns: repeat(auto-fit, minmax(180px, 1fr)); gap: 12px; margin-bottom: 20px; }
  .card { background: #fff; border: 1px solid var(--line); border-radius: 10px; padding: 14px 16px; }
  .card span { color: var(--muted); font-size: 13px; display: block; }
  .card b { font-size: 26px; }
  .scroll { overflow-x: auto; background: #fff; border: 1px solid var(--line); border-radius: 10px; }
  table { border-collapse: collapse; width: 100%; font-size: 14px; }
  th, td { text-align: left; padding: 9px 12px; border-bottom: 1px solid var(--line); vertical-align: top; }
  th { font-weight: 600; color: var(--muted); background: #fbfbfc; white-space: nowrap; }
  td.num { text-align: right; font-variant-numeric: tabular-nums; }
  code { font: 12.5px ui-monospace, SFMono-Regular, Menlo, monospace; word-break: break-all; }
  .ok { color: var(--ok); font-weight: 600; }
  .mismatch { color: var(--bad); font-weight: 600; }
  .error { color: var(--warn); font-weight: 600; }
  .muted { color: var(--muted); }
  .filters { display: flex; gap: 14px; margin: 0 0 12px; }
  .filters .on { font-weight: 600; color: var(--ink); text-decoration: none; }
  .pager { margin: 14px 0; }
  .signin { max-width: 420px; margin: 60px auto; background: #fff; border: 1px solid var(--line); border-radius: 10px; padding: 24px; }
  .signin input { width: 100%; padding: 10px; font: inherit; border: 1px solid var(--line); border-radius: 8px; margin: 8px 0 12px; }
  button { font: inherit; padding: 9px 16px; border-radius: 8px; border: 1px solid var(--ink); background: var(--ink); color: #fff; cursor: pointer; }
  header button { background: #fff; color: var(--ink); border-color: var(--line); padding: 6px 12px; }
  .problem { color: var(--bad); }
</style>
</head>
<body>
{{block "content" .}}{{end}}
</body>
</html>{{end}}`

const headerHTML = `{{define "header"}}<header>
  <b>Linq reconciliation</b>
  <nav><a href="/reconciliation">Cycles</a><a href="/reconciliation/mismatches">All mismatches</a><a href="/reconciliation/proof">D4 proof</a></nav>
  <form method="post" action="/reconciliation/sign-out"><button type="submit">Sign out</button></form>
</header>{{end}}`

const itemsTableHTML = `{{define "items"}}<div class="scroll"><table>
  <thead><tr><th>Check</th><th>Order</th><th>Result</th><th>Expected</th><th>Found</th><th>On Stellar Expert</th><th>Resolution</th></tr></thead>
  <tbody>
  {{range .}}<tr>
    <td>{{.Check}}</td>
    <td>{{if .OrderID}}<code>{{.OrderID}}</code>{{else}}<span class="muted">No order</span>{{end}}</td>
    <td class="{{.Result}}">{{.Result}}</td>
    <td>{{.Expected}}</td>
    <td>{{.Actual}}</td>
    <td>{{if .TxLink}}<a href="{{.TxLink}}" rel="noopener noreferrer" target="_blank">Transaction</a><br>{{end}}{{if .AccountLink}}<a href="{{.AccountLink}}" rel="noopener noreferrer" target="_blank">Account</a>{{end}}</td>
    <td>{{if .ResolvedWhen}}{{.Resolution}}<br><span class="muted">{{.ResolvedBy}}, {{.ResolvedWhen}}</span>{{else if eq .Result "ok"}}{{else}}<span class="muted">Not resolved</span>{{end}}</td>
  </tr>{{else}}<tr><td colspan="7" class="muted">Nothing to show.</td></tr>{{end}}
  </tbody>
</table></div>{{end}}`

const signInHTML = `{{define "content"}}<div class="signin">
  <h1>Linq reconciliation</h1>
  <p class="lead">Every 15 minutes Linq checks its settlement records against the Stellar ledger. Paste the reviewer token you were given to see the results.</p>
  {{if .Problem}}<p class="problem">{{.Problem}}</p>{{end}}
  <form method="post" action="/reconciliation/sign-in">
    <label for="token">Reviewer token</label>
    <input id="token" name="token" type="password" autocomplete="off" required autofocus>
    <button type="submit">Open</button>
  </form>
</div>{{end}}`

const overviewHTML = `{{define "refresh"}}<meta http-equiv="refresh" content="{{.Refresh}}">{{end}}
{{define "content"}}{{template "header" .}}<main>
  <h1>Reconciliation cycles</h1>
  <p class="lead">Each cycle compares Linq's settlement records with the Stellar ledger ({{.Network}} network) and with the NGN payout records, and writes down every result. It only reads: a mismatch is recorded for a person to look at, never acted on. This page reloads every minute.</p>
  <div class="cards">
    <div class="card"><span>Cycles completed</span><b>{{.Completed}}</b></div>
    <div class="card"><span>Mismatches found</span><b>{{.Mismatches}}</b></div>
    <div class="card"><span>Not yet resolved</span><b>{{.Unresolved}}</b></div>
  </div>
  <div class="scroll"><table>
    <thead><tr><th>Cycle</th><th>Started</th><th>Took</th><th>Status</th><th class="num">Orders</th><th class="num">Accounts</th><th class="num">OK</th><th class="num">Mismatches</th></tr></thead>
    <tbody>
    {{range .Cycles}}<tr>
      <td><a href="/reconciliation/cycles/{{.ID}}">#{{.ID}}</a></td>
      <td>{{when .StartedAt}}</td>
      <td>{{.Took}}</td>
      <td><span{{if .Troubled}} class="mismatch"{{end}}>{{.Status}}</span>{{if .Detail}}<br><span class="muted">{{.Detail}}</span>{{end}}</td>
      <td class="num">{{.OrdersChecked}}</td>
      <td class="num">{{.AccountsChecked}}</td>
      <td class="num">{{.Matched}}</td>
      <td class="num">{{if .Mismatches}}<a class="mismatch" href="/reconciliation/cycles/{{.ID}}?only=mismatches">{{.Mismatches}}</a>{{else}}0{{end}}</td>
    </tr>{{else}}<tr><td colspan="8" class="muted">No cycles yet. The first runs when the worker starts.</td></tr>{{end}}
    </tbody>
  </table></div>
  {{if .OlderThan}}<p class="pager"><a href="/reconciliation?before={{.OlderThan}}">Older cycles</a></p>{{end}}
</main>{{end}}`

const cycleHTML = `{{define "content"}}{{template "header" .}}<main>
  <h1>Cycle #{{.Cycle.ID}}</h1>
  <p class="lead">Started {{when .Cycle.StartedAt}}{{if .Cycle.Took}}, took {{.Cycle.Took}}{{end}}. {{.Cycle.Status}}{{if .Cycle.Detail}}: {{.Cycle.Detail}}{{end}}. Checked {{.Cycle.OrdersChecked}} orders and {{.Cycle.AccountsChecked}} accounts: {{.Cycle.Matched}} OK, {{.Cycle.Mismatches}} mismatches.</p>
  <p class="filters">
    {{if .MismatchesOnly}}<a href="/reconciliation/cycles/{{.Cycle.ID}}">All checks ({{.All}})</a><span class="on">Mismatches only ({{.Only}})</span>{{else}}<span class="on">All checks ({{.All}})</span><a href="/reconciliation/cycles/{{.Cycle.ID}}?only=mismatches">Mismatches only ({{.Only}})</a>{{end}}
  </p>
  {{template "items" .Items}}
</main>{{end}}`

const mismatchesHTML = `{{define "content"}}{{template "header" .}}<main>
  <h1>All mismatches</h1>
  <p class="lead">Every mismatch found, newest first, with how it was resolved. A mismatch nobody has explained yet is found again each cycle; it is listed once here, as last seen. Each cycle's page shows every check it made.</p>
  {{template "items" .Items}}
  {{if .OlderThan}}<p class="pager"><a href="/reconciliation/mismatches?before={{.OlderThan}}">Older mismatches</a></p>{{end}}
</main>{{end}}`

const notFoundHTML = `{{define "content"}}{{template "header" .}}<main><h1>Not found</h1><p class="lead"><a href="/reconciliation">Back to the cycles</a></p></main>{{end}}`

const errorHTML = `{{define "content"}}{{template "header" .}}<main><h1>Something went wrong</h1><p class="lead">The page could not be loaded. Try again in a minute.</p></main>{{end}}`

var pageFuncs = template.FuncMap{"when": when}

func page(parts ...string) *template.Template {
	t := template.New("layout").Funcs(pageFuncs)
	for _, p := range append([]string{layoutHTML, headerHTML, itemsTableHTML}, parts...) {
		template.Must(t.Parse(p))
	}
	return t
}

var (
	signInPage     = page(signInHTML)
	overviewPage   = page(overviewHTML)
	cyclePage      = page(cycleHTML)
	mismatchesPage = page(mismatchesHTML)
	notFoundPage   = page(notFoundHTML)
	errorPage      = page(errorHTML)
)
