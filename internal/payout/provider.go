// Package payout disburses NGN for a settled Stellar order.
//
// Linq already runs a multi-provider payout stack for every other supported
// chain, and this service does not reimplement it. It holds no bank
// credentials, no partner API keys and no provider selection logic; it hands a
// confirmed deposit to the Linq B2B API and records what comes back. That
// keeps the bank relationship in one place and keeps this repository
// publishable.
package payout

import (
	"errors"
	"fmt"
)

// Request is one NGN disbursement.
type Request struct {
	// Reference is the Stellar order ID. It doubles as the idempotency key, so
	// a retry after a timeout resumes the same payout rather than sending a
	// second one — the failure mode that matters most here, because a duplicate
	// payout is real money and cannot be recalled.
	Reference string
	AmountNGN float64
	Currency  string

	BankCode    string
	BankAccount string
	AccountName string
	BankName    string

	// SourceTxHash is the Stellar deposit that funded this payout, carried
	// through so a naira credit can be traced back to an on-chain payment
	// during reconciliation.
	SourceTxHash string
}

// Validate rejects requests that cannot succeed, before any money moves.
//
// A payout that fails at the bank has already consumed a retry and delayed a
// merchant; the cheap checks belong here.
func (r Request) Validate() error {
	switch {
	case r.Reference == "":
		return errors.New("payout: reference is required")
	case r.AmountNGN <= 0:
		return fmt.Errorf("payout: amount must be positive, got %v", r.AmountNGN)
	case r.BankCode == "":
		return errors.New("payout: bank code is required")
	case r.BankAccount == "":
		return errors.New("payout: bank account is required")
	}
	return nil
}

// Status is where a disbursement has got to.
type Status string

const (
	// StatusQueued means accepted and in progress.
	StatusQueued Status = "queued"
	// StatusPaid means the naira reached the merchant.
	StatusPaid Status = "paid"
	// StatusFailed means it will not succeed and the deposit should be refunded.
	StatusFailed Status = "failed"
)

// Result is what the payout provider reported.
type Result struct {
	Status Status
	// Reference is the provider's own identifier for the transfer.
	Reference string
	// Provider names which payout rail handled it, for reconciliation.
	Provider string
	// FailureReason is set when Status is StatusFailed.
	FailureReason string
}

// Provider disburses NGN.
type Provider interface {
	// Pay submits a disbursement. It must be idempotent on Request.Reference:
	// a retry after a network timeout has to return the existing payout rather
	// than start a second one.
	Pay(Request) (Result, error)
	// Status reports on a previously submitted payout, so an order whose
	// response was lost can be resolved without guessing.
	Status(reference string) (Result, error)
}
