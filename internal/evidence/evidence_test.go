package evidence

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/Rinku-Labs/linq-stellar/internal/store"
)

const (
	live    = "int_live"
	creds   = "int_creds"
	demo    = "int_demo"
	bankAcc = "0123456789"
	bankNm  = "Ada Obi Secret"
)

var switchedOn = time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)

// testDB is SQLite, or a fresh Postgres schema when LINQ_TEST_PG_DSN points at
// a local database: production is Postgres, and the export's queries must run
// there.
func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	if dsn := os.Getenv("LINQ_TEST_PG_DSN"); dsn != "" {
		return postgresDB(t, dsn)
	}
	name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:evidence_%s?mode=memory&cache=shared", name)),
		&gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sql, err := db.DB(); err == nil {
			sql.Close()
		}
	})
	return db
}

func postgresDB(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	if !strings.Contains(dsn, "host=127.0.0.1") && !strings.Contains(dsn, "host=localhost") {
		t.Fatalf("LINQ_TEST_PG_DSN must point at localhost")
	}
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("evidence_%d", time.Now().UnixNano())
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		if sql, err := admin.DB(); err == nil {
			sql.Close()
		}
	})
	db, err := gorm.Open(postgres.Open(dsn+" search_path="+schema), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sql, err := db.DB(); err == nil {
			sql.Close()
		}
	})
	return db
}

func at(minutes int) *time.Time {
	t := switchedOn.Add(time.Duration(minutes) * time.Minute)
	return &t
}

func mustCreate(t *testing.T, db *gorm.DB, v any) {
	t.Helper()
	if err := db.Create(v).Error; err != nil {
		t.Fatalf("create %T: %v", v, err)
	}
}

func liveOrder(id string, created int, business uint, disbursed bool) store.Order {
	o := store.Order{
		ID: id, IntegratorID: live, IdempotencyKey: id, BusinessID: business,
		Status: store.StateAwaitingDeposit, AmountUSDC: 6.05, AmountNGN: 10000,
		BankAccount: bankAcc, AccountName: bankNm, BankName: "GTBank", BankCode: "058",
		DepositAddress: "G" + id, DepositTxHash: "dep-" + id, CreatedAt: *at(created), DepositDeadline: *at(created + 30),
	}
	if disbursed {
		o.Status, o.FinancialOutcome = store.StateSettledInTreasury, "disbursed"
		o.DepositedAt, o.PaidOutAt = at(created+1), at(created+2)
		o.PayoutReference, o.PayoutProvider = "payout-"+id, "nomba"
	}
	return o
}

func seed(t *testing.T, db *gorm.DB) {
	t.Helper()
	// Before the switch: settled elsewhere, never counted.
	mustCreate(t, db, ptr(liveOrder("before", -60, 7, true)))
	// After it: five disbursed for business 7, one for another business.
	for i := 1; i <= 5; i++ {
		mustCreate(t, db, ptr(liveOrder(fmt.Sprintf("o%d", i), i*10, 7, true)))
	}
	mustCreate(t, db, ptr(liveOrder("other-biz", 5, 8, true)))

	// Notifications: one fast, one slow.
	mustCreate(t, db, &store.StatusEvent{OrderID: "o1", Kind: store.KindTransition, To: store.StateDisbursed, At: *at(12), NotifiedAt: ptr(at(12).Add(3 * time.Second))})
	mustCreate(t, db, &store.StatusEvent{OrderID: "o2", Kind: store.KindTransition, To: store.StateDisbursed, At: *at(22), NotifiedAt: ptr(at(22).Add(75 * time.Second))})

	// Credentials: issued, rotated, old key used in the overlap, revoked, rejected.
	mustCreate(t, db, &store.Integrator{ID: creds, Name: "Reviewer"})
	mustCreate(t, db, &store.APIKey{ID: "key_old", IntegratorID: creds, KeyPrefix: "pfx_old", SecretHash: "hash-secret-old",
		CreatedAt: *at(0), ExpiresAt: at(24 * 60), LastUsedAt: at(5), ReplacedBy: "key_new"})
	mustCreate(t, db, &store.APIKey{ID: "key_new", IntegratorID: creds, KeyPrefix: "pfx_new", SecretHash: "hash-secret-new", CreatedAt: *at(2), RevokedAt: at(30)})
	for _, e := range []store.CredentialEvent{
		{IntegratorID: creds, KeyID: "key_old", Action: store.CredentialIssued, Actor: "admin", At: *at(0)},
		{IntegratorID: creds, KeyID: "key_old", Action: store.CredentialRotated, Actor: "integrator", At: *at(2)},
		{IntegratorID: creds, KeyID: "key_new", Action: store.CredentialRevoked, Actor: "admin", At: *at(30)},
		{IntegratorID: creds, KeyID: "key_new", Action: store.CredentialRejectedRevoked, Actor: "system", At: *at(31)},
	} {
		mustCreate(t, db, ptr(e))
	}

	// Induced failures on the demo integrator.
	mustCreate(t, db, &store.Order{ID: "transient", IntegratorID: demo, IdempotencyKey: "t", Status: store.StateSettledInTreasury,
		FinancialOutcome: "disbursed", InducedFailure: store.InducedTransient, DepositTxHash: "dep-t", BankAccount: bankAcc,
		AccountName: bankNm, CreatedAt: *at(0), DepositDeadline: *at(30)})
	mustCreate(t, db, &store.PayoutAttempt{OrderID: "transient", Number: 1, Reference: "transient", Outcome: store.PayoutFailed, Induced: true, Reason: store.InducedReason})
	mustCreate(t, db, &store.PayoutAttempt{OrderID: "transient", Number: 2, Reference: "transient-p2", Outcome: store.PayoutPaid})
	mustCreate(t, db, &store.StatusEvent{OrderID: "transient", Kind: store.KindTransition, From: store.StatePayoutProcessing, To: store.StatePayoutQueued, Reason: store.InducedReason, At: *at(3)})
	mustCreate(t, db, &store.Order{ID: "terminal", IntegratorID: demo, IdempotencyKey: "x", Status: store.StateRefunded,
		FinancialOutcome: "refunded", InducedFailure: store.InducedTerminal, DepositTxHash: "dep-x", RefundTxHash: "refund-x",
		CreatedAt: *at(0), DepositDeadline: *at(30)})

	// Twelve cycles, eleven clean, one stopped early; one mismatch, resolved.
	for i := 0; i < 12; i++ {
		c := store.ReconciliationCycle{StartedAt: *at(i * 15), FinishedAt: at(i*15 + 1)}
		if i == 11 {
			c.Error = "stopped Horizon checks early: rate limited"
		}
		mustCreate(t, db, &c)
	}
	mustCreate(t, db, &store.ReconciliationItem{CycleID: 1, OrderID: "o1", CheckName: "payout", Result: store.ReconcileMismatch,
		Expected: "paid", Actual: "not paid", Resolution: "provider delay", ResolvedBy: "tanta", ResolvedAt: at(20)})
}

func ptr[T any](v T) *T { return &v }

func export(t *testing.T, db *gorm.DB, lookup PayoutLookup) (Summary, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Export(db, Options{
		Integrator: live, BusinessID: 7, Since: switchedOn,
		CredentialIntegrator: creds, DemoIntegrator: demo, ReconciliationURL: "https://example.test/reconciliation",
	}, lookup, dir)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	return s, dir
}

func readCSV(t *testing.T, path string) [][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return rows[1:]
}

func TestTheExportCountsOnlyTheBusinessesOrdersAfterTheSwitch(t *testing.T) {
	db := testDB(t)
	seed(t, db)
	s, dir := export(t, db, func(ref string) (string, error) { return "NOMBA-" + ref, nil })

	if s.Payments != 5 || s.Disbursements != 5 {
		t.Errorf("payments %d, disbursements %d; want 5 and 5 (no order before the switch, none from another business)", s.Payments, s.Disbursements)
	}
	rows := readCSV(t, filepath.Join(dir, "disbursements.csv"))
	if len(rows) != 5 || rows[0][0] != "o1" || rows[0][6] != "NOMBA-o1" || rows[0][9] != "60" {
		t.Errorf("first disbursement row = %v", rows[0])
	}
	if s.BankReferences != 5 {
		t.Errorf("bank references = %d, want 5", s.BankReferences)
	}
}

func TestTheExportAsksForTheLastRealReference(t *testing.T) {
	db := testDB(t)
	seed(t, db)
	mustCreate(t, db, &store.PayoutAttempt{OrderID: "o1", Number: 1, Reference: "o1", Outcome: store.PayoutFailed})
	mustCreate(t, db, &store.PayoutAttempt{OrderID: "o1", Number: 2, Reference: "o1-p2", Outcome: store.PayoutPaid})
	var asked []string
	export(t, db, func(ref string) (string, error) { asked = append(asked, ref); return "B", nil })
	if asked[0] != "o1-p2" {
		t.Errorf("asked about %q for o1, want its last reference o1-p2", asked[0])
	}
}

func TestTheSummaryMarksEachCriterion(t *testing.T) {
	db := testDB(t)
	seed(t, db)
	s, dir := export(t, db, nil)

	switch {
	case s.Issued != 1 || s.Rotated != 1 || s.Revoked != 1 || s.RejectedRevoked != 1:
		t.Errorf("credential counts = %+v", s)
	case !s.OldKeyUsedInOverlap:
		t.Errorf("the old key's use after rotation was not seen")
	case len(s.TransientRecovered) != 1 || len(s.TerminalRefunded) != 1:
		t.Errorf("failures: transient %v, terminal %v", s.TransientRecovered, s.TerminalRefunded)
	case s.Cycles != 11 || s.CycleErrors != 1:
		t.Errorf("cycles %d clean, %d stopped; want 11 and 1", s.Cycles, s.CycleErrors)
	case s.Mismatches != 1 || s.UnresolvedMismatches != 0:
		t.Errorf("mismatches %d, unresolved %d", s.Mismatches, s.UnresolvedMismatches)
	case s.Notifications != 2 || s.NotificationsOverTarget != 1:
		t.Errorf("notifications %d, over 60 s %d; want 2 and 1", s.Notifications, s.NotificationsOverTarget)
	}

	summary, _ := os.ReadFile(filepath.Join(dir, "summary.md"))
	text := string(summary)
	for _, want := range []string{
		"| 5+ NGN disbursements | 5 | ✅ met",
		"| 10+ reconciliation cycles | 11 finished cleanly (1 stopped early) | ✅ met",
		"| Bank payout reference per disbursement | not looked up | ❌ not yet",
		"| Merchant notified within 60 seconds | 2 deliveries, 1 over 60 s, slowest 75.0 s | ❌ not yet",
		"https://example.test/reconciliation",
		"are not counted",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("summary is missing %q:\n%s", want, text)
		}
	}

	failures, _ := os.ReadFile(filepath.Join(dir, "failures.md"))
	for _, want := range []string{"induced on purpose", "transient-p2", store.InducedReason, "stellar.expert/explorer/public/tx/refund-x"} {
		if !strings.Contains(string(failures), want) {
			t.Errorf("failures.md is missing %q", want)
		}
	}
}

func TestNothingPrivateIsExported(t *testing.T) {
	db := testDB(t)
	seed(t, db)
	_, dir := export(t, db, func(string) (string, error) { return "B", nil })

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		body, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		for _, private := range []string{bankAcc, bankNm, "GTBank", "hash-secret", "pfx_"} {
			if strings.Contains(string(body), private) {
				t.Errorf("%s contains %q", e.Name(), private)
			}
		}
	}
}

func TestAnEmptyDatabaseSaysWhatIsMissing(t *testing.T) {
	db := testDB(t)
	s, dir := export(t, db, nil)
	if s.Payments != 0 || s.Cycles != 0 {
		t.Errorf("summary = %+v", s)
	}
	summary, _ := os.ReadFile(filepath.Join(dir, "summary.md"))
	if strings.Count(string(summary), "❌ not yet") < 8 {
		t.Errorf("an empty export should mark every criterion not yet met:\n%s", summary)
	}
}
