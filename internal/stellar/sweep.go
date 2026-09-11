package stellar

import (
	"fmt"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/txnbuild"
)

// Sweep moves a deposit account's USDC to the destination and then closes the
// account, returning its sponsored reserves to the sponsor.
//
// The trustline is removed before the merge because Stellar refuses to merge an
// account that still holds subentries. The sponsor is the transaction source
// and pays the fee, so the deposit account never needs to hold XLM at any point
// in its life.
//
// The expected amount is advisory. What is actually swept is the on-chain
// balance, because dust or an overpayment left behind would keep a subentry
// alive and block the merge — stranding the sponsor's reserves over a rounding
// difference.
func (c *Client) Sweep(encryptedSeed, destination string, expected float64) (string, error) {
	if destination == "" {
		return "", fmt.Errorf("stellar: sweep destination is empty")
	}
	orderKp, err := c.orderKeypair(encryptedSeed)
	if err != nil {
		return "", err
	}

	onChain, err := c.USDCBalance(orderKp.Address())
	if err != nil {
		return "", fmt.Errorf("stellar: read balance before sweep: %w", err)
	}
	if onChain < expected {
		c.log.Info("on-chain balance below expected; sweeping actual balance",
			"account", orderKp.Address(), "onChain", onChain, "expected", expected)
	}

	sponsorAccount, err := c.horizon.AccountDetail(horizonclient.AccountRequest{AccountID: c.sponsor.Address()})
	if err != nil {
		return "", fmt.Errorf("stellar: load sponsor account: %w", describeHorizonError(err))
	}

	// A zero balance still needs the trustline removed and the account merged,
	// so the payment is conditional but the teardown is not.
	ops := make([]txnbuild.Operation, 0, 3)
	if onChain > 0 {
		ops = append(ops, &txnbuild.Payment{
			Destination:   destination,
			Amount:        FormatAmount(onChain),
			Asset:         c.usdc,
			SourceAccount: orderKp.Address(),
		})
	}
	ops = append(ops,
		&txnbuild.ChangeTrust{
			Line:          c.usdc.MustToChangeTrustAsset(),
			Limit:         "0",
			SourceAccount: orderKp.Address(),
		},
		&txnbuild.AccountMerge{
			Destination:   c.sponsor.Address(),
			SourceAccount: orderKp.Address(),
		},
	)

	hash, err := c.submit(&sponsorAccount, ops, orderKp, "sweep")
	if err != nil {
		return "", err
	}
	c.log.Info("swept usdc to treasury and closed deposit account",
		"account", orderKp.Address(), "destination", destination, "amount", onChain, "tx", hash)
	return hash, nil
}

// Reclaim closes a deposit account that never received a payment, returning its
// sponsored reserves to the sponsor.
//
// Without this, every abandoned order locks roughly 1.5 XLM of sponsor funds
// permanently: the only other merge happens in Sweep, and that path is reached
// only once a deposit has landed. At volume, expired orders would quietly drain
// the sponsor.
//
// It is safe against a late deposit by construction rather than by timing.
// Stellar rejects a zero-limit ChangeTrust while the trustline still holds a
// balance, and rejects AccountMerge while the account has subentries. Both are
// in one transaction, so USDC arriving between the watcher giving up and this
// running makes the whole transaction fail and leaves the funds untouched.
//
// An empty hash with a nil error means there was nothing left to close.
func (c *Client) Reclaim(encryptedSeed string) (string, error) {
	orderKp, err := c.orderKeypair(encryptedSeed)
	if err != nil {
		return "", err
	}

	// An order that failed before provisioning, or one an earlier reclaim
	// already merged, has no account to close. That is a no-op, not a failure.
	if _, err := c.horizon.AccountDetail(horizonclient.AccountRequest{AccountID: orderKp.Address()}); err != nil {
		if horizonclient.IsNotFoundError(err) {
			return "", nil
		}
		return "", describeHorizonError(err)
	}

	sponsorAccount, err := c.horizon.AccountDetail(horizonclient.AccountRequest{AccountID: c.sponsor.Address()})
	if err != nil {
		return "", fmt.Errorf("stellar: load sponsor account: %w", describeHorizonError(err))
	}

	ops := []txnbuild.Operation{
		&txnbuild.ChangeTrust{
			Line:          c.usdc.MustToChangeTrustAsset(),
			Limit:         "0",
			SourceAccount: orderKp.Address(),
		},
		&txnbuild.AccountMerge{
			Destination:   c.sponsor.Address(),
			SourceAccount: orderKp.Address(),
		},
	}

	hash, err := c.submit(&sponsorAccount, ops, orderKp, "reclaim")
	if err != nil {
		return "", err
	}
	c.log.Info("reclaimed unused deposit account", "account", orderKp.Address(), "tx", hash)
	return hash, nil
}

// submit builds, signs and submits a transaction sourced from the sponsor.
//
// The sponsor signs because it is the source and pays the fee; the order
// account signs for the operations that act on it.
func (c *Client) submit(sponsorAccount txnbuild.Account, ops []txnbuild.Operation, orderKp *keypair.Full, label string) (string, error) {
	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        sponsorAccount,
		IncrementSequenceNum: true,
		BaseFee:              c.baseFee,
		Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewTimeout(180)},
		Operations:           ops,
	})
	if err != nil {
		return "", fmt.Errorf("stellar: build %s transaction: %w", label, err)
	}
	tx, err = tx.Sign(c.passphrase, c.sponsor, orderKp)
	if err != nil {
		return "", fmt.Errorf("stellar: sign %s transaction: %w", label, err)
	}

	// Timed, because this is where the seconds are. A submission blocks until
	// the transaction makes it into a closed ledger, so "the sweep took six
	// seconds" is almost always Stellar rather than anything here — and that is
	// only visible if the wait is measured where it happens.
	started := time.Now()
	resp, err := c.horizon.SubmitTransaction(tx)
	if err != nil {
		c.call("submit:"+label, orderKp.Address(), started, describeHorizonError(err),
			"ops", len(ops))
		return "", fmt.Errorf("stellar: submit %s transaction: %w", label, describeHorizonError(err))
	}
	c.log.Info("horizon transaction submitted",
		"component", "horizon", "op", "submit:"+label,
		"account", orderKp.Address(), "ops", len(ops),
		"tx", resp.Hash, "ledger", resp.Ledger,
		"elapsed_ms", time.Since(started).Milliseconds())
	return resp.Hash, nil
}
