package stellar

import (
	"errors"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
)

// testClient builds a client with a throwaway sponsor. Nothing here touches
// Horizon; these tests cover the parts that decide correctness before any
// network call happens.
func testClient(t *testing.T) *Client {
	t.Helper()
	sponsor, err := keypair.Random()
	if err != nil {
		t.Fatalf("generate sponsor: %v", err)
	}
	c, err := New(Config{
		SponsorSeed:       sponsor.Seed(),
		EncryptionKey:     []byte("0123456789abcdef0123456789abcdef"),
		NetworkPassphrase: network.TestNetworkPassphrase,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

func TestSeedRoundTrips(t *testing.T) {
	c := testClient(t)
	address, encrypted, err := c.GenerateAccount()
	if err != nil {
		t.Fatalf("generate account: %v", err)
	}
	if !strings.HasPrefix(address, "G") {
		t.Errorf("address %q should start with G", address)
	}

	// The stored form must not contain the seed, or encrypting it achieved
	// nothing.
	kp, err := c.orderKeypair(encrypted)
	if err != nil {
		t.Fatalf("recover keypair: %v", err)
	}
	if strings.Contains(encrypted, kp.Seed()) {
		t.Error("encrypted seed contains the plaintext seed")
	}
	if kp.Address() != address {
		t.Errorf("recovered address %q, want %q", kp.Address(), address)
	}
}

// Two encryptions of the same seed must differ, or a repeated seed would be
// recognisable in storage from the ciphertext alone.
func TestEncryptionUsesFreshNonce(t *testing.T) {
	c := testClient(t)
	const seed = "SBBQAFLRZ3XTCPDPGZYRIQXNPYQ7VLWQXQZQZQZQZQZQZQZQZQZQZQZQ"

	first, err := c.EncryptSeed(seed)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	second, err := c.EncryptSeed(seed)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if first == second {
		t.Error("same plaintext encrypted to the same ciphertext twice")
	}

	for _, ct := range []string{first, second} {
		got, err := c.DecryptSeed(ct)
		if err != nil {
			t.Fatalf("decrypt: %v", err)
		}
		if got != seed {
			t.Errorf("decrypted %q, want %q", got, seed)
		}
	}
}

// A seed encrypted under one key must not decrypt under another. AES-GCM
// authenticates, so this should fail rather than return garbage.
func TestSeedDoesNotDecryptUnderWrongKey(t *testing.T) {
	c := testClient(t)
	_, encrypted, err := c.GenerateAccount()
	if err != nil {
		t.Fatalf("generate account: %v", err)
	}

	other := testClient(t)
	other.encKey = []byte("fedcba9876543210fedcba9876543210")
	if _, err := other.DecryptSeed(encrypted); err == nil {
		t.Error("decrypted a seed with the wrong key")
	}
}

// Misconfiguration must fail at construction. Every one of these would
// otherwise surface as a failed order with a payer already waiting.
func TestNewRejectsBadConfig(t *testing.T) {
	valid, err := keypair.Random()
	if err != nil {
		t.Fatalf("generate sponsor: %v", err)
	}
	goodKey := []byte("0123456789abcdef0123456789abcdef")

	cases := []struct {
		name string
		cfg  Config
	}{
		{"no sponsor seed", Config{EncryptionKey: goodKey}},
		{"malformed sponsor seed", Config{SponsorSeed: "not-a-seed", EncryptionKey: goodKey}},
		{"public key as sponsor seed", Config{SponsorSeed: valid.Address(), EncryptionKey: goodKey}},
		{"no encryption key", Config{SponsorSeed: valid.Seed()}},
		{"short encryption key", Config{SponsorSeed: valid.Seed(), EncryptionKey: []byte("tooshort")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Error("expected an error, got none")
			}
		})
	}
}

// Defaults must land on pubnet and Circle's issuer. Silently settling against
// the wrong issuer would mean accepting a worthless asset as payment.
func TestDefaultsTargetPubnetUSDC(t *testing.T) {
	sponsor, err := keypair.Random()
	if err != nil {
		t.Fatalf("generate sponsor: %v", err)
	}
	c, err := New(Config{
		SponsorSeed:   sponsor.Seed(),
		EncryptionKey: []byte("0123456789abcdef0123456789abcdef"),
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if c.passphrase != network.PublicNetworkPassphrase {
		t.Errorf("passphrase = %q, want pubnet", c.passphrase)
	}
	if c.usdc.Issuer != DefaultUSDCIssuer {
		t.Errorf("issuer = %q, want %q", c.usdc.Issuer, DefaultUSDCIssuer)
	}
	if c.usdc.Code != "USDC" {
		t.Errorf("code = %q, want USDC", c.usdc.Code)
	}
}

// Stellar rejects amounts with more than 7 decimal places as malformed.
func TestFormatAmountUsesStellarPrecision(t *testing.T) {
	cases := map[float64]string{
		0:          "0.0000000",
		1:          "1.0000000",
		0.73:       "0.7300000",
		1234.56789: "1234.5678900",
	}
	for in, want := range cases {
		if got := FormatAmount(in); got != want {
			t.Errorf("FormatAmount(%v) = %q, want %q", in, got, want)
		}
	}
}

// Only a stale sequence number may be retried by resubmitting the same
// transaction. Every other rejection is Stellar refusing what was asked for,
// and sending it again would at best waste a fee — at worst, for a sweep or a
// refund, move money a second time.
func TestOnlyAStaleSequenceIsRetried(t *testing.T) {
	if isBadSequence(nil) {
		t.Error("a nil error was read as a stale sequence")
	}
	if isBadSequence(errors.New("connection reset by peer")) {
		t.Error("a transport failure was read as a stale sequence; resubmitting one of those is how a payment goes out twice")
	}
}
