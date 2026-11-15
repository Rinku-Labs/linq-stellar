// Command scf-d4-evidence exports the records behind the SCF Tranche 2
// Deliverable 4 submission into a directory: CSV files, a failure timeline and
// a summary of which criteria the records meet. It only reads.
//
//	DATABASE_URL=... scf-d4-evidence \
//	  -integrator int_... -business 42 -since 2026-11-09T10:00:00Z \
//	  -credential-integrator int_... -demo-integrator int_... \
//	  -reconciliation-url https://.../reconciliation -out evidence/
//
// With LINQ_API_URL and LINQ_INTERNAL_SECRET set, each disbursement's bank
// reference is looked up from the Linq backend's GET /internal/payout.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/Rinku-Labs/linq-stellar/internal/evidence"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "scf-d4-evidence:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		o        evidence.Options
		business uint64
		since    string
		out      string
	)
	flag.StringVar(&o.Integrator, "integrator", "", "integrator whose live orders are the proof (required)")
	flag.Uint64Var(&business, "business", 0, "only this merchant business's orders (the unaffiliated one)")
	flag.StringVar(&since, "since", "", "RFC3339 time routing was switched on; earlier orders are not counted (required)")
	flag.StringVar(&o.CredentialIntegrator, "credential-integrator", "", "test integrator for the key rotation and revocation log")
	flag.StringVar(&o.DemoIntegrator, "demo-integrator", "", "Linq test integrator the induced failures ran on")
	flag.StringVar(&o.Network, "network", "public", "public or testnet, for Stellar Expert links")
	flag.StringVar(&o.ReconciliationURL, "reconciliation-url", "", "the reconciliation dashboard's address")
	flag.StringVar(&out, "out", "scf-d4-evidence", "directory to write into")
	flag.Parse()

	if o.Integrator == "" || since == "" {
		return fmt.Errorf("-integrator and -since are required")
	}
	t, err := time.Parse(time.RFC3339, since)
	if err != nil {
		return fmt.Errorf("-since: %w", err)
	}
	o.Since, o.BusinessID = t, uint(business)

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		return err
	}
	// Read only, whatever the role allows. One connection, kept open, so the
	// setting holds for every query.
	sql, err := db.DB()
	if err != nil {
		return err
	}
	sql.SetMaxOpenConns(1)
	sql.SetMaxIdleConns(1)
	sql.SetConnMaxLifetime(0)
	if err := db.Exec("SET SESSION CHARACTERISTICS AS TRANSACTION READ ONLY").Error; err != nil {
		return fmt.Errorf("set read only: %w", err)
	}

	s, err := evidence.Export(db, o, lookup(), out)
	if err != nil {
		return err
	}
	fmt.Printf("wrote %s: %d payments, %d disbursements, %d reconciliation cycles\n",
		out, s.Payments, s.Disbursements, s.Cycles)
	return nil
}

// lookup asks the Linq backend for a payout's bank reference. Nil when the
// backend is not configured.
func lookup() evidence.PayoutLookup {
	base := strings.TrimRight(os.Getenv("LINQ_API_URL"), "/")
	secret := os.Getenv("LINQ_INTERNAL_SECRET")
	if base == "" || secret == "" {
		return nil
	}
	client := &http.Client{Timeout: 20 * time.Second}
	return func(reference string) (string, error) {
		req, err := http.NewRequest(http.MethodGet, base+"/internal/payout?reference="+url.QueryEscape(reference), nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("X-Internal-Secret", secret)
		res, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return "", fmt.Errorf("GET /internal/payout returned %d", res.StatusCode)
		}
		var body struct {
			BankReference string `json:"bankReference"`
		}
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			return "", err
		}
		return body.BankReference, nil
	}
}
