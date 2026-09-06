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
	"github.com/Rinku-Labs/linq-stellar/internal/payout"
	"github.com/Rinku-Labs/linq-stellar/internal/settle"
	"github.com/Rinku-Labs/linq-stellar/internal/stellar"
	"github.com/Rinku-Labs/linq-stellar/internal/store"
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

	deposits := &settle.Deposits{
		DB:      db,
		Chain:   chain,
		Payouts: payout.StateQueue{},
		Log:     log,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Four loops, run together. The streamer detects deposits in about a
	// second; the scanner is the backstop that catches whatever the streamer
	// dropped across a reconnect. Both enter settlement through the same claim.
	workers := []func(context.Context){
		(&settle.Streamer{
			DB: db, Deposits: deposits, Log: log, Max: cfg.MaxStreams,
		}).Run,
		(&settle.Scanner{
			DB: db, Deposits: deposits, Log: log, Interval: cfg.ScanInterval,
		}).Run,
		(&payout.Worker{
			DB: db, Provider: provider, Log: log,
		}).Run,
		(&settle.ChainWorker{
			DB: db, Chain: chain, Treasury: cfg.TreasuryWallet, Log: log,
		}).Run,
	}

	log.Info("worker starting",
		"sponsor", chain.SponsorAddress(),
		"treasury", cfg.TreasuryWallet,
		"usdcIssuer", chain.USDCAsset().Issuer,
		"horizon", cfg.HorizonURL)

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
