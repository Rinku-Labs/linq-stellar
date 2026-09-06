package sep

import (
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/protocols/horizon"
	"github.com/stellar/go-stellar-sdk/support/render/problem"
	"github.com/stellar/go-stellar-sdk/txnbuild"
)

// stubAccounts stands in for Horizon so the three cases that matter — account
// exists, account does not, Horizon unreachable — can each be exercised.
type stubAccounts struct {
	account *horizon.Account
	err     error
}

func (s stubAccounts) AccountDetail(horizonclient.AccountRequest) (horizon.Account, error) {
	if s.err != nil {
		return horizon.Account{}, s.err
	}
	if s.account == nil {
		return horizon.Account{}, horizonclient.Error{
			Problem: problem.P{
				Type:   "https://stellar.org/horizon-errors/not_found",
				Status: 404,
			},
		}
	}
	return *s.account, nil
}

func newTestAuth(t *testing.T, accounts AccountLoader) (*Authenticator, *keypair.Full) {
	t.Helper()
	server, err := keypair.Random()
	if err != nil {
		t.Fatalf("server keypair: %v", err)
	}
	// A distinct JWT secret per authenticator, so tests that assert one
	// server's token is rejected by another are actually testing that.
	// Deployments sharing a secret genuinely do accept each other's tokens —
	// that is how HMAC works, and it is a deployment decision, not a bug.
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatalf("jwt secret: %v", err)
	}
	a, err := NewAuthenticator(AuthConfig{
		ServerSigningSeed: server.Seed(),
		HomeDomain:        "linqswitch.xyz",
		WebAuthDomain:     "linqswitch.xyz",
		NetworkPassphrase: network.TestNetworkPassphrase,
		JWTSecret:         secret,
		JWTIssuer:         "linq-stellar",
		ChallengeTimeout:  5 * time.Minute,
		Accounts:          accounts,
	})
	if err != nil {
		t.Fatalf("new authenticator: %v", err)
	}
	return a, server
}

// signChallenge does what a wallet does: sign the server's challenge.
func signChallenge(t *testing.T, challengeXDR string, client *keypair.Full) string {
	t.Helper()
	gtx, err := txnbuild.TransactionFromXDR(challengeXDR)
	if err != nil {
		t.Fatalf("parse challenge: %v", err)
	}
	tx, ok := gtx.Transaction()
	if !ok {
		t.Fatal("challenge is not a plain transaction")
	}
	signed, err := tx.Sign(network.TestNetworkPassphrase, client)
	if err != nil {
		t.Fatalf("sign challenge: %v", err)
	}
	out, err := signed.Base64()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return out
}

// The happy path for a payer whose account has never been funded — which is
// most of them.
func TestUnfundedAccountAuthenticates(t *testing.T) {
	auth, _ := newTestAuth(t, stubAccounts{})
	client, _ := keypair.Random()

	challenge, err := auth.Challenge(client.Address(), nil)
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}

	token, account, err := auth.Verify(signChallenge(t, challenge, client))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if account != client.Address() {
		t.Errorf("authenticated %s, want %s", account, client.Address())
	}

	sub, err := auth.ParseToken(token)
	if err != nil {
		t.Fatalf("parse token: %v", err)
	}
	if sub != client.Address() {
		t.Errorf("token subject %s, want %s", sub, client.Address())
	}
}

// An unsigned challenge proves nothing and must be rejected.
func TestUnsignedChallengeIsRejected(t *testing.T) {
	auth, _ := newTestAuth(t, stubAccounts{})
	client, _ := keypair.Random()

	challenge, err := auth.Challenge(client.Address(), nil)
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}
	if _, _, err := auth.Verify(challenge); err == nil {
		t.Error("an unsigned challenge authenticated")
	}
}

// Signing with a different key must not authenticate the named account.
func TestWrongSignerIsRejected(t *testing.T) {
	auth, _ := newTestAuth(t, stubAccounts{})
	client, _ := keypair.Random()
	impostor, _ := keypair.Random()

	challenge, err := auth.Challenge(client.Address(), nil)
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}
	if _, _, err := auth.Verify(signChallenge(t, challenge, impostor)); err == nil {
		t.Error("a challenge signed by the wrong key authenticated")
	}
}

// A challenge issued by another server must not be accepted, or anyone could
// mint their own.
func TestChallengeFromAnotherServerIsRejected(t *testing.T) {
	auth, _ := newTestAuth(t, stubAccounts{})
	other, _ := newTestAuth(t, stubAccounts{})
	client, _ := keypair.Random()

	challenge, err := other.Challenge(client.Address(), nil)
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}
	if _, _, err := auth.Verify(signChallenge(t, challenge, client)); err == nil {
		t.Error("a challenge from a different server authenticated")
	}
}

// A Horizon outage must fail closed. Treating it as "account not found" would
// silently downgrade a multisig account to master-key-only verification.
func TestHorizonOutageFailsClosed(t *testing.T) {
	auth, _ := newTestAuth(t, stubAccounts{err: errors.New("horizon unreachable")})
	client, _ := keypair.Random()

	challenge, err := auth.Challenge(client.Address(), nil)
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}
	_, _, err = auth.Verify(signChallenge(t, challenge, client))
	if err == nil {
		t.Fatal("authenticated despite being unable to check the account")
	}
	if !strings.Contains(err.Error(), "could not load account") {
		t.Errorf("error = %v, want a load failure", err)
	}
}

func TestChallengeRejectsBadAccount(t *testing.T) {
	auth, _ := newTestAuth(t, stubAccounts{})
	for _, bad := range []string{"", "not-an-account", "SBBQAFLRZ3XTCPDPGZYRIQXNPYQ7VLWQXQZQZQZQZQZQZQZQZQZQZQZQ"} {
		if _, err := auth.Challenge(bad, nil); err == nil {
			t.Errorf("challenge accepted %q", bad)
		}
	}
}

// A token signed with the wrong secret must not parse.
func TestTokenFromAnotherServerIsRejected(t *testing.T) {
	auth, _ := newTestAuth(t, stubAccounts{})
	other, _ := newTestAuth(t, stubAccounts{})
	client, _ := keypair.Random()

	challenge, _ := other.Challenge(client.Address(), nil)
	token, _, err := other.Verify(signChallenge(t, challenge, client))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if _, err := auth.ParseToken(token); err == nil {
		t.Error("a token signed by another server parsed")
	}
}

func TestNewAuthenticatorRejectsBadConfig(t *testing.T) {
	good, _ := keypair.Random()
	base := AuthConfig{
		ServerSigningSeed: good.Seed(),
		HomeDomain:        "linqswitch.xyz",
		WebAuthDomain:     "linqswitch.xyz",
		NetworkPassphrase: network.TestNetworkPassphrase,
		JWTSecret:         []byte("0123456789abcdef0123456789abcdef"),
	}
	cases := map[string]func(*AuthConfig){
		"no signing seed":    func(c *AuthConfig) { c.ServerSigningSeed = "" },
		"public key as seed": func(c *AuthConfig) { c.ServerSigningSeed = good.Address() },
		"no home domain":     func(c *AuthConfig) { c.HomeDomain = "" },
		"no web auth domain": func(c *AuthConfig) { c.WebAuthDomain = "" },
		"no passphrase":      func(c *AuthConfig) { c.NetworkPassphrase = "" },
		"weak jwt secret":    func(c *AuthConfig) { c.JWTSecret = []byte("short") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := base
			mutate(&cfg)
			if _, err := NewAuthenticator(cfg); err == nil {
				t.Error("expected an error, got none")
			}
		})
	}
}
