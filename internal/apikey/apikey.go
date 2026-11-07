// Package apikey is the format of the keys integrators authenticate with:
// lq_live_<8-char prefix>_<secret>, or lq_test_ on a testnet deployment.
//
// The prefix is what a key is looked up by and the only part of one that may
// ever be stored in the clear or logged. The secret is 32 bytes of entropy, so
// a plain SHA-256 of the whole key is enough; a slow hash only earns its cost
// against secrets a person chose.
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/stellar/go-stellar-sdk/network"
)

const (
	alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

	prefixLen = 8
	// secretLen is the fewest base62 characters that hold 32 random bytes:
	// 62^43 is just over 2^256.
	secretLen = 43
	// maxLen bounds what Prefix will look at, so an oversized header is refused
	// on its length alone. It leaves room for a longer secret later.
	maxLen = 128

	modeLive = "live"
	modeTest = "test"
)

// ErrMalformed means the value is not shaped like a key at all.
var ErrMalformed = errors.New("apikey: malformed key")

// Mode is the key flavour for a deployment. Keys carry it so that one pasted
// into the wrong environment is visibly wrong. It is also enforced for free:
// the mode is inside the hashed value, so a test key never matches a live row.
func Mode(networkPassphrase string) string {
	if networkPassphrase == network.TestNetworkPassphrase {
		return modeTest
	}
	return modeLive
}

// New returns a fresh key and the prefix it is looked up by. The full key
// exists only in this return value and the response that carries it.
func New(mode string) (full, prefix string, err error) {
	prefix, err = random(prefixLen)
	if err != nil {
		return "", "", err
	}
	secret, err := random(secretLen)
	if err != nil {
		return "", "", err
	}
	return fmt.Sprintf("lq_%s_%s_%s", mode, prefix, secret), prefix, nil
}

// Prefix extracts the lookup prefix from a presented key.
func Prefix(full string) (string, error) {
	if len(full) > maxLen {
		return "", ErrMalformed
	}
	parts := strings.Split(full, "_")
	if len(parts) != 4 || parts[0] != "lq" ||
		(parts[1] != modeLive && parts[1] != modeTest) ||
		len(parts[2]) != prefixLen || len(parts[3]) < secretLen {
		return "", ErrMalformed
	}
	return parts[2], nil
}

// Hash is the stored form of a key.
func Hash(full string) string {
	sum := sha256.Sum256([]byte(full))
	return hex.EncodeToString(sum[:])
}

// Matches reports whether a presented key hashes to the stored value, in
// constant time.
func Matches(full, storedHash string) bool {
	return subtle.ConstantTimeCompare([]byte(Hash(full)), []byte(storedHash)) == 1
}

// NewID returns a public identifier such as int_… or key_…. It is not a secret.
func NewID(kind string) (string, error) {
	suffix, err := random(16)
	if err != nil {
		return "", err
	}
	return kind + "_" + suffix, nil
}

// random draws n base62 characters. Bytes of 248 and above are thrown away
// rather than wrapped, because 256 is not a multiple of 62 and wrapping would
// make the first eight characters of the alphabet likelier than the rest.
func random(n int) (string, error) {
	out := make([]byte, 0, n)
	buf := make([]byte, n+n/4)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("apikey: read random: %w", err)
		}
		for _, b := range buf {
			if b < 248 && len(out) < n {
				out = append(out, alphabet[int(b)%len(alphabet)])
			}
		}
	}
	return string(out), nil
}
