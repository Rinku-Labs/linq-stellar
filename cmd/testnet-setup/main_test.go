package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Rinku-Labs/linq-stellar/internal/config"
	"github.com/Rinku-Labs/linq-stellar/internal/stellar"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/txnbuild"
)

// fakeTestnet stands in for Friendbot and Horizon, keeping what was asked.
type fakeTestnet struct {
	mu        sync.Mutex
	funded    []string
	submitted []*txnbuild.Transaction
	refuse    bool
}

func (f *fakeTestnet) server(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /friendbot", func(w http.ResponseWriter, r *http.Request) {
		if f.refuse {
			http.Error(w, `{"detail":"createAccountAlreadyExist"}`, http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.funded = append(f.funded, r.URL.Query().Get("addr"))
		f.mu.Unlock()
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /accounts/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		json.NewEncoder(w).Encode(map[string]any{"id": id, "account_id": id, "sequence": "4294967296", "balances": []any{}})
	})
	mux.HandleFunc("POST /transactions", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		parsed, err := txnbuild.TransactionFromXDR(r.PostForm.Get("tx"))
		if err != nil {
			t.Errorf("submitted XDR: %v", err)
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		tx, _ := parsed.Transaction()
		f.mu.Lock()
		f.submitted = append(f.submitted, tx)
		f.mu.Unlock()
		hash, _ := tx.HashHex(network.TestNetworkPassphrase)
		json.NewEncoder(w).Encode(map[string]any{"hash": hash, "successful": true, "ledger": 1})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// settingsIn reads KEY=VALUE lines, skipping comments.
func settingsIn(t *testing.T, text string) map[string]string {
	t.Helper()
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("not a setting: %q", line)
		}
		out[key] = value
	}
	return out
}

func TestSetupMakesATestDeploymentThatStarts(t *testing.T) {
	fake := &fakeTestnet{}
	srv := fake.server(t)
	var out, progress bytes.Buffer
	err := run(context.Background(), settings{
		HorizonURL: srv.URL, FriendbotURL: srv.URL + "/friendbot", USDCIssuer: stellar.TestnetUSDCIssuer,
	}, &out, &progress, srv.Client())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, progress.String())
	}
	made := settingsIn(t, out.String())

	sponsor, err := keypair.ParseFull(made["STELLAR_SPONSOR_KEY"])
	if err != nil {
		t.Fatalf("sponsor key: %v", err)
	}
	treasury := made["STELLAR_TREASURY_WALLET"]
	if len(fake.funded) != 2 || fake.funded[0] != sponsor.Address() || fake.funded[1] != treasury {
		t.Errorf("funded %v, want the sponsor then the treasury", fake.funded)
	}

	// One transaction: the treasury trusting testnet USDC, signed by it.
	if len(fake.submitted) != 1 {
		t.Fatalf("submitted %d transactions, want 1", len(fake.submitted))
	}
	tx := fake.submitted[0]
	ops := tx.Operations()
	trust, ok := ops[0].(*txnbuild.ChangeTrust)
	if len(ops) != 1 || !ok || tx.SourceAccount().AccountID != treasury {
		t.Fatalf("submitted %+v from %s", ops, tx.SourceAccount().AccountID)
	}
	asset, _ := trust.Line.ToChangeTrustAsset()
	if asset.GetCode() != "USDC" || asset.GetIssuer() != stellar.TestnetUSDCIssuer {
		t.Errorf("trustline for %s:%s", asset.GetCode(), asset.GetIssuer())
	}
	hash, _ := tx.Hash(network.TestNetworkPassphrase)
	signer, _ := keypair.ParseAddress(treasury)
	if sigs := tx.Signatures(); len(sigs) != 1 || signer.Verify(hash[:], sigs[0].Signature) != nil {
		t.Error("the trustline is not signed by the treasury on the test network")
	}

	// Its output, after .env.testnet.example, is a configuration the service
	// accepts: the fake provider, on testnet.
	example, err := os.ReadFile("../../.env.testnet.example")
	if err != nil {
		t.Fatal(err)
	}
	all := settingsIn(t, string(example))
	for k, v := range made {
		if _, clash := all[k]; clash {
			t.Errorf("%s is in both .env.testnet.example and the setup's output", k)
		}
		all[k] = v
	}
	for k, v := range all {
		t.Setenv(k, v)
	}
	c, err := config.Load()
	if err != nil {
		t.Fatalf("the test deployment would not start: %v", err)
	}
	if c.PayoutProvider != config.PayoutProviderFake || c.NetworkPassphrase != network.TestNetworkPassphrase ||
		c.TestRateNGN <= 0 || len(c.ReviewerToken) < 32 || !c.PublicAPIEnabled || len(c.AdminAPIKey) < 32 {
		t.Errorf("config: provider %q, network %q, rate %v, reviewer token %d chars, public API %v",
			c.PayoutProvider, c.NetworkPassphrase, c.TestRateNGN, len(c.ReviewerToken), c.PublicAPIEnabled)
	}
	if strings.Contains(progress.String(), sponsor.Seed()) {
		t.Error("the sponsor's secret was written to the progress output")
	}
}

func TestSetupRefusesMainnetUSDC(t *testing.T) {
	fake := &fakeTestnet{}
	srv := fake.server(t)
	err := run(context.Background(), settings{HorizonURL: srv.URL, FriendbotURL: srv.URL + "/friendbot", USDCIssuer: stellar.DefaultUSDCIssuer},
		&bytes.Buffer{}, &bytes.Buffer{}, srv.Client())
	if err == nil || !strings.Contains(err.Error(), "mainnet USDC") || len(fake.funded) != 0 {
		t.Errorf("mainnet USDC: %v, funded %v", err, fake.funded)
	}
}

func TestAFriendbotRefusalSaysWhatToDo(t *testing.T) {
	fake := &fakeTestnet{refuse: true}
	srv := fake.server(t)
	var out bytes.Buffer
	err := run(context.Background(), settings{HorizonURL: srv.URL, FriendbotURL: srv.URL + "/friendbot", USDCIssuer: stellar.TestnetUSDCIssuer},
		&out, &bytes.Buffer{}, srv.Client())
	if err == nil || !strings.Contains(err.Error(), "run this again") || out.Len() != 0 {
		t.Errorf("refusal: %v, printed %q", err, out.String())
	}
}
