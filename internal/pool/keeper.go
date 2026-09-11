// Package pool keeps a supply of provisioned Stellar deposit accounts ready,
// so accepting an order does not mean waiting for a ledger to close.
//
// Provisioning is four operations and a ledger wait — five to eight seconds —
// and none of it depends on the order it is done for. Running it inside POST
// /orders put that wait in front of every payer, for work any other order could
// have used. Here it runs ahead of time, in batches of twenty per transaction,
// and an order takes an account that is already on-chain.
//
// The pool is a buffer, never a dependency: an order that finds it empty
// provisions inline exactly as it did before, and is slow rather than refused.
package pool

import (
	"context"
	"log/slog"
	"time"

	"github.com/Rinku-Labs/linq-stellar/internal/stellar"
	"github.com/Rinku-Labs/linq-stellar/internal/store"
	"github.com/Rinku-Labs/linq-stellar/internal/wake"
	"gorm.io/gorm"
)

const (
	// defaultInterval is the unprompted top-up. The pool is normally refilled
	// by the signal a claim sends, so this is the backstop that covers a
	// signal lost to a restart or a connection pooler.
	defaultInterval = 60 * time.Second
	// defaultBatch is how many to mint per transaction.
	defaultBatch = 10
	// healBatch bounds how many unconfirmed rows one pass resolves. Each costs
	// a Horizon read, and they only appear after a crash mid-provision.
	healBatch = 20
)

// Keeper tops the deposit-account pool back up.
type Keeper struct {
	DB    *gorm.DB
	Chain *stellar.Client
	Log   *slog.Logger

	// Size is the target number of ready accounts. Zero or less disables the
	// pool entirely, which returns order creation to provisioning inline — and
	// to making the payer wait a ledger for it.
	Size     int
	Batch    int
	Interval time.Duration
	Bus      *wake.Bus
}

// Run keeps the pool full until the context is cancelled.
func (k *Keeper) Run(ctx context.Context) {
	if k.size() == 0 {
		k.Log.Info("deposit account pool disabled; orders will provision inline")
		return
	}
	interval := k.Interval
	if interval <= 0 {
		interval = defaultInterval
	}

	wake.Loop{
		Name:     "pool keeper",
		Topic:    wake.Pool,
		Interval: interval,
		Bus:      k.Bus,
		Log:      k.Log,
		Fields:   []any{"target", k.size(), "batch", k.batch()},
		Work:     k.pass,
	}.Run(ctx)
}

func (k *Keeper) size() int {
	if k.Size <= 0 {
		return 0
	}
	return k.Size
}

func (k *Keeper) batch() int {
	if k.Batch > 0 {
		return k.Batch
	}
	return defaultBatch
}

// pass resolves anything left half-done and then tops the pool up.
func (k *Keeper) pass(ctx context.Context) (worked bool) {
	defer func() {
		if rec := recover(); rec != nil {
			k.Log.Error("pool keeper panicked", "panic", rec)
		}
	}()

	healed := k.heal()

	ready, err := store.ReadyPoolAccounts(k.DB)
	if err != nil {
		k.Log.Error("could not count the deposit account pool", "error", err)
		return healed
	}

	short := k.size() - int(ready)
	if short <= 0 {
		return healed
	}
	if short > k.batch() {
		short = k.batch()
	}
	if ctx.Err() != nil {
		return healed
	}

	k.mint(short, int(ready))
	// Reported as work whether or not the mint succeeded: a pool that is short
	// is not idle, and letting the loop back off here would leave it refilling
	// two minutes after a busy spell drained it.
	return true
}

// mint provisions one batch of accounts and records them.
func (k *Keeper) mint(count, ready int) {
	accounts := make([]stellar.NewAccount, 0, count)
	rows := make([]store.PoolAccount, 0, count)
	for i := 0; i < count; i++ {
		address, seed, err := k.Chain.GenerateAccount()
		if err != nil {
			k.Log.Error("could not generate a deposit account", "error", err)
			return
		}
		accounts = append(accounts, stellar.NewAccount{Address: address, EncryptedSeed: seed})
		rows = append(rows, store.PoolAccount{Address: address, EncryptedSeed: seed})
	}

	// Recorded before submitting. These seeds are the only way back to the
	// accounts the next line is about to pay reserves for, and a crash in
	// between would otherwise strand them with nothing to find them by.
	if err := store.ReservePoolAccounts(k.DB, rows); err != nil {
		k.Log.Error("could not record pooled accounts before provisioning", "error", err)
		return
	}
	ids := make([]uint, len(rows))
	for i := range rows {
		ids[i] = rows[i].ID
	}

	started := time.Now()
	hash, err := k.Chain.ProvisionBatch(accounts)
	if err != nil {
		// The rows stay, unready. The next pass asks Horizon whether the
		// accounts exist — a submission can fail after it has actually applied
		// — and either adopts or drops them.
		k.Log.Error("could not provision pooled accounts",
			"count", count, "elapsed_ms", time.Since(started).Milliseconds(), "error", err)
		return
	}

	if err := store.MarkPoolReady(k.DB, ids, hash); err != nil {
		k.Log.Error("provisioned accounts but could not mark them ready",
			"count", count, "tx", hash, "error", err)
		return
	}

	k.Log.Info("topped up the deposit account pool",
		"minted", count, "ready_before", ready, "target", k.size(),
		"tx", hash, "elapsed_ms", time.Since(started).Milliseconds())
}

// heal resolves rows whose provisioning transaction never reported back.
//
// A submission that times out has not necessarily failed — it may have applied
// and only the response been lost — so the account itself is the authority.
// Asking Horizon is the only way to tell an account that exists from one that
// never did, and guessing either way costs money: dropping a real account
// strands its reserves, adopting a missing one hands a payer an address that
// bounces.
func (k *Keeper) heal() bool {
	pending, err := store.PendingPoolAccounts(k.DB, healBatch)
	if err != nil {
		k.Log.Error("could not read unconfirmed pooled accounts", "error", err)
		return false
	}
	if len(pending) == 0 {
		return false
	}

	for _, account := range pending {
		trusts, err := k.Chain.TrustsUSDC(account.Address)
		if err != nil {
			// Horizon being unreachable is not evidence either way. Leave it.
			k.Log.Warn("could not confirm a pooled account, leaving it for the next pass",
				"account", account.Address, "error", err)
			continue
		}
		if trusts {
			if err := store.MarkPoolReady(k.DB, []uint{account.ID}, account.ProvisionTxHash); err != nil {
				k.Log.Error("could not adopt a provisioned account",
					"account", account.Address, "error", err)
				continue
			}
			k.Log.Info("adopted an account whose provisioning was never confirmed",
				"account", account.Address)
			continue
		}
		if err := store.DropPoolAccount(k.DB, account.ID); err != nil {
			k.Log.Error("could not drop an unprovisioned account",
				"account", account.Address, "error", err)
			continue
		}
		k.Log.Info("dropped a pooled account that was never created",
			"account", account.Address)
	}
	return true
}
