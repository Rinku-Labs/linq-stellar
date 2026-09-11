package stellar

import (
	"fmt"
	"strconv"

	"github.com/stellar/go-stellar-sdk/txnbuild"
)

// maxInspectedOperations bounds what this service will look at, and therefore
// what it will pay a fee for. A payment into a deposit account is one
// operation; anything sprawling is not the shape we subsidise.
const maxInspectedOperations = 5

// PaymentIntent is what a signed transaction is asking to do, once it has been
// checked to be a plain USDC payment and nothing else.
type PaymentIntent struct {
	// Destinations are the accounts being paid, in operation order.
	Destinations []string
	// Total is the USDC across all payment operations.
	Total float64
}

// InspectUSDCPayment parses a signed transaction and reports what it pays,
// rejecting anything that is not a straightforward USDC payment.
//
// This exists because the caller is a browser. Fee-bumping means this service
// pays the network fee for someone else's transaction, so the only thing
// standing between that and a drained sponsor account is what the transaction
// actually does. A shared key cannot provide it — anything shipped to a browser
// is public — so the transaction itself has to be the credential.
//
// Deliberately narrow. Anything unrecognised is refused rather than
// interpreted: this is the one place where being permissive costs real money.
func InspectUSDCPayment(signedXDR string, usdc txnbuild.CreditAsset) (*PaymentIntent, error) {
	generic, err := txnbuild.TransactionFromXDR(signedXDR)
	if err != nil {
		return nil, fmt.Errorf("stellar: could not parse transaction: %w", err)
	}

	// A fee bump is already someone else's fee arrangement. Wrapping one again
	// would let a caller nest arbitrary transactions behind a shape we approved.
	if _, isFeeBump := generic.FeeBump(); isFeeBump {
		return nil, fmt.Errorf("stellar: transaction is already fee-bumped")
	}

	tx, ok := generic.Transaction()
	if !ok {
		return nil, fmt.Errorf("stellar: not a simple transaction")
	}

	ops := tx.Operations()
	if len(ops) == 0 {
		return nil, fmt.Errorf("stellar: transaction has no operations")
	}
	if len(ops) > maxInspectedOperations {
		return nil, fmt.Errorf("stellar: transaction has %d operations, more than this service will sponsor", len(ops))
	}

	intent := &PaymentIntent{}
	for i, op := range ops {
		payment, ok := op.(*txnbuild.Payment)
		if !ok {
			// Change trust, account merge, path payments and everything else are
			// refused. We sponsor payments into our own accounts, nothing more.
			return nil, fmt.Errorf("stellar: operation %d is %T, only payments are sponsored", i, op)
		}

		// Native XLM has no code or issuer, so check that first rather than
		// comparing empty strings and accidentally matching.
		if payment.Asset.IsNative() ||
			payment.Asset.GetCode() != usdc.Code ||
			payment.Asset.GetIssuer() != usdc.Issuer {
			return nil, fmt.Errorf("stellar: operation %d does not pay the USDC this service settles", i)
		}

		amount, err := parseAmount(payment.Amount)
		if err != nil {
			return nil, fmt.Errorf("stellar: operation %d has an unreadable amount %q: %w", i, payment.Amount, err)
		}

		intent.Destinations = append(intent.Destinations, payment.Destination)
		intent.Total += amount
	}
	return intent, nil
}

// FeeBumpAndSubmit wraps an already-signed transaction so the sponsor pays its
// fee, and submits the result.
//
// The payer's own account is charged nothing: in a fee bump only the fee
// account is debited for the fee, which is what makes "zero fees deducted from
// the user's wallet" literally true rather than nearly true.
//
// Callers must have satisfied themselves about what the inner transaction does
// before calling this — see InspectUSDCPayment.
func (c *Client) FeeBumpAndSubmit(signedXDR string) (string, error) {
	generic, err := txnbuild.TransactionFromXDR(signedXDR)
	if err != nil {
		return "", fmt.Errorf("stellar: could not parse transaction: %w", err)
	}
	inner, ok := generic.Transaction()
	if !ok {
		return "", fmt.Errorf("stellar: not a simple transaction")
	}

	bump, err := txnbuild.NewFeeBumpTransaction(txnbuild.FeeBumpTransactionParams{
		Inner:      inner,
		FeeAccount: c.sponsor.Address(),
		BaseFee:    c.baseFee,
	})
	if err != nil {
		return "", fmt.Errorf("stellar: build fee bump: %w", err)
	}

	// Only the fee account signs the outer transaction; the inner one keeps the
	// payer's signature untouched, which is why this cannot alter what they
	// agreed to send.
	bump, err = bump.Sign(c.passphrase, c.sponsor)
	if err != nil {
		return "", fmt.Errorf("stellar: sign fee bump: %w", err)
	}

	resp, err := c.horizon.SubmitFeeBumpTransaction(bump)
	if err != nil {
		return "", fmt.Errorf("stellar: submit fee bump: %w", describeHorizonError(err))
	}

	c.log.Info("submitted fee-bumped payment",
		"tx", resp.Hash, "feePaidBy", c.sponsor.Address())
	return resp.Hash, nil
}

// parseAmount reads a Stellar decimal amount string.
func parseAmount(v string) (float64, error) {
	return strconv.ParseFloat(v, 64)
}
