package sep

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/protocols/horizon"
	"github.com/stellar/go-stellar-sdk/txnbuild"
)

// AccountLoader reads an account's signers and thresholds.
//
// An interface rather than a concrete client so authentication can be tested
// against accounts that exist, accounts that do not, and a Horizon that is
// down — three cases with genuinely different correct behaviour.
//
// *horizonclient.Client satisfies this directly.
type AccountLoader interface {
	AccountDetail(horizonclient.AccountRequest) (horizon.Account, error)
}

// AuthConfig configures SEP-10 web authentication.
type AuthConfig struct {
	// ServerSigningSeed (S...) signs challenges. Its public half must be
	// published as SIGNING_KEY in the stellar.toml, or no wallet can verify
	// that a challenge came from Linq.
	ServerSigningSeed string
	// HomeDomain is the domain being authenticated for, e.g. linqswitch.xyz.
	HomeDomain string
	// WebAuthDomain is the domain serving this endpoint. It is pinned into the
	// challenge so a challenge issued for one deployment cannot be replayed
	// against another.
	WebAuthDomain string
	// NetworkPassphrase decides which network the challenge is valid on.
	NetworkPassphrase string
	// ChallengeTimeout is how long a challenge stays valid. Short by design:
	// the window is how long a stolen challenge remains useful.
	ChallengeTimeout time.Duration
	// JWTSecret signs the session token issued on success.
	JWTSecret []byte
	// JWTIssuer identifies this service in the token's iss claim.
	JWTIssuer string
	// JWTTimeout is the session lifetime.
	JWTTimeout time.Duration
	// Accounts loads client accounts to check signer thresholds. Optional: with
	// no loader, only the account's master key is accepted.
	Accounts AccountLoader
}

// Authenticator implements SEP-10 web authentication.
//
// The flow proves a client controls a Stellar account: the server hands out a
// specially-shaped transaction, the client signs it, and the server checks the
// signature. The transaction has sequence number 0 so it can never be submitted
// to the network — it is a signing challenge, not a payment.
type Authenticator struct {
	cfg    AuthConfig
	signer *keypair.Full
}

// NewAuthenticator validates the configuration and returns a ready
// authenticator. As with the Stellar client, every failure here is a
// deployment mistake and is caught at startup rather than mid-handshake.
func NewAuthenticator(cfg AuthConfig) (*Authenticator, error) {
	if cfg.ServerSigningSeed == "" {
		return nil, errors.New("sep10: server signing seed is not set")
	}
	signer, err := keypair.ParseFull(cfg.ServerSigningSeed)
	if err != nil {
		return nil, fmt.Errorf("sep10: invalid server signing seed: %w", err)
	}
	if cfg.HomeDomain == "" {
		return nil, errors.New("sep10: home domain is not set")
	}
	if cfg.WebAuthDomain == "" {
		return nil, errors.New("sep10: web auth domain is not set")
	}
	if cfg.NetworkPassphrase == "" {
		return nil, errors.New("sep10: network passphrase is not set")
	}
	if len(cfg.JWTSecret) < 32 {
		return nil, fmt.Errorf("sep10: jwt secret must be at least 32 bytes, got %d", len(cfg.JWTSecret))
	}
	if cfg.ChallengeTimeout <= 0 {
		cfg.ChallengeTimeout = 5 * time.Minute
	}
	if cfg.JWTTimeout <= 0 {
		cfg.JWTTimeout = 24 * time.Hour
	}
	return &Authenticator{cfg: cfg, signer: signer}, nil
}

// SigningKey returns the public key to publish as SIGNING_KEY in stellar.toml.
func (a *Authenticator) SigningKey() string { return a.signer.Address() }

// Challenge builds a challenge transaction for a client account.
//
// The result is base64 XDR for the client to sign and return. Nothing is stored
// server-side between the two calls: the challenge carries its own timebounds
// and server signature, so a replayed or expired one fails verification on its
// own terms rather than against remembered state.
func (a *Authenticator) Challenge(clientAccountID string, memoID *txnbuild.MemoID) (string, error) {
	if clientAccountID == "" {
		return "", errors.New("sep10: client account is required")
	}
	if _, err := keypair.ParseAddress(clientAccountID); err != nil {
		return "", fmt.Errorf("sep10: invalid client account: %w", err)
	}

	tx, err := txnbuild.BuildChallengeTx(
		a.cfg.ServerSigningSeed,
		clientAccountID,
		a.cfg.WebAuthDomain,
		a.cfg.HomeDomain,
		a.cfg.NetworkPassphrase,
		a.cfg.ChallengeTimeout,
		memoID,
	)
	if err != nil {
		return "", fmt.Errorf("sep10: build challenge: %w", err)
	}
	xdr, err := tx.Base64()
	if err != nil {
		return "", fmt.Errorf("sep10: encode challenge: %w", err)
	}
	return xdr, nil
}

// Verify checks a signed challenge and issues a session token.
//
// Which signatures are required depends on the client account. An account that
// does not yet exist on-chain has no signers to consult, so its master key is
// the only thing that can speak for it. An account that does exist may have
// been configured with multiple signers and a raised threshold — honouring that
// is the difference between authenticating an account and authenticating
// whoever holds one of its keys.
func (a *Authenticator) Verify(challengeXDR string) (token, clientAccountID string, err error) {
	_, clientAccountID, _, _, err = txnbuild.ReadChallengeTx(
		challengeXDR,
		a.signer.Address(),
		a.cfg.NetworkPassphrase,
		a.cfg.WebAuthDomain,
		[]string{a.cfg.HomeDomain},
	)
	if err != nil {
		return "", "", fmt.Errorf("sep10: invalid challenge: %w", err)
	}

	account, exists, err := a.loadAccount(clientAccountID)
	if err != nil {
		return "", "", err
	}

	if !exists {
		// No on-chain account: the master key is the only possible signer.
		if _, err := txnbuild.VerifyChallengeTxSigners(
			challengeXDR, a.signer.Address(), a.cfg.NetworkPassphrase,
			a.cfg.WebAuthDomain, []string{a.cfg.HomeDomain}, clientAccountID,
		); err != nil {
			return "", "", fmt.Errorf("sep10: signature verification failed: %w", err)
		}
	} else {
		summary := txnbuild.SignerSummary{}
		for _, s := range account.Signers {
			summary[s.Key] = s.Weight
		}
		if _, err := txnbuild.VerifyChallengeTxThreshold(
			challengeXDR, a.signer.Address(), a.cfg.NetworkPassphrase,
			a.cfg.WebAuthDomain, []string{a.cfg.HomeDomain},
			txnbuild.Threshold(account.Thresholds.MedThreshold), summary,
		); err != nil {
			return "", "", fmt.Errorf("sep10: signature verification failed: %w", err)
		}
	}

	token, err = a.issueToken(clientAccountID)
	if err != nil {
		return "", "", err
	}
	return token, clientAccountID, nil
}

// loadAccount reports whether a client account exists on-chain.
//
// A missing account is a normal state, not an error — most payers authenticate
// from accounts that have never been funded. A Horizon outage is different: it
// is not an answer, and must not be mistaken for "no account", which would
// silently downgrade a multisig account to master-key-only verification.
func (a *Authenticator) loadAccount(accountID string) (horizon.Account, bool, error) {
	if a.cfg.Accounts == nil {
		return horizon.Account{}, false, nil
	}
	account, err := a.cfg.Accounts.AccountDetail(horizonclient.AccountRequest{AccountID: accountID})
	if err != nil {
		if horizonclient.IsNotFoundError(err) {
			return horizon.Account{}, false, nil
		}
		return horizon.Account{}, false, fmt.Errorf("sep10: could not load account %s: %w", accountID, err)
	}
	return account, true, nil
}

func (a *Authenticator) issueToken(clientAccountID string) (string, error) {
	now := time.Now().UTC()
	claims := jwt.MapClaims{
		"iss": a.cfg.JWTIssuer,
		"sub": clientAccountID,
		"iat": now.Unix(),
		"exp": now.Add(a.cfg.JWTTimeout).Unix(),
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(a.cfg.JWTSecret)
	if err != nil {
		return "", fmt.Errorf("sep10: sign token: %w", err)
	}
	return signed, nil
}

// ParseToken validates a session token and returns the account it belongs to.
//
// The signing method is pinned to HS256. Without that check a token could
// arrive claiming alg "none", or claiming RS256 so the HMAC secret is treated
// as a public key — the two classic ways JWT verification is bypassed.
func (a *Authenticator) ParseToken(token string) (string, error) {
	parsed, err := jwt.Parse(token, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("sep10: unexpected signing method %v", t.Header["alg"])
		}
		return a.cfg.JWTSecret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil {
		return "", fmt.Errorf("sep10: invalid token: %w", err)
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return "", errors.New("sep10: token has no claims")
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return "", errors.New("sep10: token has no subject")
	}
	return sub, nil
}
