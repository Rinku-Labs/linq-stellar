// Package store holds the persistence layer for Stellar USDC settlement:
// the order record, its lifecycle states, and the compare-and-swap guards
// that keep two detectors from paying the same order twice.
package store

import "time"

// Order is one Stellar USDC -> NGN settlement.
//
// A payer sends USDC to a single-use Stellar account; once it lands, NGN is
// disbursed to the merchant's Nigerian bank account and the USDC is swept to
// treasury. Every field below exists to record one step of that, or to make a
// step provable afterwards.
//
// This is deliberately narrower than the order record in the main Linq backend,
// which carries savings, Telegram linkage and per-provider bookkeeping that has
// nothing to do with Stellar settlement. Anything not on this struct is not
// needed to settle a Stellar payment.
type Order struct {
	// --- Identity ---

	ID string `gorm:"primaryKey" json:"id"`
	// BusinessID is the merchant being settled to.
	BusinessID uint `gorm:"index" json:"businessId"`
	// IdempotencyKey is supplied by the integrator. Two creates with the same
	// key return the same order rather than minting a second deposit account.
	IdempotencyKey string `gorm:"uniqueIndex;size:255" json:"idempotencyKey"`
	// CustomerRef is the integrator's own reference. It is surfaced to the payer
	// in the SEP-7 memo, so it must stay short and free of anything private.
	CustomerRef string `json:"customerRef"`

	// --- Lifecycle ---

	Status string `gorm:"index" json:"status"`
	// FinancialOutcome is set exactly once, by compare-and-swap, to "disbursed"
	// or "refunded". It is the single guard that stops an order both paying out
	// and refunding: whichever path claims it first wins, the other stands down.
	// See ClaimFinancialOutcome.
	FinancialOutcome string `gorm:"index" json:"financialOutcome"`

	// --- Money ---

	// AmountUSDC is what actually arrived on-chain, not what was requested.
	// Deposits are manual — a payer sends what they like — so recording the
	// requested figure here would make the order permanently misreport itself.
	// Before a deposit lands it holds QuotedUSDC, so the payment URI has an
	// amount to prefill.
	AmountUSDC float64 `json:"amountUsdc"`
	AmountNGN  float64 `json:"amountNgn"`
	Rate       float64 `gorm:"type:double precision" json:"rate"`

	// QuotedUSDC and QuotedNGN are the deal struck at creation: what the payer
	// was asked to send, and what the merchant was promised for it. Unlike
	// AmountUSDC they are never rewritten, which is what makes it possible to
	// ask afterwards whether a deposit actually settled the invoice — the
	// question nobody could answer while the quote was being overwritten by the
	// deposit that was supposed to satisfy it.
	//
	// Zero means an open-amount order, where the payer picked the figure and
	// there is no invoice to honour. Orders created before these columns
	// existed read as zero and therefore keep their original behaviour.
	QuotedUSDC float64 `json:"quotedUsdc"`
	QuotedNGN  float64 `json:"quotedNgn"`
	// Underpaid marks an order whose deposit did not cover its quote. The
	// payout still goes out for what arrived — the merchant is owed the value
	// of a real payment — but the shortfall is recorded rather than absorbed
	// silently into a smaller number.
	Underpaid bool `gorm:"default:false" json:"underpaid"`
	// ShortfallNGN is how far the payout fell below QuotedNGN. Zero unless
	// Underpaid.
	ShortfallNGN float64 `gorm:"default:0" json:"shortfallNgn"`
	// FeeUSDC is always zero on Stellar. It is stored explicitly rather than
	// omitted so the zero-fee claim is auditable per order instead of being
	// inferred from a missing column.
	FeeUSDC float64 `gorm:"default:0" json:"feeUsdc"`

	// --- Stellar ---

	// DepositAddress is a single-use account, provisioned with sponsored
	// reserves and a USDC trustline, then merged back to the sponsor on sweep.
	// One account per order is what makes a payment attributable without
	// relying on the payer setting a memo correctly.
	DepositAddress string `gorm:"index" json:"depositAddress"`
	// EncryptedSeed is the deposit account's seed, AES-256-GCM at rest. The
	// account holds USDC only between deposit and sweep, and never holds XLM.
	EncryptedSeed string `json:"-"`
	// ProvisionTxHash is the sponsored-reserve transaction that created the
	// account and its trustline. Kept because it is the public proof that Linq,
	// not the payer, paid to open the account.
	ProvisionTxHash string `json:"provisionTxHash"`
	// DepositTxHash and DepositFrom identify the payment that funded the order.
	// DepositFrom is what lets a reviewer confirm payments came from distinct
	// wallets rather than one account paying itself.
	DepositTxHash string `json:"depositTxHash"`
	DepositFrom   string `json:"depositFrom"`
	// SweepTxHash moves USDC to treasury and merges the account back to the
	// sponsor, returning every sponsored lumen in the same transaction.
	SweepTxHash string `json:"sweepTxHash"`
	// RefundAddress must already hold a USDC trustline or a refund cannot land.
	// Verified at order creation, not at refund time, so a bad address is caught
	// while the payer is still on screen.
	RefundAddress string `json:"refundAddress"`
	RefundTxHash  string `json:"refundTxHash"`

	// --- Bank payout ---

	BankCode    string `json:"bankCode"`
	BankAccount string `json:"bankAccount"`
	AccountName string `json:"accountName"`
	BankName    string `json:"bankName"`
	Currency    string `gorm:"default:NGN" json:"currency"`
	// PayoutReference is the disbursing provider's identifier for the transfer,
	// recorded so an NGN credit can be traced back to this order.
	PayoutReference string `json:"payoutReference"`
	PayoutProvider  string `json:"payoutProvider"`

	// --- Operational ---

	// ManualDeposit marks orders where the payer sends an arbitrary amount
	// rather than an exact quoted one, so the payout reconciles to what landed.
	ManualDeposit  bool `json:"manualDeposit"`
	PayoutAttempts int  `gorm:"default:0" json:"payoutAttempts"`
	SweepAttempts  int  `gorm:"default:0" json:"sweepAttempts"`
	RefundAttempts int  `gorm:"default:0" json:"refundAttempts"`

	// --- Timestamps ---
	//
	// Real timestamps, not strings. The main backend stores these as TEXT in
	// mixed formats, which forced a separate column just to hold a parseable
	// deadline; there is no reason to inherit that here.

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// DepositDeadline is when the deposit watch gives up and expires the order.
	DepositDeadline time.Time  `gorm:"index" json:"depositDeadline"`
	DepositedAt     *time.Time `json:"depositedAt,omitempty"`
	PaidOutAt       *time.Time `json:"paidOutAt,omitempty"`
	SweptAt         *time.Time `json:"sweptAt,omitempty"`
	RefundedAt      *time.Time `json:"refundedAt,omitempty"`
}

func (Order) TableName() string { return "stellar_orders" }
