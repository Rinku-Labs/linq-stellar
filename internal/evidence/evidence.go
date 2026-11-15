// Package evidence exports the records behind the SCF Tranche 2 Deliverable 4
// submission (SD-10).
//
// Every figure in the submission must be checkable: a transaction hash on the
// Stellar ledger, a payout reference, a timestamped row. This package reads
// those rows and writes them out as CSV and Markdown, with a summary that says
// plainly which criteria the records meet and which they do not yet.
//
// It reads only. It never selects bank account numbers, account names or bank
// names, and it never prints a key's secret or hash.
package evidence

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/Rinku-Labs/linq-stellar/internal/store"
)

// Thresholds from the D4 success criteria.
const (
	MinDisbursements   = 5
	MinCycles          = 10
	NotificationTarget = 60 * time.Second
)

// Options says whose records to export.
type Options struct {
	// Integrator is the API caller whose live orders are the proof: the
	// integrator Linq-v2 uses after SD-8.
	Integrator string
	// BusinessID narrows Integrator's orders to one merchant business, the
	// unaffiliated one. Zero means every business.
	BusinessID uint
	// Since is when routing to the public API was switched on for that
	// business. Orders before it settled through an earlier implementation
	// and are not counted.
	Since time.Time
	// CredentialIntegrator is the test integrator whose key issue, rotation
	// and revocation are shown.
	CredentialIntegrator string
	// DemoIntegrator is the Linq test integrator the induced failures ran on.
	DemoIntegrator string
	// Network is "public" or "testnet", for Stellar Expert links.
	Network string
	// ReconciliationURL is where the dashboard is, for the summary.
	ReconciliationURL string
}

// PayoutLookup returns the bank's own reference for a payout, if the payout
// provider reports one. Nil skips the lookup.
type PayoutLookup func(reference string) (bankReference string, err error)

// Summary is what the export found, criterion by criterion.
type Summary struct {
	Payments, Disbursements, Cycles, CycleErrors   int
	Mismatches, UnresolvedMismatches               int
	Issued, Rotated, Revoked, RejectedRevoked      int
	OldKeyUsedInOverlap                            bool
	TransientRecovered, TerminalRefunded           []string
	Notifications, NotificationsOverTarget         int
	SlowestNotification                            time.Duration
	BankReferences                                 int
}

// Export writes every artifact into dir and returns the summary.
func Export(db *gorm.DB, o Options, lookup PayoutLookup, dir string) (Summary, error) {
	var s Summary
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return s, err
	}
	if o.Network == "" {
		o.Network = "public"
	}
	steps := []func() error{
		func() error { return payments(db, o, dir, &s) },
		func() error { return disbursements(db, o, lookup, dir, &s) },
		func() error { return credentials(db, o, dir, &s) },
		func() error { return failures(db, o, dir, &s) },
		func() error { return reconciliation(db, dir, &s) },
		func() error { return notifications(db, o, dir, &s) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return s, err
		}
	}
	return s, writeSummary(o, s, filepath.Join(dir, "summary.md"))
}

// orderColumns are the only order columns read. No bank details.
var orderColumns = []string{
	"id", "integrator_id", "business_id", "created_at", "status", "financial_outcome",
	"deposit_address", "deposit_tx_hash", "sweep_tx_hash", "refund_tx_hash",
	"amount_usdc", "amount_ngn", "payout_reference", "payout_provider",
	"deposited_at", "paid_out_at", "refunded_at", "induced_failure",
}

func liveOrders(db *gorm.DB, o Options) *gorm.DB {
	q := db.Model(&store.Order{}).Select(orderColumns).
		Where("integrator_id = ? AND created_at >= ?", o.Integrator, o.Since)
	if o.BusinessID != 0 {
		q = q.Where("business_id = ?", o.BusinessID)
	}
	return q.Order("created_at ASC, id ASC")
}

func explorer(network, kind, id string) string {
	if id == "" {
		return ""
	}
	return fmt.Sprintf("https://stellar.expert/explorer/%s/%s/%s", network, kind, id)
}

func stamp(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func writeCSV(path string, header []string, rows [][]string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	if err := w.Write(header); err != nil {
		f.Close()
		return err
	}
	if err := w.WriteAll(rows); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func money(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// --- Live payments -------------------------------------------------------------

func payments(db *gorm.DB, o Options, dir string, s *Summary) error {
	var orders []store.Order
	if err := liveOrders(db, o).Where("deposit_tx_hash <> ''").Find(&orders).Error; err != nil {
		return fmt.Errorf("evidence: live payments: %w", err)
	}
	rows := make([][]string, 0, len(orders))
	for _, x := range orders {
		rows = append(rows, []string{
			x.ID, strconv.FormatUint(uint64(x.BusinessID), 10), x.CreatedAt.UTC().Format(time.RFC3339),
			x.Status, money(x.AmountUSDC), x.DepositTxHash, explorer(o.Network, "tx", x.DepositTxHash),
		})
	}
	s.Payments = len(rows)
	return writeCSV(filepath.Join(dir, "payments.csv"),
		[]string{"order_id", "business_id", "created_at", "status", "amount_usdc", "deposit_tx_hash", "explorer"}, rows)
}

// --- NGN disbursements -----------------------------------------------------------

func disbursements(db *gorm.DB, o Options, lookup PayoutLookup, dir string, s *Summary) error {
	var orders []store.Order
	if err := liveOrders(db, o).Where("financial_outcome = ?", "disbursed").Find(&orders).Error; err != nil {
		return fmt.Errorf("evidence: disbursements: %w", err)
	}
	rows := make([][]string, 0, len(orders))
	for _, x := range orders {
		bank := ""
		if lookup != nil {
			ref, err := lastReference(db, x.ID)
			if err != nil {
				return err
			}
			if b, err := lookup(ref); err == nil {
				bank = b
			} else {
				bank = "lookup failed: " + err.Error()
			}
			if bank != "" && !strings.HasPrefix(bank, "lookup failed") {
				s.BankReferences++
			}
		}
		seconds := ""
		if x.DepositedAt != nil && x.PaidOutAt != nil {
			seconds = strconv.FormatFloat(x.PaidOutAt.Sub(*x.DepositedAt).Seconds(), 'f', 0, 64)
		}
		rows = append(rows, []string{
			x.ID, x.DepositTxHash, explorer(o.Network, "tx", x.DepositTxHash), money(x.AmountNGN),
			x.PayoutReference, x.PayoutProvider, bank, stamp(x.DepositedAt), stamp(x.PaidOutAt), seconds,
		})
	}
	s.Disbursements = len(rows)
	return writeCSV(filepath.Join(dir, "disbursements.csv"), []string{
		"order_id", "deposit_tx_hash", "explorer", "amount_ngn", "linq_payout_id", "payout_provider",
		"bank_reference", "deposited_at", "paid_out_at", "seconds_deposit_to_payout",
	}, rows)
}

// lastReference is the reference of an order's last real payout attempt, or its
// ID when it has none on record.
func lastReference(db *gorm.DB, orderID string) (string, error) {
	var refs []string
	err := db.Model(&store.PayoutAttempt{}).Where("order_id = ? AND induced = ?", orderID, false).
		Order("number DESC").Limit(1).Pluck("reference", &refs).Error
	if err != nil {
		return "", fmt.Errorf("evidence: payout reference for %s: %w", orderID, err)
	}
	if len(refs) == 0 {
		return orderID, nil
	}
	return refs[0], nil
}

// --- Credentials ----------------------------------------------------------------

func credentials(db *gorm.DB, o Options, dir string, s *Summary) error {
	if o.CredentialIntegrator == "" {
		return writeCSV(filepath.Join(dir, "credential-events.csv"),
			[]string{"at", "key_id", "action", "actor", "note"}, nil)
	}
	var events []store.CredentialEvent
	if err := db.Where("integrator_id = ?", o.CredentialIntegrator).Order("at ASC, id ASC").
		Find(&events).Error; err != nil {
		return fmt.Errorf("evidence: credential events: %w", err)
	}
	rows := make([][]string, 0, len(events))
	for _, e := range events {
		rows = append(rows, []string{e.At.UTC().Format(time.RFC3339), e.KeyID, e.Action, e.Actor, e.Note})
		switch e.Action {
		case store.CredentialIssued:
			s.Issued++
		case store.CredentialRotated:
			s.Rotated++
		case store.CredentialRevoked:
			s.Revoked++
		case store.CredentialRejectedRevoked:
			s.RejectedRevoked++
		}
	}
	if err := writeCSV(filepath.Join(dir, "credential-events.csv"),
		[]string{"at", "key_id", "action", "actor", "note"}, rows); err != nil {
		return err
	}

	// The keys themselves, without prefix, secret or hash. A rotated key's last
	// use falling inside its overlap is the old key still working after
	// rotation.
	var keys []store.APIKey
	if err := db.Omit("secret_hash", "key_prefix").Where("integrator_id = ?", o.CredentialIntegrator).
		Order("created_at ASC").Find(&keys).Error; err != nil {
		return fmt.Errorf("evidence: api keys: %w", err)
	}
	rotatedAt := map[string]time.Time{}
	for _, e := range events {
		if e.Action == store.CredentialRotated {
			rotatedAt[e.KeyID] = e.At
		}
	}
	keyRows := make([][]string, 0, len(keys))
	for _, k := range keys {
		inOverlap := ""
		if at, ok := rotatedAt[k.ID]; ok && k.LastUsedAt != nil && k.LastUsedAt.After(at) &&
			(k.ExpiresAt == nil || !k.LastUsedAt.After(*k.ExpiresAt)) {
			inOverlap = "yes"
			s.OldKeyUsedInOverlap = true
		}
		keyRows = append(keyRows, []string{
			k.ID, k.CreatedAt.UTC().Format(time.RFC3339), stamp(k.ExpiresAt), stamp(k.RevokedAt),
			stamp(k.LastUsedAt), k.ReplacedBy, inOverlap,
		})
	}
	return writeCSV(filepath.Join(dir, "api-keys.csv"), []string{
		"key_id", "created_at", "expires_at", "revoked_at", "last_used_at", "replaced_by", "used_after_rotation_within_overlap",
	}, keyRows)
}

// --- Induced failures --------------------------------------------------------------

func failures(db *gorm.DB, o Options, dir string, s *Summary) error {
	path := filepath.Join(dir, "failures.md")
	if o.DemoIntegrator == "" {
		return os.WriteFile(path, []byte("# Induced failures\n\nNo demo integrator given.\n"), 0o644)
	}
	var orders []store.Order
	if err := db.Model(&store.Order{}).Select(orderColumns).
		Where("integrator_id = ? AND induced_failure <> ''", o.DemoIntegrator).
		Order("created_at ASC").Find(&orders).Error; err != nil {
		return fmt.Errorf("evidence: induced orders: %w", err)
	}

	var b strings.Builder
	b.WriteString("# Induced failures\n\n")
	b.WriteString("These failures were induced on purpose, on Linq's own test orders, with ")
	b.WriteString("`allow_failure_injection` on the integrator. An induced attempt is recorded ")
	b.WriteString("with the reason `induced for demonstration` and never reaches the payout provider. ")
	b.WriteString("Everything after it (the retry, the refund, the notification) ran as it does for any order.\n")
	if len(orders) == 0 {
		b.WriteString("\nNo induced orders yet.\n")
	}
	for _, x := range orders {
		events, err := store.StatusHistory(db, x.ID)
		if err != nil {
			return fmt.Errorf("evidence: history of %s: %w", x.ID, err)
		}
		attempts, err := store.PayoutAttempts(db, x.ID)
		if err != nil {
			return fmt.Errorf("evidence: attempts of %s: %w", x.ID, err)
		}
		var deliveries []store.WebhookDelivery
		if err := db.Where("order_id = ?", x.ID).Order("at ASC").Find(&deliveries).Error; err != nil {
			return fmt.Errorf("evidence: deliveries of %s: %w", x.ID, err)
		}

		fmt.Fprintf(&b, "\n## Order %s (%s)\n\n", x.ID, x.InducedFailure)
		fmt.Fprintf(&b, "- Deposit: %s\n", orDash(explorer(o.Network, "tx", x.DepositTxHash)))
		if x.RefundTxHash != "" {
			fmt.Fprintf(&b, "- Refund: %s\n", explorer(o.Network, "tx", x.RefundTxHash))
		}
		fmt.Fprintf(&b, "- Final status: `%s`\n\n", x.Status)

		b.WriteString("| Time (UTC) | Move | Reason |\n|---|---|---|\n")
		for _, e := range events {
			if e.Kind != "" && e.Kind != store.KindTransition {
				continue
			}
			move := e.To
			if e.From != "" {
				move = e.From + " → " + e.To
			}
			fmt.Fprintf(&b, "| %s | %s | %s |\n", e.At.UTC().Format(time.RFC3339), move, e.Reason)
		}

		b.WriteString("\n| Attempt | Reference | Outcome | Induced | Started (UTC) |\n|---|---|---|---|---|\n")
		for _, a := range attempts {
			fmt.Fprintf(&b, "| %d | %s | %s | %v | %s |\n", a.Number, a.Reference, a.Outcome, a.Induced,
				a.CreatedAt.UTC().Format(time.RFC3339))
		}

		if len(deliveries) > 0 {
			b.WriteString("\n| Webhook (UTC) | Status reported | HTTP | Attempt |\n|---|---|---|---|\n")
			for _, d := range deliveries {
				fmt.Fprintf(&b, "| %s | %s | %d | %d |\n", d.At.UTC().Format(time.RFC3339), d.Status, d.ResponseCode, d.Attempt)
			}
		}

		switch {
		case x.InducedFailure == store.InducedTransient && x.FinancialOutcome == "disbursed" && hasInducedThenPaid(attempts):
			s.TransientRecovered = append(s.TransientRecovered, x.ID)
		case x.InducedFailure == store.InducedTerminal && x.Status == store.StateRefunded && x.RefundTxHash != "":
			s.TerminalRefunded = append(s.TerminalRefunded, x.ID)
		}
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func hasInducedThenPaid(attempts []store.PayoutAttempt) bool {
	sawFailure := false
	for _, a := range attempts {
		if a.Outcome == store.PayoutFailed {
			sawFailure = true
		}
		if sawFailure && a.Outcome == store.PayoutPaid {
			return true
		}
	}
	return false
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// --- Reconciliation -------------------------------------------------------------------

func reconciliation(db *gorm.DB, dir string, s *Summary) error {
	var cycles []store.ReconciliationCycle
	if err := db.Order("started_at ASC").Find(&cycles).Error; err != nil {
		return fmt.Errorf("evidence: cycles: %w", err)
	}
	rows := make([][]string, 0, len(cycles))
	for _, c := range cycles {
		if c.FinishedAt != nil {
			if c.Error == "" {
				s.Cycles++
			} else {
				s.CycleErrors++
			}
		}
		rows = append(rows, []string{
			strconv.FormatInt(c.ID, 10), c.StartedAt.UTC().Format(time.RFC3339), stamp(c.FinishedAt),
			strconv.Itoa(c.OrdersChecked), strconv.Itoa(c.AccountsChecked),
			strconv.Itoa(c.Matched), strconv.Itoa(c.Mismatches), c.Error,
		})
	}
	if err := writeCSV(filepath.Join(dir, "reconciliation-cycles.csv"), []string{
		"cycle_id", "started_at", "finished_at", "orders_checked", "accounts_checked", "matched", "mismatches", "error",
	}, rows); err != nil {
		return err
	}

	var items []store.ReconciliationItem
	if err := db.Where("result = ?", store.ReconcileMismatch).Order("id ASC").Find(&items).Error; err != nil {
		return fmt.Errorf("evidence: mismatches: %w", err)
	}
	itemRows := make([][]string, 0, len(items))
	for _, it := range items {
		s.Mismatches++
		if it.ResolvedAt == nil {
			s.UnresolvedMismatches++
		}
		itemRows = append(itemRows, []string{
			strconv.FormatInt(it.ID, 10), strconv.FormatInt(it.CycleID, 10), it.OrderID, it.CheckName,
			it.Account, it.TxHash, it.Expected, it.Actual, it.Resolution, it.ResolvedBy, stamp(it.ResolvedAt),
		})
	}
	return writeCSV(filepath.Join(dir, "reconciliation-mismatches.csv"), []string{
		"item_id", "cycle_id", "order_id", "check", "account", "tx_hash", "expected", "actual",
		"resolution", "resolved_by", "resolved_at",
	}, itemRows)
}

// --- Notification time -------------------------------------------------------------------

func notifications(db *gorm.DB, o Options, dir string, s *Summary) error {
	ids := liveOrders(db, o).Select("id")
	var events []store.StatusEvent
	if err := db.Where("order_id IN (?) AND notified_at IS NOT NULL", ids).
		Order("at ASC").Find(&events).Error; err != nil {
		return fmt.Errorf("evidence: notifications: %w", err)
	}
	rows := make([][]string, 0, len(events))
	var delays []time.Duration
	for _, e := range events {
		d := e.NotifiedAt.Sub(e.At)
		delays = append(delays, d)
		if d > NotificationTarget {
			s.NotificationsOverTarget++
		}
		if d > s.SlowestNotification {
			s.SlowestNotification = d
		}
		rows = append(rows, []string{
			e.OrderID, e.To, e.At.UTC().Format(time.RFC3339Nano), e.NotifiedAt.UTC().Format(time.RFC3339Nano),
			strconv.FormatFloat(d.Seconds(), 'f', 1, 64),
		})
	}
	sort.Slice(delays, func(i, j int) bool { return delays[i] < delays[j] })
	s.Notifications = len(rows)
	return writeCSV(filepath.Join(dir, "notifications.csv"),
		[]string{"order_id", "status", "changed_at", "delivered_at", "seconds"}, rows)
}

// --- Summary -----------------------------------------------------------------------------

func writeSummary(o Options, s Summary, path string) error {
	mark := func(ok bool) string {
		if ok {
			return "✅ met"
		}
		return "❌ not yet"
	}
	var b strings.Builder
	b.WriteString("# SCF D4 evidence summary\n\n")
	fmt.Fprintf(&b, "Generated %s. Live orders counted from %s", time.Now().UTC().Format(time.RFC3339),
		o.Since.UTC().Format(time.RFC3339))
	if o.BusinessID != 0 {
		fmt.Fprintf(&b, " for business %d", o.BusinessID)
	}
	fmt.Fprintf(&b, ", integrator `%s`. Orders before that time settled through an earlier implementation and are not counted.\n\n", o.Integrator)

	b.WriteString("| Criterion | Found | Status | File |\n|---|---|---|---|\n")
	fmt.Fprintf(&b, "| Unaffiliated business's Stellar payments | %d | %s | `payments.csv` |\n", s.Payments, mark(s.Payments > 0))
	fmt.Fprintf(&b, "| 5+ NGN disbursements | %d | %s | `disbursements.csv` |\n", s.Disbursements, mark(s.Disbursements >= MinDisbursements))
	bankNote := "not looked up"
	if s.BankReferences > 0 || s.Disbursements == 0 {
		bankNote = fmt.Sprintf("%d of %d", s.BankReferences, s.Disbursements)
	}
	fmt.Fprintf(&b, "| Bank payout reference per disbursement | %s | %s | `disbursements.csv` |\n", bankNote,
		mark(s.Disbursements > 0 && s.BankReferences == s.Disbursements))
	fmt.Fprintf(&b, "| Credential issue, rotation, revocation | issued %d, rotated %d, revoked %d, rejected after revoke %d | %s | `credential-events.csv` |\n",
		s.Issued, s.Rotated, s.Revoked, s.RejectedRevoked, mark(s.Issued > 0 && s.Rotated > 0 && s.Revoked > 0 && s.RejectedRevoked > 0))
	fmt.Fprintf(&b, "| Old key used during its rotation overlap | %v | %s | `api-keys.csv` |\n", s.OldKeyUsedInOverlap, mark(s.OldKeyUsedInOverlap))
	fmt.Fprintf(&b, "| Transient failure, then a successful retry | %s | %s | `failures.md` |\n", orDash(strings.Join(s.TransientRecovered, ", ")), mark(len(s.TransientRecovered) > 0))
	fmt.Fprintf(&b, "| Terminal failure, notice and on-chain refund | %s | %s | `failures.md` |\n", orDash(strings.Join(s.TerminalRefunded, ", ")), mark(len(s.TerminalRefunded) > 0))
	fmt.Fprintf(&b, "| 10+ reconciliation cycles | %d finished cleanly (%d stopped early) | %s | `reconciliation-cycles.csv` |\n", s.Cycles, s.CycleErrors, mark(s.Cycles >= MinCycles))
	fmt.Fprintf(&b, "| Reconciliation mismatches and resolutions | %d mismatches, %d unresolved | — | `reconciliation-mismatches.csv` |\n", s.Mismatches, s.UnresolvedMismatches)
	fmt.Fprintf(&b, "| Merchant notified within 60 seconds | %d deliveries, %d over 60 s, slowest %.1f s | %s | `notifications.csv` |\n",
		s.Notifications, s.NotificationsOverTarget, s.SlowestNotification.Seconds(), mark(s.Notifications > 0 && s.NotificationsOverTarget == 0))
	if o.ReconciliationURL != "" {
		fmt.Fprintf(&b, "\nReconciliation dashboard: %s\n", o.ReconciliationURL)
	}
	b.WriteString("\nRate limiting and idempotency are shown by `scripts/scf-d4-rate-limit-demo.sh`, whose log is kept beside this export.\n")
	b.WriteString("\nNo bank account numbers, account names, bank names or key secrets are read or written by this export.\n")
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
