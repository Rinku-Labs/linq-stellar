// Command testnet-setup prepares the Stellar side of a test deployment (SD-7),
// and the secrets it needs, in one step.
//
// It makes two new testnet accounts and funds both from Friendbot:
//
//   - the sponsor, which pays the reserves for every deposit account;
//   - the treasury, which receives swept USDC, given its own USDC trustline.
//
// The treasury is funded by Friendbot rather than sponsored, so the
// reconciliation's sponsored-account check does not see an account with no
// order behind it.
//
// It prints the accounts and secrets to add to the deployment's .env, after
// the settings copied from .env.testnet.example. It only works on the Stellar
// test network: Friendbot exists nowhere else, and it never touches anything
// that holds real money.
//
//	go run ./cmd/testnet-setup >> .env
//	docker compose run --rm --no-deps -T server testnet-setup >> .env
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Rinku-Labs/linq-stellar/internal/stellar"
	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/txnbuild"
)

type settings struct {
	HorizonURL   string
	FriendbotURL string
	USDCIssuer   string
}

func main() {
	s := settings{
		HorizonURL:   envOr("STELLAR_HORIZON_URL", "https://horizon-testnet.stellar.org"),
		FriendbotURL: envOr("FRIENDBOT_URL", "https://friendbot.stellar.org"),
		USDCIssuer:   envOr("STELLAR_USDC_ISSUER", stellar.TestnetUSDCIssuer),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := run(ctx, s, os.Stdout, os.Stderr, &http.Client{Timeout: 60 * time.Second}); err != nil {
		fmt.Fprintln(os.Stderr, "testnet-setup:", err)
		os.Exit(1)
	}
}

// run does the work. Settings go to out, so they can be redirected into a
// file; progress goes to progress.
func run(ctx context.Context, s settings, out, progress io.Writer, client *http.Client) error {
	if s.USDCIssuer == stellar.DefaultUSDCIssuer {
		return fmt.Errorf("that is mainnet USDC; a test deployment uses the testnet issuer %s", stellar.TestnetUSDCIssuer)
	}
	if _, err := keypair.ParseAddress(s.USDCIssuer); err != nil {
		return fmt.Errorf("STELLAR_USDC_ISSUER is not a Stellar account: %w", err)
	}

	sponsor, err := keypair.Random()
	if err != nil {
		return err
	}
	treasury, err := keypair.Random()
	if err != nil {
		return err
	}

	for _, account := range []struct{ name, address string }{
		{"sponsor", sponsor.Address()},
		{"treasury", treasury.Address()},
	} {
		fmt.Fprintf(progress, "Funding the %s, %s, from Friendbot...\n", account.name, account.address)
		if err := fund(ctx, client, s.FriendbotURL, account.address); err != nil {
			return fmt.Errorf("fund the %s: %w", account.name, err)
		}
	}

	fmt.Fprintln(progress, "Giving the treasury its USDC trustline...")
	horizon := &horizonclient.Client{HorizonURL: strings.TrimRight(s.HorizonURL, "/") + "/", HTTP: client}
	hash, err := trustUSDC(horizon, treasury, s.USDCIssuer)
	if err != nil {
		return fmt.Errorf("add the treasury's USDC trustline: %w", err)
	}
	fmt.Fprintf(progress, "Done: trustline transaction %s\n\n", hash)

	secrets := map[string]string{}
	for name, bytes := range map[string]int{
		// The encryption key is used as raw bytes: 32 hex characters is
		// AES-256's 32 bytes.
		"ENCRYPTION_KEY": 16,
		"ORDERS_API_KEY": 32,
		"ADMIN_API_KEY":  32,
		"REVIEWER_TOKEN": 32,
	} {
		if secrets[name], err = randomHex(bytes); err != nil {
			return err
		}
	}

	_, err = fmt.Fprintf(out, `# ---- Made by testnet-setup on %s, for a test deployment only ----
STELLAR_SPONSOR_KEY=%s
STELLAR_TREASURY_WALLET=%s
ENCRYPTION_KEY=%s
ORDERS_API_KEY=%s
ADMIN_API_KEY=%s
REVIEWER_TOKEN=%s
# The treasury's secret. The service never needs it; keep it if you want to
# move test USDC out of the treasury.
# TREASURY_SECRET=%s
`, time.Now().UTC().Format("2006-01-02"), sponsor.Seed(), treasury.Address(), secrets["ENCRYPTION_KEY"],
		secrets["ORDERS_API_KEY"], secrets["ADMIN_API_KEY"], secrets["REVIEWER_TOKEN"], treasury.Seed())
	return err
}

// fund asks Friendbot to create and fund an account with test XLM.
func fund(ctx context.Context, client *http.Client, friendbot, address string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, friendbot+"?addr="+url.QueryEscape(address), nil)
	if err != nil {
		return err
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("friendbot answered %d: %s (it limits how often it funds; wait a minute and run this again)",
			res.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// trustUSDC adds a USDC trustline to an account, signed by that account.
func trustUSDC(horizon *horizonclient.Client, account *keypair.Full, issuer string) (string, error) {
	detail, err := horizon.AccountDetail(horizonclient.AccountRequest{AccountID: account.Address()})
	if err != nil {
		return "", err
	}
	usdc := txnbuild.CreditAsset{Code: "USDC", Issuer: issuer}
	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &detail,
		IncrementSequenceNum: true,
		BaseFee:              txnbuild.MinBaseFee,
		Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewTimeout(120)},
		Operations: []txnbuild.Operation{
			&txnbuild.ChangeTrust{Line: usdc.MustToChangeTrustAsset(), Limit: txnbuild.MaxTrustlineLimit},
		},
	})
	if err != nil {
		return "", err
	}
	if tx, err = tx.Sign(network.TestNetworkPassphrase, account); err != nil {
		return "", err
	}
	res, err := horizon.SubmitTransaction(tx)
	if err != nil {
		return "", err
	}
	return res.Hash, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
