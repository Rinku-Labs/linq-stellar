package config

import (
	"strings"
	"testing"
)

func required(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"DATABASE_URL":            "postgres://localhost/test",
		"STELLAR_SPONSOR_KEY":     "SXXX",
		"STELLAR_TREASURY_WALLET": "GXXX",
		"ENCRYPTION_KEY":          "0123456789abcdef0123456789abcdef",
		"LINQ_API_URL":            "https://linq.example",
		"LINQ_INTERNAL_SECRET":    "secret",
	} {
		t.Setenv(k, v)
	}
}

// The reviewer token is the reconciliation page's only lock, and it is handed
// to people outside the team.
func TestAShortReviewerTokenIsRefused(t *testing.T) {
	required(t)
	t.Setenv("REVIEWER_TOKEN", "short")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "REVIEWER_TOKEN must be at least 32 characters") {
		t.Errorf("short token: %v", err)
	}
	t.Setenv("REVIEWER_TOKEN", strings.Repeat("a", 64))
	if c, err := Load(); err != nil || c.ReviewerToken == "" {
		t.Errorf("long token: %v", err)
	}
	t.Setenv("REVIEWER_TOKEN", "")
	if c, err := Load(); err != nil || c.ReviewerToken != "" {
		t.Errorf("no token is allowed, and means no page: %v", err)
	}
}

func TestReconciliationSettings(t *testing.T) {
	required(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ReconcileInterval.Minutes() != 15 || c.ReconcileHorizonCalls != 200 || c.ReconcilePayoutCalls != 50 || c.ReconcileSponsoredAccounts {
		t.Errorf("defaults: %+v", c)
	}
	t.Setenv("RECONCILE_INTERVAL", "10s")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "RECONCILE_INTERVAL must be at least 1m") {
		t.Errorf("an interval under a minute: %v", err)
	}
}

// The fake payout provider reports payouts it never made, so it is refused
// anywhere but a test network, and a fixed rate is refused anywhere but beside
// it.
func TestTheFakeProviderIsOnlyForATestNetwork(t *testing.T) {
	const testnet = "Test SDF Network ; September 2015"
	const mainnet = "Public Global Stellar Network ; September 2015"
	fake := func(t *testing.T, passphrase, rate string) (Config, error) {
		t.Helper()
		required(t)
		t.Setenv("LINQ_API_URL", "")
		t.Setenv("LINQ_INTERNAL_SECRET", "")
		t.Setenv("PAYOUT_PROVIDER", "fake")
		t.Setenv("STELLAR_NETWORK_PASSPHRASE", passphrase)
		t.Setenv("TEST_RATE_NGN", rate)
		return Load()
	}
	refused := func(t *testing.T, err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want refusal %q, got %v", want, err)
		}
	}

	t.Run("on testnet it needs nothing from Linq", func(t *testing.T) {
		c, err := fake(t, testnet, "1500")
		if err != nil || c.PayoutProvider != PayoutProviderFake || c.TestRateNGN != 1500 {
			t.Errorf("%+v %v", c.PayoutProvider, err)
		}
	})
	t.Run("refused on mainnet", func(t *testing.T) {
		_, err := fake(t, mainnet, "1500")
		refused(t, err, "PAYOUT_PROVIDER=fake is refused on mainnet")
	})
	t.Run("refused with no network named, which means mainnet", func(t *testing.T) {
		_, err := fake(t, "", "1500")
		refused(t, err, "PAYOUT_PROVIDER=fake is refused on mainnet")
	})
	t.Run("needs a test rate", func(t *testing.T) {
		_, err := fake(t, testnet, "")
		refused(t, err, "TEST_RATE_NGN is required with PAYOUT_PROVIDER=fake")
		_, err = fake(t, testnet, "-5")
		refused(t, err, "TEST_RATE_NGN is required with PAYOUT_PROVIDER=fake")
		_, err = fake(t, testnet, "lots")
		refused(t, err, "TEST_RATE_NGN must be a number")
	})
	t.Run("a test rate is refused beside the real provider", func(t *testing.T) {
		required(t)
		t.Setenv("TEST_RATE_NGN", "1500")
		_, err := Load()
		refused(t, err, "TEST_RATE_NGN is only for PAYOUT_PROVIDER=fake")
	})
	t.Run("the real provider is the default and needs Linq", func(t *testing.T) {
		required(t)
		c, err := Load()
		if err != nil || c.PayoutProvider != PayoutProviderLinq || c.TestRateNGN != 0 {
			t.Errorf("default: %q %v %v", c.PayoutProvider, c.TestRateNGN, err)
		}
		t.Setenv("LINQ_API_URL", "")
		_, err = Load()
		refused(t, err, "LINQ_API_URL is required")
	})
	t.Run("an unknown provider is refused", func(t *testing.T) {
		required(t)
		t.Setenv("PAYOUT_PROVIDER", "nomba")
		_, err := Load()
		refused(t, err, `PAYOUT_PROVIDER must be "linq" or "fake", got "nomba"`)
	})
}
