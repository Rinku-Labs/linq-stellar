package settle

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Rinku-Labs/linq-stellar/internal/pace"
	"github.com/Rinku-Labs/linq-stellar/internal/stellar"
	"github.com/Rinku-Labs/linq-stellar/internal/store"
	"gorm.io/gorm"
)

const (
	// defaultSuperviseInterval is how often the supervisor looks for orders
	// that need a stream. Streams are started when an order begins waiting, so
	// this only has to be faster than a payer can realistically pay.
	defaultSuperviseInterval = 5 * time.Second
	// defaultMaxStreams bounds concurrent Horizon connections. Beyond this,
	// orders are still watched by the polling scanner — slower, but never
	// unwatched.
	defaultMaxStreams = 200

	streamRetryMin = 1 * time.Second
	streamRetryMax = 30 * time.Second
)

// Streamer keeps a Horizon payment stream open per waiting order.
//
// Horizon streams are per-endpoint, so watching N deposit accounts means N
// connections. That is why this is bounded and why the polling scanner stays:
// the stream is an optimisation for latency, and the sweep is the guarantee.
type Streamer struct {
	DB       *gorm.DB
	Deposits *Deposits
	Log      *slog.Logger
	Interval time.Duration
	Max      int

	idle   pace.Backoff
	mu     sync.Mutex
	active map[string]context.CancelFunc
}

// Run supervises streams until the context is cancelled.
func (s *Streamer) Run(ctx context.Context) {
	interval := s.Interval
	if interval <= 0 {
		interval = defaultSuperviseInterval
	}
	s.active = map[string]context.CancelFunc{}
	s.Log.Info("deposit streamer started", "interval", interval, "maxStreams", s.max())

	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		s.idle.Next(s.supervise(ctx), t, interval)
		select {
		case <-ctx.Done():
			s.stopAll()
			s.Log.Info("deposit streamer stopped")
			return
		case <-t.C:
		}
	}
}

func (s *Streamer) max() int {
	if s.Max > 0 {
		return s.Max
	}
	return defaultMaxStreams
}

// supervise starts streams for newly waiting orders and stops those that have
// moved on.
func (s *Streamer) supervise(ctx context.Context) (found bool) {
	defer func() {
		if rec := recover(); rec != nil {
			s.Log.Error("stream supervisor panicked", "panic", rec)
		}
	}()

	var orders []store.Order
	err := s.DB.
		Where("status = ?", store.StateAwaitingDeposit).
		Order("deposit_deadline ASC").
		Limit(s.max()).
		Find(&orders).Error
	if err != nil {
		s.Log.Error("stream supervisor query failed", "error", err)
		return false
	}

	wanted := make(map[string]struct{}, len(orders))
	for i := range orders {
		wanted[orders[i].ID] = struct{}{}
	}

	s.mu.Lock()
	// Stop streams for orders that are no longer waiting. Leaving them open
	// would leak a Horizon connection per settled order.
	for id, cancel := range s.active {
		if _, still := wanted[id]; !still {
			cancel()
			delete(s.active, id)
		}
	}
	s.mu.Unlock()

	for i := range orders {
		s.ensure(ctx, orders[i])
	}
	return len(orders) > 0
}

// ensure starts a stream for an order if one is not already running.
func (s *Streamer) ensure(ctx context.Context, order store.Order) {
	s.mu.Lock()
	if _, running := s.active[order.ID]; running {
		s.mu.Unlock()
		return
	}
	if len(s.active) >= s.max() {
		s.mu.Unlock()
		return // the scanner still covers it
	}
	streamCtx, cancel := context.WithCancel(ctx)
	s.active[order.ID] = cancel
	s.mu.Unlock()

	go s.watch(streamCtx, order)
}

// watch streams one account until its order stops waiting.
//
// Horizon closes long-lived connections routinely, so a dropped stream is
// expected rather than exceptional. Each reconnect resumes from the last cursor
// this service actually handled: resuming at "now" instead would silently skip
// every payment that landed while the connection was down.
func (s *Streamer) watch(ctx context.Context, order store.Order) {
	defer func() {
		if rec := recover(); rec != nil {
			s.Log.Error("deposit stream panicked", "order", order.ID, "panic", rec)
		}
		s.mu.Lock()
		delete(s.active, order.ID)
		s.mu.Unlock()
	}()

	streamID := "payments:" + order.DepositAddress
	backoff := streamRetryMin

	for ctx.Err() == nil {
		cursor, err := store.LoadCursor(s.DB, streamID)
		if err != nil {
			s.Log.Error("could not load stream cursor", "order", order.ID, "error", err)
			return
		}

		err = s.Deposits.Chain.StreamUSDCPayments(ctx, order.DepositAddress, cursor,
			func(ev stellar.PaymentEvent) error {
				return s.handle(order, ev, streamID)
			})

		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.Log.Warn("deposit stream dropped, reconnecting",
				"order", order.ID, "backoff", backoff, "error", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > streamRetryMax {
			backoff = streamRetryMax
		}
	}
}

// handle records a streamed payment and advances the cursor.
//
// The cursor is saved only after the deposit has been recorded. Saving it first
// would mean a crash in between skips the payment for good on reconnect — the
// payer's money arrives and nothing ever credits it.
func (s *Streamer) handle(order store.Order, ev stellar.PaymentEvent, streamID string) error {
	// Read the live balance rather than trusting the event amount alone: a payer
	// may send twice, and the second payment is what makes the total correct.
	balance, err := s.Deposits.Chain.USDCBalance(order.DepositAddress)
	if err != nil {
		return err
	}

	if _, err := s.Deposits.Record(&order, balance, stellar.DepositInfo{
		TxHash: ev.TxHash,
		From:   ev.From,
	}); err != nil {
		return err
	}

	if err := store.SaveCursor(s.DB, streamID, ev.Cursor); err != nil {
		// The deposit is recorded, so nothing is lost; the worst case is that
		// this event is seen again after a reconnect, and the claim makes that
		// harmless.
		s.Log.Warn("could not save stream cursor", "order", order.ID, "error", err)
	}
	return nil
}

func (s *Streamer) stopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, cancel := range s.active {
		cancel()
		delete(s.active, id)
	}
}
