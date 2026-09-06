package store

// Order lifecycle.
//
// An order runs two legs. The fiat leg pays the merchant in NGN; the crypto leg
// sweeps the deposited USDC to treasury and returns the sponsored reserves. The
// fiat leg runs first — the merchant is paid before Linq moves its own funds —
// so StateDisbursed is not terminal.
//
// Values are stored as strings rather than an enum so a state can be read
// straight out of the database during an incident without a lookup table.
const (
	// StateInitiated is set at creation, before the deposit account exists.
	StateInitiated = "initiated"
	// StateAwaitingDeposit means the account is provisioned with a USDC
	// trustline and the payer has been given the address. Both the Horizon
	// stream and the polling sweep watch orders in this state.
	StateAwaitingDeposit = "awaiting_deposit"
	// StateDepositDetected is claimed by whichever detector sees the funds
	// first. Exactly one may hold it; see ClaimStatus.
	StateDepositDetected = "deposit_detected"

	// Fiat leg.
	StatePayoutQueued     = "payout_queued"
	StatePayoutProcessing = "payout_processing"
	// StateDisbursed means NGN reached the merchant. The crypto leg follows.
	StateDisbursed = "disbursed"

	// Crypto leg.
	StateSweepQueued     = "sweep_queued"
	StateSweepProcessing = "sweep_processing"
	// StateSettledInTreasury means USDC is in treasury and the deposit account
	// has been merged back to the sponsor, returning its reserves.
	StateSettledInTreasury = "settled_in_treasury"

	// Refund path, taken when the payout fails after a deposit landed.
	StateRefundQueued     = "refund_queued"
	StateRefundProcessing = "refund_processing"
	StateRefunded         = "refunded"

	// StateExpired means the deposit deadline passed with nothing received.
	// No money moved, so this is not a failure worth paging anyone about.
	StateExpired = "expired"
	// StateFailed means the order needs a human. Money may have moved.
	StateFailed = "failed"
)

// transitions is the set of moves an order may legally make.
//
// This is enforced rather than documented because the alternative — every
// worker deciding for itself what comes next — is how an order ends up both
// refunded and disbursed.
var transitions = map[string][]string{
	StateInitiated:       {StateAwaitingDeposit, StateFailed},
	StateAwaitingDeposit: {StateDepositDetected, StateExpired, StateFailed},
	StateDepositDetected: {StatePayoutQueued, StateFailed},

	StatePayoutQueued:     {StatePayoutProcessing, StateRefundQueued, StateFailed},
	StatePayoutProcessing: {StateDisbursed, StateRefundQueued, StatePayoutQueued, StateFailed},
	StateDisbursed:        {StateSweepQueued},

	StateSweepQueued:     {StateSweepProcessing, StateFailed},
	StateSweepProcessing: {StateSettledInTreasury, StateSweepQueued, StateFailed},

	StateRefundQueued:     {StateRefundProcessing, StateFailed},
	StateRefundProcessing: {StateRefunded, StateRefundQueued, StateFailed},
}

// CanTransition reports whether an order may move from one state to another.
func CanTransition(from, to string) bool {
	for _, allowed := range transitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// IsTerminal reports whether an order has finished moving.
func IsTerminal(state string) bool {
	switch state {
	case StateSettledInTreasury, StateRefunded, StateExpired, StateFailed:
		return true
	}
	return false
}

// HoldsFunds reports whether an order's deposit account may still hold USDC.
// Used by reconciliation to decide which accounts are worth checking on-chain.
func HoldsFunds(state string) bool {
	switch state {
	case StateDepositDetected, StatePayoutQueued, StatePayoutProcessing,
		StateDisbursed, StateSweepQueued, StateSweepProcessing,
		StateRefundQueued, StateRefundProcessing:
		return true
	}
	return false
}
