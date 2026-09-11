package settle

import (
	"context"
	"log/slog"
	"time"

	"github.com/Rinku-Labs/linq-stellar/internal/store"
	"github.com/Rinku-Labs/linq-stellar/internal/wake"
	"gorm.io/gorm"
)

const (
	// defaultScanInterval is how often the sweep runs. Stellar ledgers close
	// roughly every 5 seconds, so sweeping faster than this mostly spends
	// Horizon quota.
	defaultScanInterval = 15 * time.Second
	// defaultBatch bounds one sweep so a single tick cannot spend unbounded
	// time on Horizon. Orders are taken deadline-first, so those closest to
	// expiring are never starved by a backlog.
	defaultBatch = 100
)

// Scanner polls deposit accounts for orders that are still waiting.
//
// It is the backstop behind the Horizon stream, and it is driven from order
// state in the database rather than from an in-flight queue message. That
// matters for restarts: anything still awaiting a deposit is simply picked up
// on the next sweep, with no delivery to lose and no watch to restart from the
// beginning.
type Scanner struct {
	DB       *gorm.DB
	Deposits *Deposits
	Log      *slog.Logger
	Interval time.Duration
	Batch    int

	// Bus ends a wait when an order starts waiting for a deposit, so the
	// backstop is looking at it from the first second rather than from the end
	// of an interval that widened while the service was quiet.
	Bus *wake.Bus
}

// Run sweeps until the context is cancelled.
func (s *Scanner) Run(ctx context.Context) {
	interval := s.Interval
	if interval <= 0 {
		interval = defaultScanInterval
	}

	wake.Loop{
		Name:     "deposit scanner",
		Topic:    wake.Deposits,
		Interval: interval,
		Bus:      s.Bus,
		Log:      s.Log,
		Work:     s.sweep,
	}.Run(ctx)
}

// sweep runs one pass. It recovers on its own so one bad cycle — a malformed
// row, a Horizon shape nothing expected — cannot take the scanner down and
// leave every waiting order unwatched.
func (s *Scanner) sweep(ctx context.Context) (found bool) {
	defer func() {
		if rec := recover(); rec != nil {
			s.Log.Error("deposit sweep panicked", "panic", rec)
		}
	}()

	batch := s.Batch
	if batch <= 0 {
		batch = defaultBatch
	}

	var orders []store.Order
	err := s.DB.
		Where("status = ?", store.StateAwaitingDeposit).
		Order("deposit_deadline ASC").
		Limit(batch).
		Find(&orders).Error
	if err != nil {
		s.Log.Error("deposit sweep query failed", "error", err)
		return false
	}

	for i := range orders {
		if ctx.Err() != nil {
			return len(orders) > 0
		}
		s.check(&orders[i])
	}
	return len(orders) > 0
}

// check does one balance read for a single order.
func (s *Scanner) check(order *store.Order) {
	balance, err := s.Deposits.Chain.USDCBalance(order.DepositAddress)
	if err != nil {
		// Horizon being unavailable is not evidence the deposit is absent, so
		// the order keeps its deadline and is retried next sweep.
		s.Log.Warn("could not read deposit balance",
			"order", order.ID, "address", order.DepositAddress, "error", err)
		return
	}

	if balance > 0 {
		// Attribution is best effort. The deposit is already confirmed by
		// balance, and failing the order because Horizon would not name the
		// sender would be worse than recording it without one.
		info, infoErr := s.Deposits.Chain.Deposit(order.DepositAddress)
		if infoErr != nil {
			s.Log.Warn("could not attribute deposit, continuing",
				"order", order.ID, "error", infoErr)
		}
		if _, err := s.Deposits.Record(order, balance, info); err != nil {
			s.Log.Error("could not record deposit", "order", order.ID, "error", err)
		}
		return
	}

	// Nothing yet. Give up only once the deadline has genuinely passed.
	if !order.DepositDeadline.IsZero() && time.Now().UTC().After(order.DepositDeadline) {
		if err := s.Deposits.Expire(order); err != nil {
			s.Log.Error("could not expire order", "order", order.ID, "error", err)
		}
	}
}
