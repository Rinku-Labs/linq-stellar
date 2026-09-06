package sep

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"
)

const testIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

func TestPayURICarriesAssetAndAmount(t *testing.T) {
	uri, err := BuildPayURI(PayParams{
		Destination: "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H",
		Amount:      50,
		AssetCode:   "USDC",
		AssetIssuer: testIssuer,
		Memo:        "ORDER_12345",
		Msg:         "Payment to Merchant XYZ",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.HasPrefix(uri, "web+stellar:pay?") {
		t.Fatalf("uri %q has the wrong scheme", uri)
	}

	q, err := url.ParseQuery(strings.TrimPrefix(uri, "web+stellar:pay?"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// The asset pair is the whole point: without it a wallet prefills a
	// native XLM payment to a USDC-only account.
	if q.Get("asset_code") != "USDC" || q.Get("asset_issuer") != testIssuer {
		t.Errorf("asset = %s/%s, want USDC/%s", q.Get("asset_code"), q.Get("asset_issuer"), testIssuer)
	}
	if q.Get("amount") != "50" {
		t.Errorf("amount = %q, want 50", q.Get("amount"))
	}
	if q.Get("memo_type") != string(MemoText) {
		t.Errorf("memo_type = %q, want MEMO_TEXT", q.Get("memo_type"))
	}
}

// A zero or negative amount must be absent, not rendered as "0" — a wallet
// would prefill that as a zero-value payment.
func TestPayURIOmitsUnusableAmount(t *testing.T) {
	for _, amount := range []float64{0, -1} {
		uri, err := BuildPayURI(PayParams{Destination: "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H", Amount: amount})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		if strings.Contains(uri, "amount=") {
			t.Errorf("amount %v produced %q, expected no amount parameter", amount, uri)
		}
	}
}

func TestPayURIRequiresDestination(t *testing.T) {
	if _, err := BuildPayURI(PayParams{Amount: 10}); err == nil {
		t.Error("expected an error with no destination")
	}
}

func TestMsgIsCappedAt300(t *testing.T) {
	uri, err := BuildPayURI(PayParams{
		Destination: "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H",
		Msg:         strings.Repeat("x", 400),
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	q, _ := url.ParseQuery(strings.TrimPrefix(uri, "web+stellar:pay?"))
	if got := len(q.Get("msg")); got != 300 {
		t.Errorf("msg length = %d, want 300", got)
	}
}

// The signature must verify against the published key, and must break if any
// part of the request is altered — otherwise it proves nothing.
func TestSignatureRoundTripsAndDetectsTampering(t *testing.T) {
	signer, err := keypair.Random()
	if err != nil {
		t.Fatalf("keypair: %v", err)
	}

	uri, err := BuildPayURI(PayParams{
		Destination:  "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H",
		Amount:       50,
		AssetCode:    "USDC",
		AssetIssuer:  testIssuer,
		OriginDomain: "linqswitch.xyz",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	signed, err := Sign(uri, signer)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := Verify(signed, signer.FromAddress()); err != nil {
		t.Fatalf("verify: %v", err)
	}

	// Swapping the destination is the attack this exists to stop.
	tampered := strings.Replace(signed,
		"GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H",
		"GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", 1)
	if err := Verify(tampered, signer.FromAddress()); err == nil {
		t.Error("a tampered destination still verified")
	}

	// A different key must not verify.
	other, _ := keypair.Random()
	if err := Verify(signed, other.FromAddress()); err == nil {
		t.Error("signature verified under the wrong key")
	}
}

func TestSignRejectsDoubleSigning(t *testing.T) {
	signer, _ := keypair.Random()
	uri, _ := BuildPayURI(PayParams{Destination: "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H"})
	signed, err := Sign(uri, signer)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := Sign(signed, signer); err == nil {
		t.Error("signing an already-signed uri should fail")
	}
}

func TestTOMLPublishesKeysAndAccounts(t *testing.T) {
	out := TOMLConfig{
		NetworkPassphrase:    "Public Global Stellar Network ; September 2015",
		WebAuthEndpoint:      "https://linqswitch.xyz/auth",
		SigningKey:           "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H",
		URIRequestSigningKey: testIssuer,
		Accounts:             []string{"GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H", ""},
		OrgName:              "Linq",
		Currencies:           []Currency{{Code: "USDC", Issuer: testIssuer, Status: "live", Decimals: 7}},
	}.RenderTOML()

	for _, want := range []string{
		`VERSION="2.0.0"`,
		`WEB_AUTH_ENDPOINT="https://linqswitch.xyz/auth"`,
		`URI_REQUEST_SIGNING_KEY=`,
		`[[CURRENCIES]]`,
		`code="USDC"`,
		`display_decimals=7`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stellar.toml missing %q\n---\n%s", want, out)
		}
	}

	// Empty values must be omitted, not published as "". A wallet reading an
	// empty SIGNING_KEY would treat it as the answer rather than as absent.
	if strings.Contains(out, `=""`) {
		t.Errorf("stellar.toml published an empty value\n---\n%s", out)
	}
	if strings.Contains(out, "TRANSFER_SERVER_SEP0024") {
		t.Error("advertised SEP-24 support that does not exist")
	}
}
