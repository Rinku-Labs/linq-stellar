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

	hash, err := c.submitSponsored("sweep", []*keypair.Full{c.sponsor, orderKp}, ops)
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

	hash, err := c.submitSponsored("reclaim", []*keypair.Full{c.sponsor, orderKp}, ops)
	if err != nil {
		return "", err
	}
	c.log.Info("reclaimed unused deposit account", "account", orderKp.Address(), "tx", hash)
	return hash, nil
}

// sponsorSubmitAttempts bounds the retry below. Two more goes is enough to get
// past another process having taken the sequence number in between; past that,
// something other than a race is wrong and retrying only delays finding out.
const sponsorSubmitAttempts = 3

// submitSponsored builds, signs and submits a transaction sourced from the
// sponsor account, and is the only place that does.
//
// Everything here — provisioning, sweeping, refunding, reclaiming — is sourced
// from the one sponsor account, and a Stellar account has one sequence number.
// Two transactions built from the same reading of it are not both valid: the
// second is rejected with tx_bad_seq, having done nothing. That stopped being
// theoretical when accounts started being provisioned ahead of demand, because
// the moment the pool runs dry is exactly the moment an order provisions inline
// while the keeper is minting a batch — the two most likely collisions in the
// service, at the same instant, in different processes.
//
// So two defences. The mutex removes the race inside a process, which is the
// cheap half. The retry covers the other deployment: reload the sequence and
// build again, because a stale sequence is a fact about timing rather than
// about the transaction, and the same transaction will be perfectly valid a
// moment later.
//
// The sponsor signs because it is the source and pays the fee; the accounts the
// operations act on sign for themselves.
func (c *Client) submitSponsored(label string, signers []*keypair.Full, ops []txnbuild.Operation) (string, error) {
	c.sponsorMu.Lock()
	defer c.sponsorMu.Unlock()

	var lastErr error
	for attempt := 1; attempt <= sponsorSubmitAttempts; attempt++ {
		// Loaded inside the loop: the whole point of a retry here is to read
		// the sequence number again.
		loaded := time.Now()
		sponsorAccount, err := c.horizon.AccountDetail(horizonclient.AccountRequest{AccountID: c.sponsor.Address()})
		c.call("sponsor_account", c.sponsor.Address(), loaded, describeHorizonError(err))
		if err != nil {
			return "", fmt.Errorf("stellar: load sponsor account: %w", describeHorizonError(err))
		}

		tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
			SourceAccount:        &sponsorAccount,
			IncrementSequenceNum: true,
			BaseFee:              c.baseFee,
			Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewTimeout(180)},
			Operations:           ops,
		})
		if err != nil {
			return "", fmt.Errorf("stellar: build %s transaction: %w", label, err)
		}
		tx, err = tx.Sign(c.passphrase, signers...)
		if err != nil {
			return "", fmt.Errorf("stellar: sign %s transaction: %w", label, err)
		}

		// Timed, because this is where the seconds are. A submission blocks
		// until the transaction makes it into a closed ledger, so "the sweep
		// took six seconds" is almost always Stellar rather than anything here
		// — and that is only visible if the wait is measured where it happens.
		started := time.Now()
		resp, err := c.horizon.SubmitTransaction(tx)
		if err == nil {
			c.log.Info("horizon transaction submitted",
				"component", "horizon", "op", "submit:"+label,
				"account", c.sponsor.Address(), "ops", len(ops),
				"tx", resp.Hash, "ledger", resp.Ledger, "attempt", attempt,
				"elapsed_ms", time.Since(started).Milliseconds())
			return resp.Hash, nil
		}

		lastErr = describeHorizonError(err)
		if !isBadSequence(err) || attempt == sponsorSubmitAttempts {
			c.call("submit:"+label, c.sponsor.Address(), started, lastErr,
				"ops", len(ops), "attempt", attempt)
			return "", fmt.Errorf("stellar: submit %s transaction: %w", label, lastErr)
		}

		c.log.Warn("sponsor sequence was taken, rebuilding and resubmitting",
			"component", "horizon", "op", "submit:"+label,
			"ops", len(ops), "attempt", attempt,
			"elapsed_ms", time.Since(started).Milliseconds())
	}
	return "", fmt.Errorf("stellar: submit %s transaction: %w", label, lastErr)
}

// isBadSequence reports whether Horizon rejected a transaction for holding a
// sequence number something else had already used.
//
// Distinguished from every other rejection because it is the only one where
// resubmitting the identical transaction is the right response: nothing about
// what it asks for was refused.
func isBadSequence(err error) bool {
	hErr := horizonclient.GetError(err)
	if hErr == nil {
		return false
	}
	codes, codesErr := hErr.ResultCodes()
	if codesErr != nil {
		return false
	}
	return codes.TransactionCode == "tx_bad_seq"
}
