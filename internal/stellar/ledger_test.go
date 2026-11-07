package stellar

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
)

// fakeHorizon serves canned Horizon replies by path, so the reads are tested
// against the JSON Horizon really sends rather than against a stand-in type.
func fakeHorizon(t *testing.T, routes map[string]string) (*Client, *[]string) {
	t.Helper()
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path+"?"+r.URL.RawQuery)
		body, ok := routes[r.URL.Path]
		switch {
		case !ok:
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"type":"https://stellar.org/horizon-errors/not_found","title":"Resource Missing","status":404}`))
		case body == "429":
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"type":"https://stellar.org/horizon-errors/rate_limit_exceeded","title":"Rate Limit Exceeded","status":429}`))
		default:
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(srv.Close)
	sponsor, _ := keypair.Random()
	c, err := New(Config{
		HorizonURL:        srv.URL,
		SponsorSeed:       sponsor.Seed(),
		EncryptionKey:     []byte("0123456789abcdef0123456789abcdef"),
		NetworkPassphrase: network.TestNetworkPassphrase,
		USDCIssuer:        issuer,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, &asked
}

const issuer = "GBBD47IF6LWK7P7MDEVSCWR7DPUWV3NY3DTQEVFL4NAT4AQH3ZLLFLA5"

func TestAccountStateReadsTheUSDCBalance(t *testing.T) {
	c, _ := fakeHorizon(t, map[string]string{
		"/accounts/GHOLDS": `{"id":"GHOLDS","account_id":"GHOLDS","balances":[
			{"balance":"9.9999900","asset_type":"native"},
			{"balance":"0.0800000","asset_type":"credit_alphanum4","asset_code":"USDC","asset_issuer":"` + issuer + `"},
			{"balance":"5.0000000","asset_type":"credit_alphanum4","asset_code":"USDC","asset_issuer":"GSOMEONEELSE"}]}`,
	})
	state, err := c.AccountState("GHOLDS")
	if err != nil || !state.Exists || !state.TrustsUSDC || state.USDC != 0.08 {
		t.Errorf("holding account: %+v %v", state, err)
	}
	gone, err := c.AccountState("GMERGED")
	if err != nil || gone.Exists {
		t.Errorf("merged account: %+v %v", gone, err)
	}
}

func TestLedgerTransactionReadsWhatMoved(t *testing.T) {
	c, asked := fakeHorizon(t, map[string]string{
		"/transactions/abc/operations": `{"_embedded":{"records":[
			{"type":"payment","type_i":1,"transaction_successful":true,"from":"GDEP","to":"GTREASURY",
			 "asset_type":"credit_alphanum4","asset_code":"USDC","asset_issuer":"` + issuer + `","amount":"0.0800000"},
			{"type":"change_trust","type_i":6,"transaction_successful":true},
			{"type":"account_merge","type_i":8,"transaction_successful":true,"account":"GDEP","into":"GSPONSOR"}]}}`,
		"/transactions/bad/operations": `{"_embedded":{"records":[
			{"type":"payment","type_i":1,"transaction_successful":false,"from":"GDEP","to":"GTREASURY",
			 "asset_type":"credit_alphanum4","asset_code":"USDC","asset_issuer":"` + issuer + `","amount":"1.0000000"}]}}`,
	})
	tx, err := c.LedgerTransaction("abc")
	if err != nil || !tx.Found || !tx.Successful || len(tx.Movements) != 2 {
		t.Fatalf("sweep: %+v %v", tx, err)
	}
	if m := tx.Movements[0]; m.Kind != "payment" || !m.USDC || m.Amount != 0.08 || m.From != "GDEP" || m.To != "GTREASURY" {
		t.Errorf("payment: %+v", m)
	}
	if m := tx.Movements[1]; m.Kind != "account_merge" || m.From != "GDEP" || m.To != "GSPONSOR" {
		t.Errorf("merge: %+v", m)
	}
	if !strings.Contains((*asked)[0], "include_failed=true") {
		t.Errorf("failed transactions must be included: %s", (*asked)[0])
	}

	failed, err := c.LedgerTransaction("bad")
	if err != nil || !failed.Found || failed.Successful {
		t.Errorf("failed tx: %+v %v", failed, err)
	}
	missing, err := c.LedgerTransaction("nothing")
	if err != nil || missing.Found {
		t.Errorf("missing tx: %+v %v", missing, err)
	}
}

func TestSponsoredAccountsPagesBySponsor(t *testing.T) {
	var records []string
	for i := 0; i < 3; i++ {
		records = append(records, fmt.Sprintf(`{"id":"GA%d","account_id":"GA%d","paging_token":"GA%d"}`, i, i, i))
	}
	c, asked := fakeHorizon(t, map[string]string{
		"/accounts": `{"_embedded":{"records":[` + strings.Join(records, ",") + `]}}`,
	})
	page, err := c.SponsoredAccounts("", 3)
	if err != nil || len(page.Accounts) != 3 || page.Next != "GA2" {
		t.Fatalf("full page: %+v %v", page, err)
	}
	if !strings.Contains((*asked)[0], "sponsor="+c.SponsorAddress()) {
		t.Errorf("search must be by our sponsor: %s", (*asked)[0])
	}
	last, err := c.SponsoredAccounts("GA2", 10)
	if err != nil || last.Next != "" {
		t.Errorf("a short page is the last: %+v %v", last, err)
	}
}

func TestARateLimitIsMarked(t *testing.T) {
	c, _ := fakeHorizon(t, map[string]string{"/accounts/GBUSY": "429", "/transactions/busy/operations": "429"})
	if _, err := c.AccountState("GBUSY"); !errors.Is(err, ErrRateLimited) {
		t.Errorf("account: %v", err)
	}
	if _, err := c.LedgerTransaction("busy"); !errors.Is(err, ErrRateLimited) {
		t.Errorf("transaction: %v", err)
	}
}
