package settle

import (
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/uselinq/linq-stellar/internal/stellar"
	"github.com/uselinq/linq-stellar/internal/store"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		db.Exec("DELETE FROM stellar_orders")
		db.Exec("DELETE FROM stellar_stream_cursors")
	})
	return db
}

// countingPayouts records how many times an order was queued for disbursement.
type countingPayouts struct {
	calls atomic.Int64
	err   error
}

func (p *countingPayouts) Enqueue(string) error {
	p.calls.Add(1)
	return p.err
}

func waitingOrder(t *testing.T, db *gorm.DB) *store.Order {
	t.Helper()
	o := &store.Order{
		ID:              "order-1",
		Status:          store.StateAwaitingDeposit,
		Rate:            1655,
		FeeUSDC:         0,
		ManualDeposit:   true,
		DepositAddress:  "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H",
		DepositDeadline: time.Now().UTC().Add(10 * time.Minute),
	}
	if err := db.Create(o).Error; err != nil {
		t.Fatalf("create order: %v", err)
	}
	return o
}

func newDeposits(db *gorm.DB, p Payouts) *Deposits {
	return &Deposits{
		DB:      db,
		Payouts: p,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// The property the whole design rests on: however many detectors see the same
// deposit, exactly one credits it.
func TestConcurrentDetectorsCreditOnce(t *testing.T) {
	db := testDB(t)
	order := waitingOrder(t, db)
	payouts := &countingPayouts{}
	d := newDeposits(db, payouts)

	const racers = 8
	var wg sync.WaitGroup
	var wins atomic.Int64

	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			o := *order
			won, err := d.Record(&o, 50, stellar.DepositInfo{TxHash: "tx", From: "GPAYER"})
			if err != nil {
				t.Errorf("record: %v", err)
			}
			if won {
				wins.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := wins.Load(); got != 1 {
		t.Errorf("%d detectors won the claim, want exactly 1", got)
	}
	if got := payouts.calls.Load(); got != 1 {
		t.Errorf("payout queued %d times, want exactly 1", got)
	}

	var after store.Order
	if err := db.First(&after, "id = ?", order.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.Status != store.StatePayoutQueued {
		t.Errorf("status = %q, want %q", after.Status, store.StatePayoutQueued)
	}
	if after.AmountUSDC != 50 {
		t.Errorf("amountUsdc = %v, want 50", after.AmountUSDC)
	}
	if after.DepositFrom != "GPAYER" {
		t.Errorf("depositFrom = %q, want GPAYER", after.DepositFrom)
	}
}

// A claimed order whose payout could not be queued must go back to waiting,
// not sit in a state nothing is working on.
func TestFailedEnqueueReleasesTheClaim(t *testing.T) {
	db := testDB(t)
	order := waitingOrder(t, db)
	d := newDeposits(db, &countingPayouts{err: errStub})

	won, err := d.Record(order, 50, stellar.DepositInfo{})
	if won {
		t.Error("reported a win despite the payout failing to queue")
	}
	if err == nil {
		t.Error("expected an error when the payout could not be queued")
	}

	var after store.Order
	if err := db.First(&after, "id = ?", order.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.Status != store.StateAwaitingDeposit {
		t.Errorf("status = %q, want the order back at %q", after.Status, store.StateAwaitingDeposit)
	}
}

// A precomputed order keeps the NGN it was quoted; a manual one reconciles to
// what actually arrived.
func TestManualDepositReconcilesNGN(t *testing.T) {
	for _, tc := range []struct {
		name    string
		manual  bool
		wantNGN float64
	}{
		{"manual reconciles", true, 50 * 1655},
		{"precomputed keeps quote", false, 82750},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			o := waitingOrder(t, db)
			o.ManualDeposit = tc.manual
			o.AmountNGN = 82750
			db.Save(o)

			d := newDeposits(db, &countingPayouts{})
			if _, err := d.Record(o, 50, stellar.DepositInfo{}); err != nil {
				t.Fatalf("record: %v", err)
			}

			var after store.Order
			db.First(&after, "id = ?", o.ID)
			if after.AmountNGN != tc.wantNGN {
				t.Errorf("amountNgn = %v, want %v", after.AmountNGN, tc.wantNGN)
			}
		})
	}
}

func TestReconcile(t *testing.T) {
	cases := []struct {
		name               string
		balance, fee, rate float64
		wantPayable        float64
		wantErr            bool
	}{
		{"zero fee pays the full deposit", 50, 0, 1655, 50, false},
		{"fee comes off the top", 50, 1, 1655, 49, false},
		{"nothing arrived", 0, 0, 1655, 0, true},
		{"negative balance", -1, 0, 1655, 0, true},
		{"deposit equals the fee", 1, 1, 1655, 0, true},
		{"deposit below the fee", 0.5, 1, 1655, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payable, ngn, err := Reconcile(tc.balance, tc.fee, tc.rate)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error, got none")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if payable != tc.wantPayable {
				t.Errorf("payable = %v, want %v", payable, tc.wantPayable)
			}
			if want := tc.wantPayable * tc.rate; ngn != want {
				t.Errorf("ngn = %v, want %v", ngn, want)
			}
		})
	}
}

func TestExpireOnlyMovesWaitingOrders(t *testing.T) {
	db := testDB(t)
	order := waitingOrder(t, db)
	d := newDeposits(db, &countingPayouts{})

	if err := d.Expire(order); err != nil {
		t.Fatalf("expire: %v", err)
	}
	var after store.Order
	db.First(&after, "id = ?", order.ID)
	if after.Status != store.StateExpired {
		t.Fatalf("status = %q, want expired", after.Status)
	}

	// Expiring again must not resurrect or re-transition a finished order.
	if err := d.Expire(order); err != nil {
		t.Fatalf("second expire: %v", err)
	}
	db.First(&after, "id = ?", order.ID)
	if after.Status != store.StateExpired {
		t.Errorf("status = %q after second expire, want expired", after.Status)
	}
}

var errStub = stubError("payout unavailable")

type stubError string

func (e stubError) Error() string { return string(e) }
