package stellar

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/protocols/horizon/operations"
)

// PaymentEvent is one incoming USDC payment observed on a deposit account.
type PaymentEvent struct {
	TxHash string
	From   string
	Amount float64
	// Cursor is Horizon's paging token for this event. Persist it only after
	// the event has been handled: saved early, a crash skips the payment on
	// reconnect and the payer is never credited.
	Cursor string
}

// StreamUSDCPayments streams incoming USDC payments for one account, calling fn
// for each.
//
// It blocks until the context is cancelled or Horizon drops the connection, and
// returns the error either way — reconnecting is the caller's decision, because
// only the caller knows the last cursor it successfully handled.
//
// Pass the last persisted cursor to resume. An empty cursor starts from the
// beginning of the account's history rather than from "now", so a deposit made
// before the stream first connected is still seen. That matters: an account is
// published to a payer the moment it is provisioned, and the payer does not
// wait for a stream to be ready.
func (c *Client) StreamUSDCPayments(ctx context.Context, address, cursor string, fn func(PaymentEvent) error) error {
	req := horizonclient.OperationRequest{ForAccount: address}
	if cursor != "" {
		req.Cursor = cursor
	}

	// The SDK's handler cannot return an error, so a handler failure is captured
	// here and the stream is cancelled — continuing after a failed handler would
	// advance past a payment nothing recorded.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	c.log.Info("horizon payment stream opened",
		"component", "horizon", "op", "stream:payments",
		"account", address, "cursor", cursor)

	opened := time.Now()
	var handlerErr error
	err := c.horizon.StreamPayments(streamCtx, req, func(op operations.Operation) {
		payment, ok := op.(operations.Payment)
		if !ok {
			return
		}
		if !c.isIncomingUSDC(payment, address) {
			return
		}
		amount, parseErr := strconv.ParseFloat(payment.Amount, 64)
		if parseErr != nil {
			handlerErr = fmt.Errorf("stellar: parse streamed amount %q: %w", payment.Amount, parseErr)
			cancel()
			return
		}
		// The line that says the money arrived, and when this service first knew
		// about it. Everything downstream is timed from here, so it is logged
		// before it is handled rather than after.
		c.log.Info("horizon payment observed",
			"component", "horizon", "op", "stream:payments",
			"account", address, "from", payment.From, "amount", amount,
			"tx", payment.TransactionHash, "cursor", payment.PagingToken())

		if err := fn(PaymentEvent{
			TxHash: payment.TransactionHash,
			From:   payment.From,
			Amount: amount,
			Cursor: payment.PagingToken(),
		}); err != nil {
			handlerErr = err
			cancel()
		}
	})

	c.log.Debug("horizon payment stream closed",
		"component", "horizon", "op", "stream:payments",
		"account", address, "open_ms", time.Since(opened).Milliseconds())

	if handlerErr != nil {
		return handlerErr
	}
	// A cancelled parent context is an orderly shutdown, not a stream failure.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// isIncomingUSDC filters the stream down to deposits.
//
// Outgoing payments appear on the same stream — the sweep out of this very
// account is one — and crediting one as a deposit would pay a merchant for
// Linq's own transfer.
func (c *Client) isIncomingUSDC(p operations.Payment, address string) bool {
	if !p.TransactionSuccessful {
		return false
	}
	if p.To != address {
		return false
	}
	return p.Code == c.usdc.Code && p.Issuer == c.usdc.Issuer
}
