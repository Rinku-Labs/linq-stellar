package stellar

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/stellar/go-stellar-sdk/keypair"
)

// GenerateAccount creates a fresh keypair for a single order and returns its
// public address (G...) alongside its encrypted secret seed.
//
// The address is not yet usable: a Stellar account does not exist on-chain
// until something creates it, and cannot hold USDC until it has a trustline.
// Call Provision before handing the address to a payer, or their payment will
// fail with op_no_destination.
func (c *Client) GenerateAccount() (address, encryptedSeed string, err error) {
	full, err := keypair.Random()
	if err != nil {
		return "", "", fmt.Errorf("stellar: generate keypair: %w", err)
	}
	encryptedSeed, err = c.EncryptSeed(full.Seed())
	if err != nil {
		return "", "", err
	}
	return full.Address(), encryptedSeed, nil
}

// EncryptSeed seals a secret seed with AES-GCM for storage.
//
// The nonce is random per seed and stored alongside the ciphertext, so two
// orders never encrypt to the same value even when something upstream goes
// wrong and hands us the same seed twice.
func (c *Client) EncryptSeed(seed string) (string, error) {
	gcm, err := c.aead()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("stellar: read nonce: %w", err)
	}
	return hex.EncodeToString(gcm.Seal(nonce, nonce, []byte(seed), nil)), nil
}

// DecryptSeed reverses EncryptSeed.
func (c *Client) DecryptSeed(ciphertext string) (string, error) {
	raw, err := hex.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("stellar: decode seed: %w", err)
	}
	gcm, err := c.aead()
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("stellar: ciphertext too short")
	}
	nonce, body := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, body, nil)
	if err != nil {
		return "", fmt.Errorf("stellar: decrypt seed: %w", err)
	}
	return string(plaintext), nil
}

// orderKeypair decrypts a stored seed back into a signing keypair.
//
// Every operation on a deposit account goes through here, because the service
// holds seeds only in encrypted form and needs the account's own signature to
// change its trustline or merge it away.
func (c *Client) orderKeypair(encryptedSeed string) (*keypair.Full, error) {
	seed, err := c.DecryptSeed(encryptedSeed)
	if err != nil {
		return nil, err
	}
	kp, err := keypair.ParseFull(seed)
	if err != nil {
		return nil, fmt.Errorf("stellar: decrypted seed is not a valid keypair: %w", err)
	}
	return kp, nil
}

func (c *Client) aead() (cipher.AEAD, error) {
	block, err := aes.NewCipher(c.encKey)
	if err != nil {
		return nil, fmt.Errorf("stellar: build cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("stellar: build gcm: %w", err)
	}
	return gcm, nil
}
