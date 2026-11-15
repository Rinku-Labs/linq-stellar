package payout

import (
	"errors"
	"fmt"

	"github.com/Rinku-Labs/linq-stellar/internal/store"
	"gorm.io/gorm"
)

// FakeProvider is the provider name a fake payout reports, and what the order
// records as its payout provider.
const FakeProvider = "fake"

// Fake is the payout provider of a test deployment (PAYOUT_PROVIDER=fake). It
// pays every payout it is sent, at once, and calls no one: no naira moves.
// config refuses it on mainnet.
//
// Failures are not its job. An induced failure (SD-4) is recorded by the payout
// worker and never reaches a provider, so a test order that should fail does,
// and its retry or refund runs as it would against the Linq backend.
//
// It keeps no records of its own. The payout worker writes each attempt to
// stellar_payout_attempts before sending it, so a reference Status is asked
// about is found there, and only an attempt that was sent counts. That keeps
// its answers right across restarts, which the reconciliation's payout check
// depends on: an in-memory fake would report every order paid before the last
// deploy as having no payout.
type Fake struct {
	DB *gorm.DB
}

// Pay reports the payout paid. Like the real provider it is idempotent on the
// reference: the same reference always gets the same payout ID.
func (f Fake) Pay(req Request) (Result, error) {
	amount := req.AmountNGN
	return Result{Status: StatusPaid, Reference: fakePayoutID(req.Reference), Provider: FakeProvider, AmountNGN: &amount}, nil
}

// Status reports a sent payout as paid, for the order's amount. A reference
// never sent, or only ever induced, is ErrNotFound, as the Linq backend would
// say of a reference it was never given.
func (f Fake) Status(reference string) (Result, error) {
	var attempt store.PayoutAttempt
	err := f.DB.Where("reference = ? AND induced = ?", reference, false).First(&attempt).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Result{}, fmt.Errorf("%w: %s", ErrNotFound, reference)
	}
	if err != nil {
		return Result{}, fmt.Errorf("payout: fake status of %s: %w", reference, err)
	}
	var order store.Order
	if err := f.DB.Select("id", "amount_ngn").First(&order, "id = ?", attempt.OrderID).Error; err != nil {
		return Result{}, fmt.Errorf("payout: fake status of %s: order %s: %w", reference, attempt.OrderID, err)
	}
	amount := order.AmountNGN
	return Result{Status: StatusPaid, Reference: fakePayoutID(reference), Provider: FakeProvider, AmountNGN: &amount}, nil
}

func fakePayoutID(reference string) string { return "fake_" + reference }

// FixedRate is the naira-per-USDC rate a fake-provider deployment quotes at
// (TEST_RATE_NGN). config refuses it beside the real provider.
type FixedRate float64

// Rate returns the fixed rate.
func (r FixedRate) Rate() (float64, error) {
	if r <= 0 {
		return 0, errors.New("payout: no test rate set")
	}
	return float64(r), nil
}
