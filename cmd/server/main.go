// Command server exposes the HTTP surface: order creation and status, the
// SEP-1 stellar.toml, SEP-10 authentication and the trustline preflight.
//
// It runs separately from the worker so a slow Horizon call in settlement can
// never make the API stop answering, and so the two can be scaled apart.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Rinku-Labs/linq-stellar/internal/api"
	"github.com/Rinku-Labs/linq-stellar/internal/config"
	"github.com/Rinku-Labs/linq-stellar/internal/sep"
	"github.com/Rinku-Labs/linq-stellar/internal/stellar"
	"github.com/Rinku-Labs/linq-stellar/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server exited", "error", err)
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

	srv := &api.Server{
		DB:            db,
		Chain:         chain,
		HomeDomain:    cfg.HomeDomain,
		DepositWindow: cfg.DepositWindow,
		Log:           log,
	}

	// SEP-7 signing is optional. Without a key the URIs are still valid; wallets
	// that check simply show them as unverified.
	if cfg.SEP10SigningKey != "" {
		signer, err := sep.NewURISigner(cfg.SEP10SigningKey)
		if err != nil {
			return err
		}
		srv.URISigner = signer
	}

	// SEP-10 is optional too. A half-configured authenticator that issues
	// challenges nothing can verify is worse than one plainly switched off, so
	// it is all-or-nothing.
	if cfg.SEP10Enabled() {
		auth, err := sep.NewAuthenticator(sep.AuthConfig{
			ServerSigningSeed: cfg.SEP10SigningKey,
			HomeDomain:        cfg.HomeDomain,
			WebAuthDomain:     cfg.WebAuthDomain,
			NetworkPassphrase: chain.NetworkPassphrase(),
			ChallengeTimeout:  cfg.ChallengeTimeout,
			JWTSecret:         cfg.SEP10JWTSecret,
			JWTIssuer:         cfg.HomeDomain,
			Accounts:          chain.Horizon(),
		})
		if err != nil {
			return err
		}
		srv.Auth = auth
		log.Info("sep-10 enabled", "homeDomain", cfg.HomeDomain, "signingKey", auth.SigningKey())
	} else {
		log.Warn("sep-10 disabled: signing key, jwt secret or home domain not configured")
	}

	srv.TOML = buildTOML(cfg, chain, srv)

	httpSrv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("server listening",
			"port", cfg.Port,
			"sponsor", chain.SponsorAddress(),
			"treasury", cfg.TreasuryWallet,
			"usdcIssuer", chain.USDCAsset().Issuer)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("listen failed", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

// buildTOML assembles the SEP-1 file.
//
// ACCOUNTS lists the sponsor and treasury deliberately: it lets anyone audit
// provisioning and settlement volume directly on Horizon, without asking Linq
// for anything.
func buildTOML(cfg config.Config, chain *stellar.Client, srv *api.Server) sep.TOMLConfig {
	t := sep.TOMLConfig{
		NetworkPassphrase: chain.NetworkPassphrase(),
		Accounts:          []string{chain.SponsorAddress(), cfg.TreasuryWallet},
		OrgName:           cfg.OrgName,
		OrgURL:            cfg.OrgURL,
		OrgDescription:    cfg.OrgDescription,
		OrgEmail:          cfg.OrgEmail,
		Currencies: []sep.Currency{{
			Code:     "USDC",
			Issuer:   chain.USDCAsset().Issuer,
			Status:   "live",
			Decimals: 7,
			Name:     "USD Coin",
		}},
	}
	if srv.Auth != nil {
		t.SigningKey = srv.Auth.SigningKey()
		t.WebAuthEndpoint = "https://" + cfg.WebAuthDomain + "/sep10/auth"
	}
	if srv.URISigner != nil {
		t.URIRequestSigningKey = srv.URISigner.Address()
	}
	return t
}
