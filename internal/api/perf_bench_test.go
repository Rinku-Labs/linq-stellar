package api

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Rinku-Labs/linq-stellar/internal/stellar"
	"github.com/Rinku-Labs/linq-stellar/internal/store"
	"github.com/stellar/go-stellar-sdk/keypair"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The /v1 hot path on real Postgres. Round trips are what cost here, so SQLite
// would measure the wrong thing. Needs a local LINQ_TEST_PG_DSN.
//
// BENCH_POOL=prod applies the pool cmd/server runs with; anything else is
// database/sql's defaults. Run old and new builds alternately on a laptop.

func benchServer(b *testing.B) (*Server, string) {
	b.Helper()
	dsn := os.Getenv("LINQ_TEST_PG_DSN")
	if dsn == "" {
		b.Skip("needs LINQ_TEST_PG_DSN")
	}
	if !strings.Contains(dsn, "host=127.0.0.1") && !strings.Contains(dsn, "host=localhost") {
		b.Fatal("LINQ_TEST_PG_DSN must be local")
	}
	open := func(dsn string) *gorm.DB {
		db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
		if err != nil {
			b.Fatalf("open: %v", err)
		}
		return db
	}
	schema := fmt.Sprintf("bench_%d", time.Now().UnixNano())
	admin := open(dsn)
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		b.Fatalf("schema: %v", err)
	}
	b.Cleanup(func() { admin.Exec("DROP SCHEMA " + schema + " CASCADE") })

	db := open(dsn + " search_path=" + schema)
	if err := store.Migrate(db); err != nil {
		b.Fatalf("migrate: %v", err)
	}
	sql, err := db.DB()
	if err != nil {
		b.Fatalf("pool: %v", err)
	}
	b.Cleanup(func() { _ = sql.Close() })
	if os.Getenv("BENCH_POOL") == "prod" {
		sql.SetMaxOpenConns(20)
		sql.SetMaxIdleConns(20)
	} else {
		// Bounded only so a parallel run cannot exhaust the local server.
		sql.SetMaxOpenConns(90)
	}

	sponsor, _ := keypair.Random()
	chain, err := stellar.New(stellar.Config{
		SponsorSeed:   sponsor.Seed(),
		EncryptionKey: []byte("0123456789abcdef0123456789abcdef"),
	})
	if err != nil {
		b.Fatalf("chain: %v", err)
	}
	s := &Server{
		DB: db, Chain: chain, OrdersAPIKey: "bench-orders-key",
		PublicAPIEnabled: true, AdminAPIKey: testAdminKey,
		DepositWindow: 30 * time.Minute,
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	limit := 1 << 30 // never refuse: this measures the allowed path
	integrator, err := store.CreateIntegrator(db, "bench", &limit)
	if err != nil {
		b.Fatalf("integrator: %v", err)
	}
	issued, err := store.IssueKey(db, integrator.ID, "live", store.ActorAdmin, time.Now().UTC())
	if err != nil {
		b.Fatalf("issue: %v", err)
	}

	// An order with a real history, for the read path.
	order := store.Order{
		ID: "bench-order", IntegratorID: integrator.ID, IdempotencyKey: "k",
		Status: store.StateAwaitingDeposit, AmountUSDC: 5, Rate: 1655,
		DepositAddress: "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H",
		EncryptedSeed:  strings.Repeat("s", 200),
	}
	legacy := order
	legacy.ID, legacy.IntegratorID = "bench-legacy", store.LegacyIntegratorID
	for _, o := range []*store.Order{&order, &legacy} {
		if err := db.Create(o).Error; err != nil {
			b.Fatalf("order: %v", err)
		}
		for i := 0; i < 6; i++ {
			event := store.StatusEvent{
				OrderID: o.ID, From: "a", To: "b", Kind: store.KindTransition,
				Reason: "because", At: time.Now().UTC(),
			}
			if err := db.Create(&event).Error; err != nil {
				b.Fatalf("event: %v", err)
			}
		}
	}
	return s, issued.Full
}

func benchRequests(b *testing.B, path, key string, parallel bool) {
	s, issued := benchServer(b)
	if key == "" {
		key = issued
	} else {
		key = s.OrdersAPIKey
	}
	handler := s.Routes()
	one := func() {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("X-API-Key", key)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK && w.Code != http.StatusNotFound {
			b.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
	}
	one()
	b.ReportAllocs()
	b.ResetTimer()
	if !parallel {
		for i := 0; i < b.N; i++ {
			one()
		}
		return
	}
	b.SetParallelism(8)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			one()
		}
	})
}

// BenchmarkV1AuthAndLimit is the fixed price of any /v1 request.
func BenchmarkV1AuthAndLimit(b *testing.B) { benchRequests(b, "/v1/orders/none", "", false) }

// BenchmarkV1GetOrder is a status poll, the commonest call an integrator makes.
func BenchmarkV1GetOrder(b *testing.B) { benchRequests(b, "/v1/orders/bench-order", "", false) }

// BenchmarkV1GetOrderParallel is the same poll from many callers on one key,
// which is the worst case for the single counter row.
func BenchmarkV1GetOrderParallel(b *testing.B) { benchRequests(b, "/v1/orders/bench-order", "", true) }

// BenchmarkLegacyGetOrder pins the legacy route.
func BenchmarkLegacyGetOrder(b *testing.B) { benchRequests(b, "/orders/bench-legacy", "legacy", false) }

// BenchmarkControlHealthz touches no database. It should not move between
// builds; if it does, the machine drifted and the run says nothing.
func BenchmarkControlHealthz(b *testing.B) {
	s, _ := benchServer(b)
	handler := s.Routes()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	}
}
