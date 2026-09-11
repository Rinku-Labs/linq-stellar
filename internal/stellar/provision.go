package stellar

import (
	"fmt"

	"github.com/stellar/go-stellar-sdk/keypair"
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

	// Both signatures are required: the sponsor is paying the reserves, and the
	// new account must consent both to being sponsored and to the trustline.
	hash, err := c.submitSponsored("provision", []*keypair.Full{c.sponsor, orderKp}, []txnbuild.Operation{
		&txnbuild.BeginSponsoringFutureReserves{SponsoredID: orderKp.Address()},
		&txnbuild.CreateAccount{Destination: orderKp.Address(), Amount: "0"},
		&txnbuild.ChangeTrust{
			Line:          c.usdc.MustToChangeTrustAsset(),
			Limit:         txnbuild.MaxTrustlineLimit,
			SourceAccount: orderKp.Address(),
		},
		&txnbuild.EndSponsoringFutureReserves{SourceAccount: orderKp.Address()},
	})
	if err != nil {
		return "", err
	}

	c.log.Info("provisioned stellar deposit account",
		"component", "horizon", "account", orderKp.Address(), "tx", hash)
	return hash, nil
}

// maxBatchAccounts bounds one provisioning transaction. Stellar allows 100
// operations per transaction and each account costs four, so this is the
// network's limit rounded down to something that still fits comfortably inside
// one ledger.
const maxBatchAccounts = 20

// NewAccount is a deposit account that has been created but not yet handed to
// anyone.
type NewAccount struct {
	Address       string
	EncryptedSeed string
}

// ProvisionBatch creates several deposit accounts in a single transaction.
//
// Provisioning is the slow half of accepting an order: submitting a transaction
// means waiting for a ledger to close, which is five seconds on a good day and
// was eight in the run that prompted this. Doing it while a payer waits put
// that entire wait on the checkout screen, for work that has nothing to do with
// their particular order — any account would have done.
//
// So they are made ahead of time, several at once. One transaction for twenty
// accounts costs one ledger wait and one fee rather than twenty of each, and
// the accounts sit in the pool until an order needs one. See internal/store's
// deposit pool and the keeper in cmd/worker.
//
// Either the whole transaction applies or none of it does, so a batch cannot
// leave half-built accounts behind — an account without its trustline would
// bounce a payer's USDC.
func (c *Client) ProvisionBatch(accounts []NewAccount) (string, error) {
	if len(accounts) == 0 {
		return "", fmt.Errorf("stellar: no accounts to provision")
	}
	if len(accounts) > maxBatchAccounts {
		return "", fmt.Errorf("stellar: %d accounts is more than the %d one transaction holds",
			len(accounts), maxBatchAccounts)
	}

	signers := []*keypair.Full{c.sponsor}
	ops := make([]txnbuild.Operation, 0, len(accounts)*4)
	for _, account := range accounts {
		kp, err := c.orderKeypair(account.EncryptedSeed)
		if err != nil {
			return "", err
		}
		if kp.Address() != account.Address {
			// The seed and the address disagree, which means the row being
			// provisioned is not the account it claims to be. Publishing that
			// address would take a payer's USDC into an account nothing here
			// holds the key to.
			return "", fmt.Errorf("stellar: seed for %s decrypts to %s", account.Address, kp.Address())
		}
		signers = append(signers, kp)
		ops = append(ops,
			&txnbuild.BeginSponsoringFutureReserves{SponsoredID: kp.Address()},
			&txnbuild.CreateAccount{Destination: kp.Address(), Amount: "0"},
			&txnbuild.ChangeTrust{
				Line:          c.usdc.MustToChangeTrustAsset(),
				Limit:         txnbuild.MaxTrustlineLimit,
				SourceAccount: kp.Address(),
			},
			&txnbuild.EndSponsoringFutureReserves{SourceAccount: kp.Address()},
		)
	}

	// Every new account signs for its own sponsorship and trustline, alongside
	// the sponsor that pays for them.
	hash, err := c.submitSponsored("provision_batch", signers, ops)
	if err != nil {
		return "", err
	}

	c.log.Info("provisioned stellar deposit accounts",
		"component", "horizon", "count", len(accounts), "tx", hash)
	return hash, nil
}
