package stellar

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/protocols/horizon/operations"
)

// The reads below are what reconciliation checks the books against. None of
// them signs or submits anything: they are lookups of what the ledger already
// says, so the engine that uses them can be read-only by construction.

// ErrRateLimited means Horizon refused the request for being over its rate
// limit. A caller that gets it should stop asking for a while rather than
// spend the rest of its calls being refused.
var ErrRateLimited = errors.New("stellar: horizon rate limit reached")

// AccountState is what Horizon says about an account right now.
type AccountState struct {
	// Exists is false once an account has been merged away, or before it was
	// ever created.
	Exists bool
	// TrustsUSDC and USDC describe this deployment's USDC: the trustline, and
	// the balance on it (zero without one).
	TrustsUSDC bool
	USDC       float64
}

// AccountState looks an account up.
func (c *Client) AccountState(address string) (AccountState, error) {
	started := time.Now()
	account, err := c.horizon.AccountDetail(horizonclient.AccountRequest{AccountID: address})
	if err != nil {
		if horizonclient.IsNotFoundError(err) {
			c.call("account", address, started, nil, "exists", false)
			return AccountState{}, nil
		}
		err = horizonError(err)
		c.call("account", address, started, err)
		return AccountState{}, err
	}
	state := AccountState{Exists: true}
	for _, b := range account.Balances {
		if b.Asset.Code == c.usdc.Code && b.Asset.Issuer == c.usdc.Issuer {
			v, parseErr := strconv.ParseFloat(b.Balance, 64)
			if parseErr != nil {
				return AccountState{}, fmt.Errorf("stellar: parse usdc balance %q: %w", b.Balance, parseErr)
			}
			state.TrustsUSDC, state.USDC = true, v
		}
	}
	c.call("account", address, started, nil, "exists", true, "usdc", state.USDC)
	return state, nil
}

// Movement is one operation in a transaction that moved funds: a payment, or
// an account merge (which moves an account's remaining lumens).
type Movement struct {
	Kind string // "payment" or "account_merge"
	From string
	To   string
	// USDC is true for a payment in this deployment's USDC; Amount is then in
	// USDC. Merges and other assets carry Amount zero.
	USDC   bool
	Amount float64
}

// LedgerTx is a transaction as the ledger recorded it.
type LedgerTx struct {
	// Found is false when Horizon has no transaction with this hash.
	Found bool
	// Successful is false for a transaction that was included but failed: it
	// is on the ledger and paid its fee, and moved nothing.
	Successful bool
	Movements  []Movement
}

// maxTxOperations bounds the operations read for one transaction. The ones
// this service submits have at most four.
const maxTxOperations = 200

// LedgerTransaction reads a transaction's operations, failed or not.
func (c *Client) LedgerTransaction(hash string) (LedgerTx, error) {
	started := time.Now()
	page, err := c.horizon.Operations(horizonclient.OperationRequest{
		ForTransaction: hash,
		IncludeFailed:  true,
		Limit:          maxTxOperations,
	})
	if err != nil {
		if horizonclient.IsNotFoundError(err) {
			c.call("transaction", "", started, nil, "tx", hash, "found", false)
			return LedgerTx{}, nil
		}
		err = horizonError(err)
		c.call("transaction", "", started, err, "tx", hash)
		return LedgerTx{}, err
	}
	tx := LedgerTx{Found: len(page.Embedded.Records) > 0, Successful: len(page.Embedded.Records) > 0}
	for _, record := range page.Embedded.Records {
		switch op := record.(type) {
		case operations.Payment:
			tx.Successful = tx.Successful && op.TransactionSuccessful
			usdc := op.Code == c.usdc.Code && op.Issuer == c.usdc.Issuer
			m := Movement{Kind: "payment", From: op.From, To: op.To, USDC: usdc}
			if usdc {
				v, parseErr := strconv.ParseFloat(op.Amount, 64)
				if parseErr != nil {
					return LedgerTx{}, fmt.Errorf("stellar: parse payment amount %q: %w", op.Amount, parseErr)
				}
				m.Amount = v
			}
			tx.Movements = append(tx.Movements, m)
		case operations.AccountMerge:
			tx.Successful = tx.Successful && op.TransactionSuccessful
			tx.Movements = append(tx.Movements, Movement{Kind: "account_merge", From: op.Account, To: op.Into})
		default:
			tx.Successful = tx.Successful && record.IsTransactionSuccessful()
		}
	}
	c.call("transaction", "", started, nil, "tx", hash, "found", tx.Found, "successful", tx.Successful)
	return tx, nil
}

// SponsoredPage is one page of the accounts this deployment's sponsor pays
// reserves for.
type SponsoredPage struct {
	Accounts []string
	// Next is where the following page starts. Empty means this was the last.
	Next string
}

// SponsoredAccounts lists the accounts the sponsor pays reserves for, oldest
// first, starting after cursor ("" for the beginning).
func (c *Client) SponsoredAccounts(cursor string, limit int) (SponsoredPage, error) {
	started := time.Now()
	sponsor := c.sponsor.Address()
	page, err := c.horizon.Accounts(horizonclient.AccountsRequest{
		Sponsor: sponsor,
		Cursor:  cursor,
		Limit:   uint(limit),
		Order:   horizonclient.OrderAsc,
	})
	if err != nil {
		err = horizonError(err)
		c.call("sponsored_accounts", sponsor, started, err)
		return SponsoredPage{}, err
	}
	out := SponsoredPage{}
	for _, a := range page.Embedded.Records {
		out.Accounts = append(out.Accounts, a.AccountID)
	}
	if n := len(page.Embedded.Records); n > 0 && n >= limit {
		out.Next = page.Embedded.Records[n-1].PagingToken()
	}
	c.call("sponsored_accounts", sponsor, started, nil, "accounts", len(out.Accounts))
	return out, nil
}

// horizonError describes a Horizon failure, and marks a rate-limit refusal so
// a caller can tell it apart from an outage.
func horizonError(err error) error {
	if hErr := horizonclient.GetError(err); hErr != nil {
		limited := hErr.Problem.Status == http.StatusTooManyRequests ||
			(hErr.Response != nil && hErr.Response.StatusCode == http.StatusTooManyRequests)
		if limited {
			return fmt.Errorf("%w: %v", ErrRateLimited, describeHorizonError(err))
		}
	}
	return describeHorizonError(err)
}
