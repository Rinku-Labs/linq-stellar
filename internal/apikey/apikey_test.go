package apikey

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/network"
)

func TestKeyFormat(t *testing.T) {
	for passphrase, want := range map[string]string{
		network.PublicNetworkPassphrase: `^lq_live_[0-9A-Za-z]{8}_[0-9A-Za-z]{43}$`,
		network.TestNetworkPassphrase:   `^lq_test_[0-9A-Za-z]{8}_[0-9A-Za-z]{43}$`,
	} {
		full, prefix, err := New(Mode(passphrase))
		if err != nil {
			t.Fatalf("new: %v", err)
		}
		if !regexp.MustCompile(want).MatchString(full) {
			t.Errorf("key %q does not match %s", full, want)
		}
		got, err := Prefix(full)
		if err != nil || got != prefix {
			t.Errorf("Prefix = %q, %v; want %q", got, err, prefix)
		}
	}
}

func TestMalformedKeysAreRefused(t *testing.T) {
	secret := strings.Repeat("a", secretLen)
	for name, key := range map[string]string{
		"empty":        "",
		"legacy hex":   strings.Repeat("ab", 32),
		"wrong brand":  "xx_live_abcdefgh_" + secret,
		"wrong mode":   "lq_prod_abcdefgh_" + secret,
		"short prefix": "lq_live_abc_" + secret,
		"short secret": "lq_live_abcdefgh_abc",
		"extra part":   "lq_live_abcdefgh_" + secret + "_x",
		"oversized":    "lq_live_abcdefgh_" + strings.Repeat("a", maxLen),
	} {
		if _, err := Prefix(key); err == nil {
			t.Errorf("%s: accepted %q", name, key)
		}
	}
}

func TestMatches(t *testing.T) {
	full, _, err := New(modeLive)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	stored := Hash(full)
	if !Matches(full, stored) {
		t.Error("the right key did not match")
	}
	if Matches(full+"x", stored) {
		t.Error("the wrong key matched")
	}
	// The mode is inside the hash, so a test key cannot pass for a live one.
	if Matches(strings.Replace(full, "lq_live_", "lq_test_", 1), stored) {
		t.Error("a test key matched a live key's hash")
	}
}
