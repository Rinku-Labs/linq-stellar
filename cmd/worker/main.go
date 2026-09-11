// Command worker runs settlement: deposit detection, NGN payout dispatch, and
// the on-chain sweep, refund and reclaim.
//
// Every worker is driven from order state in the database rather than from an
// in-flight queue message. That is what makes a restart uneventful — there is
// no delivery to lose and no watch to resume from the beginning; whatever is
// still in a working state is picked up on the next sweep.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/Rinku-Labs/linq-stellar/internal/config"
	"github.com/Rinku-Labs/linq-stellar/internal/notify"
	"github.com/Rinku-Labs/linq-stellar/internal/payout"
	"github.com/Rinku-Labs/linq-stellar/internal/pool"
	"github.com/Rinku-Labs/linq-stellar/internal/settle"
	"github.com/Rinku-Labs/linq-stellar/internal/stellar"
	"github.com/Rinku-Labs/linq-stellar/internal/store"
	"github.com/Rinku-Labs/linq-stellar/internal/wake"
)

func main() {
	if err := run(); err != nil {
		slog.Error("worker exited", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)

	db, err := openDB(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	if err := store.Migrate(db); err != nil {
		return err
	}

	chain, err := stellar.New(stellar.Config{
		HorizonURL:        cfg.HorizonURL,
		NetworkPassphrase: cfg.NetworkPassphrase,
		USDCIssuer:        cfg.USDCIssuer,
		SponsorSeed:       cfg.SponsorKey,
		EncryptionKey:     cfg.EncryptionKey,
		BaseFee:           cfg.BaseFee,
		Logger:            log,
	})
	if err != nil {
		return err
	}

	// Fail here rather than on the first sweep: a treasury without a USDC
	// trustline cannot receive anything, and every sweep would fail after the
	// merchant had already been paid.
	if trusts, err := chain.TrustsUSDC(cfg.TreasuryWallet); err != nil {
		log.Warn("could not verify the treasury trustline at startup", "error", err)
	} else if !trusts {
		log.Error("treasury has no USDC trustline; sweeps will fail",
			"treasury", cfg.TreasuryWallet)
	}

	provider, err := payout.NewLinqAPI(cfg.LinqAPIURL, cfg.LinqAPISecret)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// One bus, shared by every loop, bridged onto Postgres so a signal from the
	// API server — which runs as its own deployment — reaches the loops here.
	bus := wake.New()
	bus.Bridge(wake.Notifier{DB: db, Log: log}.Notify)
	store.OnTransition(waker(bus))
	go wake.Listen(ctx, db, bus, log)

	deposits := &settle.Deposits{
		DB:      db,
		Chain:   chain,
		Payouts: payout.StateQueue{},
		Log:     log.With("component", "settle"),
	}

	// Six loops, run together. The streamer detects deposits in about a
	// second; the scanner is the backstop that catches whatever the streamer
	// dropped across a reconnect. Both enter settlement through the same claim.
	// The notifier is what tells the Linq backend any of it happened.
	//
	// Each gets a logger tagged with which loop it is. Without that, five loops
	// interleaved in one stream produce a log where every line is true and the
	// one question you have during an incident — which of these is stuck — has
	// to be answered by recognising message wording.
	workers := []func(context.Context){
		(&settle.Streamer{
			DB: db, Deposits: deposits, Log: log.With("component", "streamer"),
			Max: cfg.MaxStreams, Bus: bus,
		}).Run,
		(&settle.Scanner{
			DB: db, Deposits: deposits, Log: log.With("component", "scanner"),
			Interval: cfg.ScanInterval, Bus: bus,
		}).Run,
		(&payout.Worker{
			DB: db, Provider: provider, Log: log.With("component", "payout"), Bus: bus,
		}).Run,
		(&settle.ChainWorker{
			DB: db, Chain: chain, Treasury: cfg.TreasuryWallet,
			Log: log.With("component", "chain"), Bus: bus,
		}).Run,
		(&notify.Worker{
			DB:     db,
			Log:    log.With("component", "notify"),
			URL:    cfg.MerchantWebhookURL,
			Secret: cfg.MerchantWebhookSecret,
			Bus:    bus,
		}).Run,
		// Sixth: the pool keeper. It is the only loop that does no work for an
		// order already in flight — it works for the next one, so that accepting
		// it costs a database round-trip instead of a ledger.
		(&pool.Keeper{
			DB: db, Chain: chain, Log: log.With("component", "pool"),
			Size: cfg.AccountPoolSize, Bus: bus,
		}).Run,
	}

	log.Info("worker starting",
		"sponsor", chain.SponsorAddress(),
		"treasury", cfg.TreasuryWallet,
		"usdcIssuer", chain.USDCAsset().Issuer,
		// The resolved endpoint, not the raw setting. STELLAR_HORIZON_URL is
		// optional and falls back to the public network, so logging the config
		// value printed an empty string on every default deployment and left
		// the one question this line exists to answer — which Horizon are we
		// actually talking to — unanswered.
		"horizon", chain.Horizon().HorizonURL)

	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func(fn func(context.Context)) {
			defer wg.Done()
			fn(ctx)
		}(w)
	}

	<-ctx.Done()
	log.Info("shutting down; waiting for workers to finish their current pass")
	wg.Wait()
	return nil
}

// waker turns "an order moved" into "wake whoever owns what comes next".
//
// This is the whole of the hand-off between the loops, in one place on purpose.
// Each state below is the start of some other loop's queue, so entering it is
// the moment that loop should stop waiting — and reading this function is how
// you find out, six months from now, why a queue you just added is still being
// worked at its polling interval.
//
// Two omissions are deliberate. A payout released for another attempt
// (payout_processing -> payout_queued) is not signalled: the interval between
// attempts is the only thing standing between a provider having a bad minute
// and this service spending all three of an order's attempts inside one second.
// The same goes for a sweep or refund handed back after a Horizon failure.
func waker(bus *wake.Bus) func(orderID, from, to string) {
	return func(_, _, to string) {
		switch to {
		case store.StateAwaitingDeposit:
			bus.Signal(wake.Deposits)
		case store.StatePayoutQueued:
			bus.Signal(wake.Payouts)
		case store.StateSweepQueued, store.StateRefundQueued, store.StateExpired:
			bus.Signal(wake.Chain)
		}
		// Reported states are signalled on top of the above, not instead of
		// them: disbursed both queues a sweep and owes the merchant an email,
		// and an order that only got one of those is the bug this replaced.
		if notify.Notifiable(to) {
			bus.Signal(wake.Notify)
		}
	}
}
