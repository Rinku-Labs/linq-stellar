package settle

import (
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rinku-Labs/linq-stellar/internal/money"
	"github.com/Rinku-Labs/linq-stellar/internal/stellar"
	"github.com/Rinku-Labs/linq-stellar/internal/store"
	"github.com/glebarez/sqlite"
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

// A deposit that covers the quote pays the quote, manual or not. A manual
// order pays more for an overpayment; neither pays less than what was promised.
func TestManualDepositReconcilesNGN(t *testing.T) {
	for _, tc := range []struct {
		name     string
		manual   bool
		received float64
		wantNGN  float64
	}{
		{"manual pays for an overpayment", true, 60, 60 * 1655},
		{"precomputed caps at the quote", false, 60, 82750},
		{"manual honours the quote when covered", true, 50, 82750},
		{"precomputed honours the quote", false, 50, 82750},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			o := waitingOrder(t, db)
			o.ManualDeposit = tc.manual
			o.AmountNGN, o.QuotedNGN, o.QuotedUSDC = 82750, 82750, 50
			db.Save(o)

			d := newDeposits(db, &countingPayouts{})
			if _, err := d.Record(o, tc.received, stellar.DepositInfo{}); err != nil {
				t.Fatalf("record: %v", err)
			}

			var after store.Order
			db.First(&after, "id = ?", o.ID)
			if after.AmountNGN != tc.wantNGN {
				t.Errorf("amountNgn = %v, want %v", after.AmountNGN, tc.wantNGN)
			}
			if after.Underpaid {
				t.Error("a covered deposit was recorded as underpaid")
			}
		})
	}
}

// The incident, end to end: a ₦100 order quoted at 0.073303 USDC must pay the
// merchant ₦100, not the ₦95.49 that recomputing from a rounded-down deposit
// produced.
func TestCoveredDepositPaysTheInvoice(t *testing.T) {
	db := testDB(t)
	o := waitingOrder(t, db)
	o.Rate = 1364.21
	o.ManualDeposit = true
	o.AmountNGN, o.QuotedNGN, o.QuotedUSDC = 100, 100, 0.073303
	db.Save(o)

	d := newDeposits(db, &countingPayouts{})
	if _, err := d.Record(o, 0.073303, stellar.DepositInfo{}); err != nil {
		t.Fatalf("record: %v", err)
	}

	var after store.Order
	db.First(&after, "id = ?", o.ID)
	if after.AmountNGN != 100 {
		t.Errorf("amountNgn = %v, want the invoiced 100", after.AmountNGN)
	}
}

// An underpayment still pays out — the money is real — but it is recorded as
// one rather than quietly becoming a smaller payout.
func TestUnderpaymentIsPaidAndFlagged(t *testing.T) {
	db := testDB(t)
	o := waitingOrder(t, db)
	o.Rate = 1364.21
	o.ManualDeposit = true
	o.AmountNGN, o.QuotedNGN, o.QuotedUSDC = 100, 100, 0.073303
	db.Save(o)

	d := newDeposits(db, &countingPayouts{})
	// What the old two-decimal quote asked a payer for.
	if _, err := d.Record(o, 0.07, stellar.DepositInfo{}); err != nil {
		t.Fatalf("record: %v", err)
	}

	var after store.Order
	db.First(&after, "id = ?", o.ID)
	if !after.Underpaid {
		t.Error("a deposit 4.5% below the quote was not flagged as underpaid")
	}
	if want := 95.49; after.AmountNGN != want {
		t.Errorf("amountNgn = %v, want %v (the value of what arrived)", after.AmountNGN, want)
	}
	if want := 4.51; after.ShortfallNGN != want {
		t.Errorf("shortfallNgn = %v, want %v", after.ShortfallNGN, want)
	}
}

// Orders created before the quote columns existed read as zero and must keep
// behaving exactly as they did: paid for whatever arrived.
func TestOrderWithoutAQuotePaysForWhatArrived(t *testing.T) {
	db := testDB(t)
	o := waitingOrder(t, db)
	o.ManualDeposit = true
	db.Save(o)

	d := newDeposits(db, &countingPayouts{})
	if _, err := d.Record(o, 50, stellar.DepositInfo{}); err != nil {
		t.Fatalf("record: %v", err)
	}

	var after store.Order
	db.First(&after, "id = ?", o.ID)
	if want := 50 * 1655.0; after.AmountNGN != want {
		t.Errorf("amountNgn = %v, want %v", after.AmountNGN, want)
	}
	if after.Underpaid {
		t.Error("an open-amount order was flagged as underpaid")
	}
}

// An order already in flight when the quote columns shipped carries its quote
// in AmountUSDC/AmountNGN, and must settle by the same rule as a new one.
//
// The underpaid case is a deliberate change of behaviour. A fixed-amount order
// used to keep its quoted NGN whatever arrived, so a payer who sent a tenth of
// the invoice still had the merchant credited in full and Linq absorbed the
// rest — free money for anyone who noticed. Now a short deposit pays what it
// is worth and is flagged, on fixed and manual orders alike.
func TestInFlightOrderUsesItsPreQuoteColumns(t *testing.T) {
	for _, tc := range []struct {
		name     string
		manual   bool
		received float64
		wantNGN  float64
	}{
		{"fixed order covered in full is paid the quote", false, 0.073303, 100},
		{"fixed order underpaid is paid what arrived", false, 0.07, 95.49},
		{"manual order is paid the invoice once covered", true, 0.073303, 100},
		{"manual order underpaid is paid what arrived", true, 0.07, 95.49},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			o := waitingOrder(t, db)
			o.Rate = 1364.21
			o.ManualDeposit = tc.manual
			// No QuotedUSDC/QuotedNGN, exactly as an existing row reads.
			o.AmountUSDC, o.AmountNGN = 0.073303, 100
			db.Save(o)

			d := newDeposits(db, &countingPayouts{})
			if _, err := d.Record(o, tc.received, stellar.DepositInfo{}); err != nil {
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
		name        string
		in          Deposit
		wantPayable float64
		wantNGN     float64
		wantUnder   bool
		wantErr     bool
	}{
		{
			name:        "zero fee pays the full deposit",
			in:          Deposit{Received: 50, Rate: 1655},
			wantPayable: 50, wantNGN: 82750,
		},
		{
			name:        "fee comes off the top",
			in:          Deposit{Received: 50, Fee: 1, Rate: 1655},
			wantPayable: 49, wantNGN: 81095,
		},
		{
			name:        "a covered quote is paid in full",
			in:          Deposit{Received: 0.073303, Rate: 1364.21, QuotedUSDC: 0.073303, QuotedNGN: 100},
			wantPayable: 0.073303, wantNGN: 100,
		},
		{
			name:        "a quote covered to the dust unit is still covered",
			in:          Deposit{Received: 0.073302, Rate: 1364.21, QuotedUSDC: 0.073303, QuotedNGN: 100},
			wantPayable: 0.073302, wantNGN: 100,
		},
		{
			name:        "the two-decimal underpayment",
			in:          Deposit{Received: 0.07, Rate: 1364.21, QuotedUSDC: 0.073303, QuotedNGN: 100},
			wantPayable: 0.07, wantNGN: 95.49, wantUnder: true,
		},
		{
			name:        "an overpayment on a manual order pays for what arrived",
			in:          Deposit{Received: 1, Rate: 1364.21, QuotedUSDC: 0.073303, QuotedNGN: 100, Manual: true},
			wantPayable: 1, wantNGN: 1364.21,
		},
		{
			name:        "an overpayment on a fixed order pays the quote",
			in:          Deposit{Received: 1, Rate: 1364.21, QuotedUSDC: 0.073303, QuotedNGN: 100},
			wantPayable: 1, wantNGN: 100,
		},
		{name: "nothing arrived", in: Deposit{Rate: 1655}, wantErr: true},
		{name: "negative balance", in: Deposit{Received: -1, Rate: 1655}, wantErr: true},
		{name: "deposit equals the fee", in: Deposit{Received: 1, Fee: 1, Rate: 1655}, wantErr: true},
		{name: "deposit below the fee", in: Deposit{Received: 0.5, Fee: 1, Rate: 1655}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Reconcile(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error, got none")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.PayableUSDC != tc.wantPayable {
				t.Errorf("payable = %v, want %v", got.PayableUSDC, tc.wantPayable)
			}
			if got.PayoutNGN != tc.wantNGN {
				t.Errorf("payoutNgn = %v, want %v", got.PayoutNGN, tc.wantNGN)
			}
			if got.Underpaid != tc.wantUnder {
				t.Errorf("underpaid = %v, want %v", got.Underpaid, tc.wantUnder)
			}
		})
	}
}

// The invariant the whole change exists to hold, checked across a spread of
// invoices and rates rather than at the one figure from the incident: quote an
// invoice, pay exactly that quote, and the merchant receives exactly the
// invoice.
func TestQuotingAndSettlingRoundTripsExactly(t *testing.T) {
	rates := []float64{1364.21, 1362.55, 1655, 1000, 1499.999, 923.4567}
	invoices := []float64{1, 50, 100, 150, 999.99, 25_000, 1_000_000}

	for _, rate := range rates {
		for _, invoice := range invoices {
			quoted, err := money.QuoteUSDC(invoice, rate)
			if err != nil {
				t.Fatalf("QuoteUSDC(%v, %v): %v", invoice, rate, err)
			}
			got, err := Reconcile(Deposit{
				Received: quoted, Rate: rate,
				QuotedUSDC: quoted, QuotedNGN: money.RoundNGN(invoice),
				Manual: true,
			})
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if got.Underpaid {
				t.Errorf("paying the exact quote for ₦%v at %v was read as an underpayment",
					invoice, rate)
			}
			if want := money.RoundNGN(invoice); got.PayoutNGN < want {
				t.Errorf("₦%v invoice at %v paid out %v, short of the invoice",
					invoice, rate, got.PayoutNGN)
			}
		}
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
