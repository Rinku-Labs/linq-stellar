package stellar

import (
	"fmt"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/txnbuild"
)

// Provision makes a freshly generated keypair usable as a USDC deposit address.
//
// Unlike Solana or the EVM chains, a Stellar address cannot receive a token
// until the account exists on-chain and holds a trustline for that asset. Both
// cost XLM base reserves. Sponsored reserves let the sponsor account fund them
// and reclaim every lumen when the order account is closed by Sweep or Reclaim,
// so the standing cost of an unused deposit account is nothing at all.
//
// All four operations share one transaction, so the account cannot end up
// existing without its trustline — a state in which a payer's USDC would bounce
// off an address Linq had already published.
//
// Returns the transaction hash, which is worth keeping: it is the public proof
// that Linq opened the account rather than the payer.
func (c *Client) Provision(encryptedSeed string) (string, error) {
	orderKp, err := c.orderKeypair(encryptedSeed)
	if err != nil {
		return "", err
	}

	sponsorAccount, err := c.horizon.AccountDetail(horizonclient.AccountRequest{AccountID: c.sponsor.Address()})
	if err != nil {
		return "", fmt.Errorf("stellar: load sponsor account: %w", describeHorizonError(err))
	}

	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &sponsorAccount,
		IncrementSequenceNum: true,
		BaseFee:              c.baseFee,
		Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewTimeout(180)},
		Operations: []txnbuild.Operation{
			&txnbuild.BeginSponsoringFutureReserves{SponsoredID: orderKp.Address()},
			&txnbuild.CreateAccount{Destination: orderKp.Address(), Amount: "0"},
			&txnbuild.ChangeTrust{
				Line:          c.usdc.MustToChangeTrustAsset(),
				Limit:         txnbuild.MaxTrustlineLimit,
				SourceAccount: orderKp.Address(),
			},
			&txnbuild.EndSponsoringFutureReserves{SourceAccount: orderKp.Address()},
		},
	})
	if err != nil {
		return "", fmt.Errorf("stellar: build provisioning transaction: %w", err)
	}

	// Both signatures are required: the sponsor is paying the reserves, and the
	// new account must consent both to being sponsored and to the trustline.
	tx, err = tx.Sign(c.passphrase, c.sponsor, orderKp)
	if err != nil {
		return "", fmt.Errorf("stellar: sign provisioning transaction: %w", err)
	}

	resp, err := c.horizon.SubmitTransaction(tx)
	if err != nil {
		return "", fmt.Errorf("stellar: submit provisioning transaction: %w", describeHorizonError(err))
	}

	c.log.Info("provisioned stellar deposit account",
		"account", orderKp.Address(), "tx", resp.Hash)
	return resp.Hash, nil
}
