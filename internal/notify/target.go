package notify

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

const maxWebhookURLLength = 2048

// An integrator's webhook URL is a stranger's input that this service then
// calls, from inside its own network. Left unchecked it is a way to make the
// worker request cloud metadata or anything else only it can reach.

// ErrBlockedAddress means a webhook host resolved to an address that is not on
// the public internet.
var ErrBlockedAddress = errors.New("webhook host is not a public address")

// blockedPrefixes are the non-public ranges netip has no predicate for.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64: an IPv4 address in disguise
	netip.MustParsePrefix("2001:db8::/32"),
}

func isPublic(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return false
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

// ValidateWebhookURL refuses what can be refused without a DNS lookup. The
// lookup's answer can change after this runs, so refuseNonPublic is what really
// protects the network; this is here to tell an admin at once.
func ValidateWebhookURL(raw string) error {
	if len(raw) > maxWebhookURLLength {
		return errors.New("url is too long")
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return errors.New("url is not valid")
	case u.Scheme != "https":
		return errors.New("url must use https")
	case u.Hostname() == "":
		return errors.New("url needs a host")
	case u.User != nil:
		return errors.New("url must not contain credentials")
	case u.Fragment != "":
		return errors.New("url must not contain a fragment")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return ErrBlockedAddress
	}
	if addr, err := netip.ParseAddr(host); err == nil && !isPublic(addr) {
		return ErrBlockedAddress
	}
	return nil
}

// refuseNonPublic runs after DNS, on the exact address about to be connected to.
// Checking there, rather than resolving separately first, is what stops a host
// that answers with a public address for the check and a private one for the
// connection.
func refuseNonPublic(_, address string, _ syscall.RawConn) error {
	addrPort, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("parse dial address: %w", err)
	}
	if !isPublic(addrPort.Addr()) {
		return ErrBlockedAddress
	}
	return nil
}

// NewIntegratorClient returns the client for integrators' webhooks. Redirects
// are not followed, because a public host could otherwise point this at an
// internal one; a 3xx counts as a failed delivery. Proxy settings are ignored
// for the same reason: a proxy would make the connection the check sees its own.
func NewIntegratorClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: refuseNonPublic}
	return &http.Client{
		Timeout: deliveryTimeout,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConnsPerHost:   2,
			ForceAttemptHTTP2:     true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
