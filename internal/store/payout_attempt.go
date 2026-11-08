package store

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// Payout attempts.
//
// Each try at paying an order's naira is one row, written before the request
// goes out. The reference on that row is the idempotency key the payout
// provider sees, so the rows are the record of every reference that could
// have moved money for an order, and in what order.

// Attempt outcomes.
const (
	// PayoutPending means the attempt was started and its outcome is not known.
	// Its reference may have reached the provider.
	PayoutPending = "pending"
	// PayoutPaid means the provider confirmed the naira reached the merchant.
	PayoutPaid = "paid"
	// PayoutFailed means the provider confirmed the attempt failed.
	PayoutFailed = "failed"
)

// Induced failure kinds, for demonstrating the failure paths on Linq's own
// test orders. See InducePayoutFailure.
const (
	// InducedTransient fails the first attempt; later attempts run for real.
	InducedTransient = "transient"
	// InducedTerminal fails every attempt, so the order is refunded.
	InducedTerminal = "terminal"
)

// InducedReason is recorded on every attempt failed on purpose.
const InducedReason = "induced for demonstration"

var (
	ErrOrderNotFound           = errors.New("store: order not found")
	ErrFailureInjectionOff     = errors.New("store: failure injection is not allowed for this order's integrator")
	ErrFailureInjectionTooLate = errors.New("store: the order's payout has started, or a failure is already induced")
	ErrUnknownInducedKind      = errors.New("store: kind must be transient or terminal")
)

// PayoutAttempt is one try at paying an order.
type PayoutAttempt struct {
	ID      uint   `gorm:"primaryKey" json:"-"`
	OrderID string `gorm:"not null;size:255;uniqueIndex:idx_payout_attempts_order_number,priority:1" json:"-"`
	// Number counts from 1.
	Number int `gorm:"not null;uniqueIndex:idx_payout_attempts_order_number,priority:2" json:"number"`
	// Reference is the idempotency key sent to the payout provider. For an
	// induced attempt it is the reference the attempt would have used; it was
	// never sent.
	Reference string `gorm:"not null;index" json:"reference"`
	Outcome   string `gorm:"not null;default:pending" json:"outcome"`
	// RetrySafe is set on a failed attempt when the provider confirmed no money
	// moved, so a new reference cannot pay the merchant twice.
	RetrySafe bool `gorm:"not null;default:false" json:"-"`
	// Induced marks an attempt failed on purpose. It never reached the provider.
	Induced bool `gorm:"not null;default:false" json:"induced"`
	// Reason is the provider's failure reason. It can quote internal details,
	// so it is not served to integrators.
	Reason    string    `json:"-"`
	CreatedAt time.Time `json:"startedAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (PayoutAttempt) TableName() string { return "stellar_payout_attempts" }

// PayoutReference is the reference attempt n of an order sends.
//
// The first attempt uses the order ID, as every payout did before attempts
// were recorded. A later attempt gets its own reference only when newRefs is
// set; otherwise it reuses the order ID, and the provider, which is idempotent
// on the reference, returns the first attempt's result instead of paying.
func PayoutReference(orderID string, n int, newRefs bool) string {
	if n <= 1 || !newRefs {
		return orderID
	}
	return fmt.Sprintf("%s-p%d", orderID, n)
}

// LatestPayoutAttempt returns an order's most recent attempt, or nil if it has
// none.
func LatestPayoutAttempt(db *gorm.DB, orderID string) (*PayoutAttempt, error) {
	var attempt PayoutAttempt
	err := db.Where("order_id = ?", orderID).Order("number DESC").First(&attempt).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &attempt, nil
}

// PayoutAttempts returns an order's attempts, first to last.
func PayoutAttempts(db *gorm.DB, orderID string) ([]PayoutAttempt, error) {
	var attempts []PayoutAttempt
	err := db.Where("order_id = ?", orderID).Order("number ASC").Find(&attempts).Error
	return attempts, err
}

// StartPayoutAttempt records attempt n before its request is sent, so the
// reference is on record even if the process dies mid-request.
func StartPayoutAttempt(db *gorm.DB, orderID string, n int, reference string) (PayoutAttempt, error) {
	attempt := PayoutAttempt{OrderID: orderID, Number: n, Reference: reference, Outcome: PayoutPending}
	return attempt, db.Create(&attempt).Error
}

// RecordInducedFailure records attempt n as failed on purpose. Nothing is sent
// to the provider, so the failure is safe to retry.
func RecordInducedFailure(db *gorm.DB, orderID string, n int, reference string) (PayoutAttempt, error) {
	attempt := PayoutAttempt{
		OrderID: orderID, Number: n, Reference: reference,
		Outcome: PayoutFailed, RetrySafe: true, Induced: true, Reason: InducedReason,
	}
	return attempt, db.Create(&attempt).Error
}

// ResolvePayoutAttempt records a pending attempt's outcome. An attempt that
// already has one keeps it.
func ResolvePayoutAttempt(db *gorm.DB, id uint, outcome string, retrySafe bool, reason string) error {
	return db.Model(&PayoutAttempt{}).
		Where("id = ? AND outcome = ?", id, PayoutPending).
		Updates(map[string]any{
			"outcome":    outcome,
			"retry_safe": retrySafe,
			"reason":     reason,
			"updated_at": time.Now().UTC(),
		}).Error
}

// inducibleStates are the states an order can be in before its payout starts.
var inducibleStates = []string{
	StateInitiated, StateAwaitingDeposit, StateDepositDetected, StatePayoutQueued,
}

// InducePayoutFailure marks an order so the payout worker fails its payout on
// purpose, without calling the provider. It is for demonstrating the failure
// paths on Linq's own test orders.
//
// It is refused unless the order's integrator has AllowFailureInjection, which
// has no API route: it is set in the database, on Linq's own test integrators
// only. It is also refused once the order has any payout attempt, so it never
// changes a payout already under way, and it can be set only once.
func InducePayoutFailure(db *gorm.DB, orderID, kind string) error {
	if kind != InducedTransient && kind != InducedTerminal {
		return ErrUnknownInducedKind
	}
	return db.Transaction(func(tx *gorm.DB) error {
		var order Order
		err := tx.Select("id", "integrator_id").First(&order, "id = ?", orderID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrOrderNotFound
		}
		if err != nil {
			return err
		}

		var integrator Integrator
		err = tx.Select("id", "allow_failure_injection").First(&integrator, "id = ?", order.IntegratorID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrFailureInjectionOff
		}
		if err != nil {
			return err
		}
		if !integrator.AllowFailureInjection {
			return ErrFailureInjectionOff
		}

		result := tx.Model(&Order{}).
			// payout_attempts covers orders paid before attempts were recorded.
			Where("id = ? AND induced_failure = '' AND payout_attempts = 0 AND status IN ?", orderID, inducibleStates).
			Where("NOT EXISTS (SELECT 1 FROM stellar_payout_attempts WHERE order_id = ?)", orderID).
			Updates(map[string]any{"induced_failure": kind, "updated_at": time.Now().UTC()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrFailureInjectionTooLate
		}
		return nil
	})
}

// FailureInjectionAllowed says whether an integrator may have its orders' payouts
// fail on purpose. An integrator with no row may not.
func FailureInjectionAllowed(db *gorm.DB, integratorID string) (bool, error) {
	var allowed []bool
	err := db.Model(&Integrator{}).Where("id = ?", integratorID).Pluck("allow_failure_injection", &allowed).Error
	return len(allowed) == 1 && allowed[0], err
}

// InducedFailureOf reads an order's induced failure kind fresh from the
// database. The worker calls it after claiming the order, so a failure induced
// after the order was listed but before the claim is still honoured.
func InducedFailureOf(db *gorm.DB, orderID string) (string, error) {
	var kinds []string
	if err := db.Model(&Order{}).Where("id = ?", orderID).Pluck("induced_failure", &kinds).Error; err != nil {
		return "", err
	}
	if len(kinds) == 0 {
		return "", ErrOrderNotFound
	}
	return kinds[0], nil
}
