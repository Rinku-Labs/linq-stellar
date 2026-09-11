// Package config loads and validates service configuration from the
// environment.
//
// Everything is validated once at startup. A service that settles payments
// should refuse to start with a missing sponsor key rather than discover it
// when a payer is already waiting on a deposit address.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the full service configuration.
type Config struct {
	Port        string
	LogLevel    string
	DatabaseURL string

	HorizonURL        string
	NetworkPassphrase string
	USDCIssuer        string
	SponsorKey        string
	TreasuryWallet    string
	BaseFee           int64
	EncryptionKey     []byte

	ScanInterval  time.Duration
	DepositWindow time.Duration
	MaxStreams    int
	// AccountPoolSize is how many provisioned deposit accounts to keep ready
	// for incoming orders. Zero disables the pool, returning order creation to
	// provisioning inline — and to making the payer wait a ledger for it. Each
	// idle account holds about one XLM of sponsor reserves, all of it returned
	// when the account is merged back after settlement, so the standing cost is
	// a float rather than a spend.
	AccountPoolSize int

	LinqAPIURL    string
	LinqAPISecret string

	// OrdersAPIKey guards POST /orders and GET /orders/{id} — the only routes
	// that create a deposit account or read one back. Left optional here
	// because the worker binary shares this struct and never serves HTTP; the
	// server binary is the one that requires it before it will listen.
	OrdersAPIKey string

	HomeDomain       string
	WebAuthDomain    string
	SEP10SigningKey  string
	SEP10JWTSecret   []byte
	ChallengeTimeout time.Duration

	// MerchantWebhookURL is the Linq backend endpoint told about order status
	// changes. Empty disables notification: the service still settles, but
	// nothing downstream hears about it, which is only right for a local run.
	MerchantWebhookURL string
	// MerchantWebhookSecret signs those deliveries (HMAC-SHA256, hex, sent as
	// x-linq-signature). Must match the secret the backend verifies with.
	MerchantWebhookSecret string

	OrgName        string
	OrgURL         string
	OrgDescription string
	OrgEmail       string
}

// Load reads configuration from the environment.
//
// Errors are accumulated rather than returned one at a time, so a misconfigured
// deployment gets the whole list on the first boot instead of discovering the
// next missing variable on each restart.
func Load() (Config, error) {
	c := Config{
		Port:              env("PORT", "8080"),
		LogLevel:          env("LOG_LEVEL", "info"),
		DatabaseURL:       os.Getenv("DATABASE_URL"),
		HorizonURL:        os.Getenv("STELLAR_HORIZON_URL"),
		NetworkPassphrase: os.Getenv("STELLAR_NETWORK_PASSPHRASE"),
		USDCIssuer:        os.Getenv("STELLAR_USDC_ISSUER"),
		SponsorKey:        os.Getenv("STELLAR_SPONSOR_KEY"),
		TreasuryWallet:    os.Getenv("STELLAR_TREASURY_WALLET"),
		EncryptionKey:     []byte(os.Getenv("ENCRYPTION_KEY")),
		LinqAPIURL:        os.Getenv("LINQ_API_URL"),
		LinqAPISecret:     os.Getenv("LINQ_INTERNAL_SECRET"),
		OrdersAPIKey:      os.Getenv("ORDERS_API_KEY"),

		MerchantWebhookURL:    os.Getenv("MERCHANT_WEBHOOK_URL"),
		MerchantWebhookSecret: os.Getenv("MERCHANT_WEBHOOK_SECRET"),

		HomeDomain:        os.Getenv("SEP10_HOME_DOMAIN"),
		WebAuthDomain:     env("SEP10_WEB_AUTH_DOMAIN", os.Getenv("SEP10_HOME_DOMAIN")),
		SEP10SigningKey:   os.Getenv("SEP10_SIGNING_KEY"),
		SEP10JWTSecret:    []byte(os.Getenv("SEP10_JWT_SECRET")),
		OrgName:           env("ORG_NAME", "Linq"),
		OrgURL:            os.Getenv("ORG_URL"),
		OrgDescription:    os.Getenv("ORG_DESCRIPTION"),
		OrgEmail:          os.Getenv("ORG_EMAIL"),
		ScanInterval:      duration("STELLAR_SCAN_INTERVAL", 15*time.Second),
		DepositWindow:     duration("STELLAR_DEPOSIT_WINDOW", 30*time.Minute),
		ChallengeTimeout:  duration("SEP10_CHALLENGE_TIMEOUT", 5*time.Minute),
		BaseFee:           integer("STELLAR_BASE_FEE", 0),
		MaxStreams:        int(integer("STELLAR_MAX_STREAMS", 200)),
		AccountPoolSize:   int(integer("STELLAR_ACCOUNT_POOL", 10)),
	}

	var problems []string
	if c.DatabaseURL == "" {
		problems = append(problems, "DATABASE_URL is required")
	}
	if c.SponsorKey == "" {
		problems = append(problems, "STELLAR_SPONSOR_KEY is required")
	}
	if c.TreasuryWallet == "" {
		problems = append(problems, "STELLAR_TREASURY_WALLET is required")
	}
	switch len(c.EncryptionKey) {
	case 16, 24, 32:
	default:
		problems = append(problems,
			fmt.Sprintf("ENCRYPTION_KEY must be 16, 24 or 32 bytes, got %d", len(c.EncryptionKey)))
	}
	if c.LinqAPIURL == "" {
		problems = append(problems, "LINQ_API_URL is required")
	}
	if c.LinqAPISecret == "" {
		problems = append(problems, "LINQ_INTERNAL_SECRET is required")
	}

	if len(problems) > 0 {
		return Config{}, errors.New("config: " + strings.Join(problems, "; "))
	}
	return c, nil
}

// SEP10Enabled reports whether web authentication is configured.
//
// SEP-10 is optional: the deposit-address flow works without it, and a
// half-configured authenticator that answers challenges nothing can verify is
// worse than one that is plainly switched off.
func (c Config) SEP10Enabled() bool {
	return c.SEP10SigningKey != "" && len(c.SEP10JWTSecret) >= 32 && c.HomeDomain != ""
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func duration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func integer(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return fallback
}
