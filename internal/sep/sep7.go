// Package sep implements the Stellar Ecosystem Proposals Linq exposes:
// SEP-1 (stellar.toml), SEP-7 (payment URIs) and SEP-10 (authentication).
package sep

import (
	"encoding/base64"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"

	"github.com/stellar/go-stellar-sdk/keypair"
)

// sep7SignaturePrefix is the payload prefix defined by SEP-7: 35 zero bytes,
// then a byte of value 4, then the scheme's name. Signing a bare URI instead
// would let a signature produced for one purpose be replayed as another.
var sep7SignaturePrefix = append(
	append(make([]byte, 35), 4),
	[]byte("stellar.sep.7 - URI Scheme")...,
)

// MemoType is how a wallet should interpret the memo it attaches.
type MemoType string

const (
	MemoText   MemoType = "MEMO_TEXT"
	MemoID     MemoType = "MEMO_ID"
	MemoHash   MemoType = "MEMO_HASH"
	MemoReturn MemoType = "MEMO_RETURN"
)

// PayParams describes a payment request for a wallet to prefill.
type PayParams struct {
	// Destination is the account being paid. Required.
	Destination string
	// Amount is optional; omit it to let the payer choose.
	Amount float64
	// AssetCode and AssetIssuer identify the asset. Omit both for native XLM.
	AssetCode   string
	AssetIssuer string
	// Memo is attached to the transaction, typically an order reference.
	Memo     string
	MemoType MemoType
	// Msg is shown by the wallet to explain the payment. Capped at 300 by spec.
	Msg string
	// OriginDomain is the domain hosting the stellar.toml that carries the key
	// this request is verified against. Required for a signature to mean
	// anything, since it is what tells the wallet where to look.
	OriginDomain string
}

// BuildPayURI renders a `web+stellar:pay` URI.
//
// A wallet that scans this gets destination, asset and amount already filled
// in. That matters more than convenience: the alternative is a payer reading a
// bare address and typing the rest, which is where wrong-asset and wrong-amount
// mistakes come from. On Stellar the wrong-asset case is the dangerous one — an
// account expecting USDC will happily be sent XLM.
func BuildPayURI(p PayParams) (string, error) {
	if p.Destination == "" {
		return "", fmt.Errorf("sep7: destination is required")
	}

	q := url.Values{}
	q.Set("destination", p.Destination)

	// A zero, negative or non-finite amount is left out entirely rather than
	// emitted as "0", which a wallet would prefill as a zero-value payment.
	if p.Amount > 0 && !math.IsInf(p.Amount, 0) && !math.IsNaN(p.Amount) {
		q.Set("amount", formatAmount(p.Amount))
	}
	if p.AssetCode != "" {
		q.Set("asset_code", p.AssetCode)
	}
	if p.AssetIssuer != "" {
		q.Set("asset_issuer", p.AssetIssuer)
	}
	if p.Memo != "" {
		q.Set("memo", p.Memo)
		mt := p.MemoType
		if mt == "" {
			mt = MemoText
		}
		q.Set("memo_type", string(mt))
	}
	if p.Msg != "" {
		msg := p.Msg
		if len(msg) > 300 {
			msg = msg[:300]
		}
		q.Set("msg", msg)
	}
	if p.OriginDomain != "" {
		q.Set("origin_domain", p.OriginDomain)
	}

	return "web+stellar:pay?" + q.Encode(), nil
}

// Sign appends a SEP-7 signature to a URI.
//
// Without one, wallets that check show the request as unverified, because
// nothing ties it to a domain. With one, a wallet fetches the origin domain's
// stellar.toml, reads URI_REQUEST_SIGNING_KEY, and can confirm the payment
// request really came from Linq rather than from someone who intercepted a QR
// code and swapped the destination.
//
// The signature must be the last parameter, so a verifier can strip it and
// reconstruct exactly what was signed.
func Sign(uri string, signer *keypair.Full) (string, error) {
	if signer == nil {
		return "", fmt.Errorf("sep7: signing key is nil")
	}
	if strings.Contains(uri, "&signature=") || strings.Contains(uri, "?signature=") {
		return "", fmt.Errorf("sep7: uri is already signed")
	}

	sig, err := signer.Sign(append(append([]byte{}, sep7SignaturePrefix...), []byte(uri)...))
	if err != nil {
		return "", fmt.Errorf("sep7: sign uri: %w", err)
	}
	encoded := url.QueryEscape(base64.StdEncoding.EncodeToString(sig))
	return uri + "&signature=" + encoded, nil
}

// Verify reports whether a signed URI was signed by the given account.
//
// Provided so the signing path can be tested against a real verification
// rather than against itself.
func Verify(signedURI string, signer *keypair.FromAddress) error {
	idx := strings.LastIndex(signedURI, "&signature=")
	if idx == -1 {
		return fmt.Errorf("sep7: uri carries no signature")
	}
	unsigned := signedURI[:idx]

	raw, err := url.QueryUnescape(signedURI[idx+len("&signature="):])
	if err != nil {
		return fmt.Errorf("sep7: unescape signature: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return fmt.Errorf("sep7: decode signature: %w", err)
	}
	return signer.Verify(append(append([]byte{}, sep7SignaturePrefix...), []byte(unsigned)...), sig)
}

// formatAmount renders up to 7 decimal places with no trailing zeros. More
// precision than that is rejected by wallets as malformed.
func formatAmount(v float64) string {
	s := strconv.FormatFloat(v, 'f', 7, 64)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// URISigner signs SEP-7 payment requests for one domain.
//
// A thin wrapper over the keypair so callers hold a signer rather than a raw
// secret, and so a deployment without a signing key simply holds nil and emits
// unsigned URIs instead of failing.
type URISigner struct {
	key *keypair.Full
}

// NewURISigner parses a signing seed. Its public half must be published as
// URI_REQUEST_SIGNING_KEY in the stellar.toml, or wallets have nothing to
// verify against and will treat signed requests as unverified anyway.
func NewURISigner(seed string) (*URISigner, error) {
	if seed == "" {
		return nil, fmt.Errorf("sep7: signing seed is empty")
	}
	kp, err := keypair.ParseFull(seed)
	if err != nil {
		return nil, fmt.Errorf("sep7: invalid signing seed: %w", err)
	}
	return &URISigner{key: kp}, nil
}

// Address returns the public key to publish as URI_REQUEST_SIGNING_KEY.
func (s *URISigner) Address() string { return s.key.Address() }

// Sign appends a signature to a payment URI.
func (s *URISigner) Sign(uri string) (string, error) { return Sign(uri, s.key) }
