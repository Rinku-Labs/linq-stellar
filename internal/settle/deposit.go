// Package settle turns an observed on-chain deposit into a settled order.
//
// Deposit detection is deliberately redundant. A Horizon stream sees a payment
// in about a second; a polling sweep catches whatever the stream dropped across
// a reconnect. Redundancy is the point — a stream that silently misses an event
// means a payer who paid and got nothing — but it means two goroutines can
// reach the same order at the same instant.
//
// Everything therefore funnels through Deposits.Record, which claims the order
// with a compare-and-swap before anything downstream happens. A detector that
// queues a payout without claiming first will eventually pay an order twice.
// One door to the money, no exceptions.
package settle

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/Rinku-Labs/linq-stellar/internal/money"
	"github.com/Rinku-Labs/linq-stellar/internal/stellar"
	"github.com/Rinku-Labs/linq-stellar/internal/store"
	"gorm.io/gorm"
)

// Payouts queues an order for NGN disbursement once its deposit is confirmed.
type Payouts interface {
	Enqueue(orderID string) error
}

// Deposits records confirmed deposits and hands them to payout.
type Deposits struct {
	DB      *gorm.DB
	Chain   *stellar.Client
	Payouts Payouts
	Log     *slog.Logger
}

// Record claims an order for a confirmed deposit and queues its payout.
//
// Returns true only for the caller that won the claim. A false return is the
// ordinary outcome when the other detector arrived first: nothing has gone
// wrong and the caller should simply stop.
//
// The amount recorded is what actually arrived, never what was requested.
// Deposits are manual — a payer sends what they like — so storing the requested
// figure would make the order permanently misreport itself. What the order was
// quoted at survives separately, in QuotedUSDC and QuotedNGN, because the
// payout has to be measured against it.
func (d *Deposits) Record(order *store.Order, onChain float64, info stellar.DepositInfo) (bool, error) {
	// Orders created before the quote columns existed still carry their quote:
	// AmountUSDC and AmountNGN hold it right up until the deposit rewrites
	// them, which is what the next few lines are about to do. Reading it here
	// means an order already in flight during the deploy is settled by the same
	// rule as a new one, instead of having its quoted NGN overwritten by a
	// figure recomputed from the deposit.
	quotedUSDC, quotedNGN := order.QuotedUSDC, order.QuotedNGN
	if quotedNGN == 0 {
		quotedUSDC, quotedNGN = order.AmountUSDC, order.AmountNGN
	}

	settlement, err := Reconcile(Deposit{
		Received:   onChain,
		Fee:        order.FeeUSDC,
		Rate:       order.Rate,
		QuotedUSDC: quotedUSDC,
		QuotedNGN:  quotedNGN,
		Manual:     order.ManualDeposit,
	})
	if err != nil {
		// Not an error worth logging loudly: "nothing has arrived yet" is the
		// normal state of an order being watched.
		return false, nil
	}

	now := time.Now().UTC()
	fields := map[string]any{
		"amount_usdc":   settlement.PayableUSDC,
		"amount_ngn":    settlement.PayoutNGN,
		"underpaid":     settlement.Underpaid,
		"shortfall_ngn": settlement.ShortfallNGN,
		"deposited_at":  &now,
	}
	if info.TxHash != "" {
		fields["deposit_tx_hash"] = info.TxHash
	}
	if info.From != "" {
		fields["deposit_from"] = info.From
	}

	won, err := store.ClaimStatusWith(d.DB, order.ID,
		store.StateAwaitingDeposit, store.StateDepositDetected, fields)
	if err != nil {
		return false, fmt.Errorf("settle: claim deposit for %s: %w", order.ID, err)
	}
	if !won {
		return false, nil
	}

	d.Log.Info("deposit confirmed",
		"order", order.ID, "usdc", settlement.PayableUSDC,
		"payoutNgn", settlement.PayoutNGN, "quotedNgn", quotedNGN,
		"from", info.From, "tx", info.TxHash)

	// An underpayment is not a failure — the payout still goes out for what
	// arrived — but it is the one case where the merchant is paid less than
	// they invoiced, and that should never be something only arithmetic knows.
	if settlement.Underpaid {
		d.Log.Warn("deposit did not cover the quote; paying for what arrived",
			"order", order.ID,
			"quotedUsdc", quotedUSDC, "receivedUsdc", settlement.PayableUSDC,
			"quotedNgn", quotedNGN, "payoutNgn", settlement.PayoutNGN,
			"shortfallNgn", settlement.ShortfallNGN)
	}

	// From here the order is claimed but no payout job exists yet. If queueing
	// fails the claim is handed back rather than left stranded in a state
	// nothing is working on — the detectors will find it again next sweep.
	if err := d.Payouts.Enqueue(order.ID); err != nil {
		d.Log.Error("payout enqueue failed, releasing claim",
			"order", order.ID, "error", err)
		if relErr := store.ReleaseStatus(d.DB, order.ID,
			store.StateDepositDetected, store.StateAwaitingDeposit); relErr != nil {
			// Now it is stuck: claimed, unqueued, and not released. Say so
			// loudly, because it needs a human before the payer does.
			d.Log.Error("could not release claim; order is stranded",
				"order", order.ID, "error", relErr)
		}
		return false, fmt.Errorf("settle: enqueue payout for %s: %w", order.ID, err)
	}

	if _, err := store.ClaimStatus(d.DB, order.ID,
		store.StateDepositDetected, store.StatePayoutQueued); err != nil {
		// The job is already queued, so the payout will still happen; only the
		// recorded state is behind. Worth knowing about, not worth failing.
		d.Log.Error("could not advance to payout_queued", "order", order.ID, "error", err)
	}
	return true, nil
}

// Expire gives up on an order whose deposit never arrived.
//
// No money moved, so this is an ordinary ending rather than a failure. The
// deposit account still holds sponsored reserves, which the reclaim worker
// returns to the sponsor.
func (d *Deposits) Expire(order *store.Order) error {
	won, err := store.ClaimStatus(d.DB, order.ID,
		store.StateAwaitingDeposit, store.StateExpired)
	if err != nil {
		return fmt.Errorf("settle: expire %s: %w", order.ID, err)
	}
	if won {
		d.Log.Info("order expired with no deposit",
			"order", order.ID, "address", order.DepositAddress)
	}
	return nil
}

// Deposit is everything Reconcile needs to decide what a payment is worth.
type Deposit struct {
	// Received is the USDC that landed on-chain.
	Received float64
	// Fee comes off the top. Always zero on Stellar today, carried explicitly
	// so the zero-fee claim is auditable per order.
	Fee float64
	// Rate is the NGN per USDC the order was quoted at.
	Rate float64
	// QuotedUSDC and QuotedNGN are what the order was created for. Both zero
	// for an open-amount order, where the payer chose the figure and there is
	// no invoice to honour.
	QuotedUSDC float64
	QuotedNGN  float64
	// Manual marks an order where the payer sends an amount of their choosing,
	// so an overpayment pays out proportionally rather than capping at the
	// quote. It has no bearing on underpayment.
	Manual bool
}

// Settlement is what a deposit is worth, and whether it settled the invoice.
type Settlement struct {
	// PayableUSDC is the deposit net of fees.
	PayableUSDC float64
	// PayoutNGN is what the merchant is paid, in whole kobo.
	PayoutNGN float64
	// Underpaid is true when the deposit did not cover the quote.
	Underpaid bool
	// ShortfallNGN is how far PayoutNGN fell below the quote. Zero unless
	// Underpaid.
	ShortfallNGN float64
}

// Reconcile is the money maths for a deposit: given what landed, what is
// payable and what is it worth in NGN?
//
// Split out so it can be exercised without Horizon or a database. It is the
// only part of deposit handling that decides an amount of money, and it is
// worth being able to prove.
//
// The rule is one line: the merchant never receives less than the invoice when
// the payer covered it, and never less than what actually arrived is worth.
// Concretely, for an order carrying a quote —
//
//   - deposit covers the quote: pay the quoted NGN. Recomputing it from the
//     deposit instead is what turned a ₦100 invoice into a ₦95.49 payout,
//     because the quote the payer worked from had already been rounded down.
//   - manual order, deposit over the quote: pay for what arrived. A payer who
//     sends more on an open-amount order is buying more naira, not making a
//     donation, so the proportional figure wins where it is larger.
//   - deposit short of the quote: pay for what arrived and say so. The payment
//     is real and the merchant is owed its value; what must not happen is the
//     shortfall disappearing into a smaller number nobody flagged.
//
// An error means there is nothing to pay out yet, which is the normal
// keep-waiting signal rather than a failure.
func Reconcile(d Deposit) (Settlement, error) {
	if d.Received <= 0 {
		return Settlement{}, fmt.Errorf("settle: no balance")
	}
	payable := d.Received - d.Fee
	if payable <= 0 {
		return Settlement{}, fmt.Errorf("settle: deposit does not cover the fee")
	}
	payable = money.RoundUSDC(payable)

	s := Settlement{
		PayableUSDC: payable,
		PayoutNGN:   money.QuoteNGN(payable, d.Rate),
	}

	// No quote to honour: an open-amount order is worth exactly what arrived.
	if d.QuotedNGN <= 0 {
		return s, nil
	}

	quoted := money.RoundNGN(d.QuotedNGN)

	if !money.Covers(payable, d.QuotedUSDC) {
		s.Underpaid = true
		if s.ShortfallNGN = money.RoundNGN(quoted - s.PayoutNGN); s.ShortfallNGN < 0 {
			// A deposit below the quoted USDC but worth more in naira means the
			// two halves of the quote disagree. Nothing is owed, so report no
			// shortfall rather than a negative one.
			s.ShortfallNGN = 0
		}
		return s, nil
	}

	// The invoice is covered. A manual order may pay more for an overpayment;
	// neither may pay less than what was promised.
	if !d.Manual || s.PayoutNGN < quoted {
		s.PayoutNGN = quoted
	}
	return s, nil
}
