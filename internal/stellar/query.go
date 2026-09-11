package stellar

import (
	"fmt"
	"strconv"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/protocols/horizon/operations"
)

// USDCBalance returns an address's USDC balance.
//
// An account that does not exist yet, or exists without a USDC trustline,
// reports zero rather than an error: both are ordinary states for a deposit
// account that is waiting, and neither is worth failing an order over.
func (c *Client) USDCBalance(address string) (float64, error) {
	started := time.Now()
	account, err := c.horizon.AccountDetail(horizonclient.AccountRequest{AccountID: address})
	if err != nil {
		c.call("balance", address, started, filterNotFound(err))
		if horizonclient.IsNotFoundError(err) {
			return 0, nil
		}
		return 0, describeHorizonError(err)
	}

	for _, b := range account.Balances {
		if b.Asset.Code == c.usdc.Code && b.Asset.Issuer == c.usdc.Issuer {
			v, parseErr := strconv.ParseFloat(b.Balance, 64)
			if parseErr != nil {
				return 0, fmt.Errorf("stellar: parse usdc balance %q: %w", b.Balance, parseErr)
			}
			c.call("balance", address, started, nil, "usdc", v)
			return v, nil
		}
	}
	c.call("balance", address, started, nil, "usdc", 0)
	return 0, nil
}

// TrustsUSDC reports whether an address can actually receive USDC.
//
// Worth checking before an address is accepted as a refund destination: a
// payment to an account without the trustline fails with op_no_trust, and
// discovering that at refund time means the money is already stuck.
//
// Note the difference between the two failure modes here. A missing account is
// a definite "no". A Horizon outage is not an answer at all, and is returned as
// an error so a caller does not turn away a good address over a network hiccup.
func (c *Client) TrustsUSDC(address string) (bool, error) {
	started := time.Now()
	account, err := c.horizon.AccountDetail(horizonclient.AccountRequest{AccountID: address})
	if err != nil {
		c.call("trustline", address, started, filterNotFound(err))
		if horizonclient.IsNotFoundError(err) {
			return false, nil
		}
		return false, describeHorizonError(err)
	}

	for _, b := range account.Balances {
		if b.Asset.Code == c.usdc.Code && b.Asset.Issuer == c.usdc.Issuer {
			c.call("trustline", address, started, nil, "trusts", true)
			return true, nil
		}
	}
	c.call("trustline", address, started, nil, "trusts", false)
	return false, nil
}

// filterNotFound drops the one Horizon error that is an ordinary answer.
//
// A deposit account that does not exist yet is the normal state of an account
// nobody has paid, and logging it as a failure would make every waiting order
// look like a problem.
func filterNotFound(err error) error {
	if horizonclient.IsNotFoundError(err) {
		return nil
	}
	return describeHorizonError(err)
}

// DepositInfo identifies the payment that funded a deposit account.
type DepositInfo struct {
	// TxHash is the Stellar transaction hash, for an explorer link and for
	// proving on-chain that the order was really paid.
	TxHash string
	// From is the account the USDC came from. It is the only refund destination
	// available when the payer never supplied one, and it is what shows that
	// payments came from distinct wallets rather than one account paying itself.
	From string
}

// Deposit finds the USDC payment that funded a deposit account.
//
// Horizon answers this in a single call, unlike Sui, where the sender has to be
// chased through the coin object's previous transaction.
//
// Payments come back oldest-first, so the first successful incoming USDC
// payment is the deposit even after the account has been swept or topped up.
func (c *Client) Deposit(address string) (DepositInfo, error) {
	started := time.Now()
	page, err := c.horizon.Payments(horizonclient.OperationRequest{
		ForAccount: address,
		Limit:      50,
	})
	if err != nil {
		c.call("payments", address, started, describeHorizonError(err))
		return DepositInfo{}, fmt.Errorf("stellar: horizon payments for %s: %w", address, describeHorizonError(err))
	}
	c.call("payments", address, started, nil, "records", len(page.Embedded.Records))

	for _, record := range page.Embedded.Records {
		payment, ok := record.(operations.Payment)
		if !ok {
			// Path payments and account merges can also deliver funds, but the
			// checkout instructs a plain payment, and treating another shape as
			// the deposit risks naming the wrong account as the sender — which
			// would send a refund to a stranger.
			continue
		}
		if !payment.TransactionSuccessful {
			continue
		}
		// Incoming only: a sweep out of this account is also a payment record.
		if payment.To != address {
			continue
		}
		if payment.Code != c.usdc.Code || payment.Issuer != c.usdc.Issuer {
			continue
		}
		return DepositInfo{TxHash: payment.TransactionHash, From: payment.From}, nil
	}

	return DepositInfo{}, fmt.Errorf("stellar: no incoming usdc payment found for %s", address)
}
