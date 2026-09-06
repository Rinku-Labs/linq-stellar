package sep

import (
	"fmt"
	"strings"
)

// TOMLConfig is the content of the SEP-1 stellar.toml served at
// /.well-known/stellar.toml on the origin domain.
//
// This file is how anything on Stellar discovers what Linq is and which keys
// speak for it. Two fields carry real weight: SIGNING_KEY, which a wallet uses
// to verify SEP-10 authentication challenges, and URIRequestSigningKey, which
// it uses to verify SEP-7 payment requests. Publish the wrong key in either and
// the corresponding flow silently stops being trustworthy.
type TOMLConfig struct {
	NetworkPassphrase string
	// WebAuthEndpoint is the SEP-10 endpoint. Wallets will not attempt
	// authentication without it, however complete the handler is.
	WebAuthEndpoint string
	// SigningKey (G...) is the public half of the SEP-10 server key.
	SigningKey string
	// URIRequestSigningKey (G...) is the public half of the SEP-7 key.
	URIRequestSigningKey string
	// Accounts are the public accounts Linq operates. Listing the sponsor and
	// treasury here is deliberate: it lets anyone audit provisioning and
	// settlement volume on Horizon without asking Linq for anything.
	Accounts []string
	// TransferServerSEP24 is advertised only once a hosted deposit/withdrawal
	// service actually exists behind it.
	TransferServerSEP24 string

	OrgName        string
	OrgURL         string
	OrgLogo        string
	OrgDescription string
	OrgEmail       string

	// Currencies are the assets Linq accepts.
	Currencies []Currency
}

// Currency is one asset entry in the stellar.toml.
type Currency struct {
	Code     string
	Issuer   string
	Status   string
	Name     string
	Desc     string
	Decimals int
}

// RenderTOML produces the stellar.toml body.
//
// Written by hand rather than through a TOML library because the output is a
// short, fixed shape and a published file that anything on the network may read
// is worth being able to eyeball in the source.
func (c TOMLConfig) RenderTOML() string {
	var b strings.Builder

	b.WriteString("# Linq — Stellar Ecosystem Proposal metadata\n")
	b.WriteString("# https://stellar.org/protocol/sep-1\n\n")

	b.WriteString(`VERSION="2.0.0"` + "\n")
	writeKV(&b, "NETWORK_PASSPHRASE", c.NetworkPassphrase)
	writeKV(&b, "WEB_AUTH_ENDPOINT", c.WebAuthEndpoint)
	writeKV(&b, "SIGNING_KEY", c.SigningKey)
	writeKV(&b, "URI_REQUEST_SIGNING_KEY", c.URIRequestSigningKey)
	writeKV(&b, "TRANSFER_SERVER_SEP0024", c.TransferServerSEP24)

	if len(c.Accounts) > 0 {
		quoted := make([]string, 0, len(c.Accounts))
		for _, a := range c.Accounts {
			if a != "" {
				quoted = append(quoted, fmt.Sprintf("%q", a))
			}
		}
		if len(quoted) > 0 {
			fmt.Fprintf(&b, "ACCOUNTS=[%s]\n", strings.Join(quoted, ", "))
		}
	}

	b.WriteString("\n[DOCUMENTATION]\n")
	writeKV(&b, "ORG_NAME", c.OrgName)
	writeKV(&b, "ORG_URL", c.OrgURL)
	writeKV(&b, "ORG_LOGO", c.OrgLogo)
	writeKV(&b, "ORG_DESCRIPTION", c.OrgDescription)
	writeKV(&b, "ORG_OFFICIAL_EMAIL", c.OrgEmail)

	for _, cur := range c.Currencies {
		b.WriteString("\n[[CURRENCIES]]\n")
		writeKV(&b, "code", cur.Code)
		writeKV(&b, "issuer", cur.Issuer)
		writeKV(&b, "status", cur.Status)
		if cur.Decimals > 0 {
			fmt.Fprintf(&b, "display_decimals=%d\n", cur.Decimals)
		}
		writeKV(&b, "name", cur.Name)
		writeKV(&b, "desc", cur.Desc)
	}

	return b.String()
}

// writeKV emits a key only when it has a value. An empty SIGNING_KEY or
// WEB_AUTH_ENDPOINT is worse than an absent one: a wallet reading it would
// treat the empty string as the answer rather than as "not offered".
func writeKV(b *strings.Builder, key, value string) {
	if value == "" {
		return
	}
	// %q emits a quoted, escaped string, which is exactly TOML's basic-string
	// form. Escaping before this would double-escape every quote.
	fmt.Fprintf(b, "%s=%q\n", key, value)
}
