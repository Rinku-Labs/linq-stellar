package store

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Reconciliation is this service's books checked against the ledger: what the
// order table says happened, compared with what Horizon and the Linq backend
// say happened. It only reads and records. A mismatch is written down for a
// person to look at; nothing here moves money or changes an order.

// The results an item can have.
const (
	ReconcileOK       = "ok"
	ReconcileMismatch = "mismatch"
	// ReconcileError means the check could not be made, a Horizon outage say.
	// It is neither a pass nor a mismatch, and the check runs again next cycle.
	ReconcileError = "error"
)

var (
	ErrReconcileItemNotFound = errors.New("store: reconciliation item not found")
	// ErrNothingToResolve means the item passed: there is no mismatch to explain.
	ErrNothingToResolve = errors.New("store: reconciliation item passed and has nothing to resolve")
	// ErrAlreadyResolved means someone has already written the resolution.
	// Resolutions are kept as written rather than overwritten, so the note a
	// reviewer reads is the one that was given at the time.
	ErrAlreadyResolved = errors.New("store: reconciliation item is already resolved")
)

// ReconciliationCycle is one run of the reconciliation engine.
//
// The row is written when the cycle starts, so one that dies partway still
// shows when it started and what it had checked; FinishedAt stays empty and
// Error says why. Matched and Mismatches count this cycle's ok and mismatch
// items.
type ReconciliationCycle struct {
	ID         int64      `gorm:"primaryKey;autoIncrement" json:"id"`
	StartedAt  time.Time  `gorm:"not null;index" json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt"`
	// OrdersChecked is the number of orders with at least one item this cycle.
	OrdersChecked int `gorm:"not null;default:0" json:"ordersChecked"`
	// AccountsChecked is the Stellar accounts looked at: open and closed
	// deposit accounts, and every account the sponsor search returned.
	AccountsChecked int    `gorm:"not null;default:0" json:"accountsChecked"`
	Matched         int    `gorm:"not null;default:0" json:"matched"`
	Mismatches      int    `gorm:"not null;default:0" json:"mismatches"`
	Error           string `json:"error"`
}

func (ReconciliationCycle) TableName() string { return "reconciliation_cycles" }

// ReconciliationItem is one check on one order, or on one account with no
// order behind it, in one cycle.
type ReconciliationItem struct {
	ID      int64 `gorm:"primaryKey;autoIncrement" json:"id"`
	CycleID int64 `gorm:"not null;index" json:"cycleId"`
	// OrderID is empty for a sponsored account no order owns.
	OrderID   string `gorm:"size:255;index:idx_reconciliation_items_order_check,priority:1" json:"orderId"`
	CheckName string `gorm:"size:64;not null;index:idx_reconciliation_items_order_check,priority:2" json:"check"`
	Result    string `gorm:"size:16;not null;index;check:chk_reconciliation_items_result,result IN ('ok','mismatch','error')" json:"result"`
	// Expected and Actual are written for a person: what the books say, and
	// what the ledger or the Linq backend said. Never bank details.
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
	// Account and TxHash are the Stellar account and transaction the check was
	// about, kept apart from the text so a page can link them to an explorer.
	Account string `gorm:"size:64" json:"account,omitempty"`
	TxHash  string `gorm:"size:64" json:"txHash,omitempty"`

	Resolution string     `json:"resolution"`
	ResolvedBy string     `gorm:"size:100" json:"resolvedBy"`
	ResolvedAt *time.Time `json:"resolvedAt"`
}

func (ReconciliationItem) TableName() string { return "reconciliation_items" }

// StartCycle writes a cycle's row before it checks anything.
func StartCycle(db *gorm.DB, now time.Time) (ReconciliationCycle, error) {
	cycle := ReconciliationCycle{StartedAt: now.UTC()}
	err := db.Create(&cycle).Error
	return cycle, err
}

// FinishCycle records a cycle's counts, its error if it had one, and when it
// ended.
func FinishCycle(db *gorm.DB, cycle ReconciliationCycle, now time.Time) error {
	finished := now.UTC()
	return db.Model(&ReconciliationCycle{}).Where("id = ?", cycle.ID).Updates(map[string]any{
		"finished_at":      &finished,
		"orders_checked":   cycle.OrdersChecked,
		"accounts_checked": cycle.AccountsChecked,
		"matched":          cycle.Matched,
		"mismatches":       cycle.Mismatches,
		"error":            cycle.Error,
	}).Error
}

// AddReconciliationItem records one check's result.
func AddReconciliationItem(db *gorm.DB, item *ReconciliationItem) error {
	return db.Create(item).Error
}

// SettledChecks says which checks on these orders need not run again.
//
// A passed check has its answer: the ledger does not change a confirmed
// transaction, and the Linq backend does not change a payout it has finished.
// A resolved mismatch settles only the checks named in finalOnceResolved,
// whose answer cannot change either. Any other check keeps running after a
// resolution, so the resolution covers the answer it explained and no later
// one (see ExplainedAlready). A resolved error settles nothing: the check was
// never answered.
//
// The result maps order ID to the set of settled check names.
func SettledChecks(db *gorm.DB, orderIDs, finalOnceResolved []string) (map[string]map[string]bool, error) {
	settled := make(map[string]map[string]bool, len(orderIDs))
	if len(orderIDs) == 0 {
		return settled, nil
	}
	var rows []struct {
		OrderID   string
		CheckName string
	}
	q := db.Model(&ReconciliationItem{}).Distinct("order_id", "check_name")
	if len(finalOnceResolved) == 0 {
		q = q.Where("order_id IN ? AND result = ?", orderIDs, ReconcileOK)
	} else {
		q = q.Where("order_id IN ? AND (result = ? OR (result = ? AND resolved_at IS NOT NULL AND check_name IN ?))",
			orderIDs, ReconcileOK, ReconcileMismatch, finalOnceResolved)
	}
	if err := q.Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		if settled[row.OrderID] == nil {
			settled[row.OrderID] = map[string]bool{}
		}
		settled[row.OrderID][row.CheckName] = true
	}
	return settled, nil
}

// ExplainedAlready says whether a person has resolved this exact mismatch: the
// same check on the same order and account, with the same answer. A check that
// keeps running after a resolution records nothing while its answer stays the
// one explained, and a new mismatch as soon as the answer changes.
func ExplainedAlready(db *gorm.DB, item ReconciliationItem) (bool, error) {
	var n int64
	err := db.Model(&ReconciliationItem{}).
		Where("order_id = ? AND check_name = ? AND account = ? AND result = ? AND actual = ? AND resolved_at IS NOT NULL",
			item.OrderID, item.CheckName, item.Account, ReconcileMismatch, item.Actual).
		Limit(1).Count(&n).Error
	return n > 0, err
}

// ResolveReconciliationItem writes a person's explanation of a mismatch or an
// error. A passed item has nothing to explain, and an item already resolved
// keeps the note it was given.
//
// An unexplained mismatch is recorded again every cycle, so the note also goes
// on every other unresolved copy of the same problem: the same result and
// answer, for the same check, order and account. Copies resolved before keep
// their own note.
func ResolveReconciliationItem(db *gorm.DB, id int64, resolution, by string, now time.Time) (ReconciliationItem, error) {
	at := now.UTC()
	note := map[string]any{"resolution": resolution, "resolved_by": by, "resolved_at": &at}
	var item ReconciliationItem
	err := db.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&ReconciliationItem{}).
			Where("id = ? AND result <> ? AND resolved_at IS NULL", id, ReconcileOK).
			Updates(note)
		if res.Error != nil {
			return res.Error
		}
		if err := tx.First(&item, "id = ?", id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrReconcileItemNotFound
			}
			return err
		}
		if res.RowsAffected == 0 {
			if item.Result == ReconcileOK {
				return ErrNothingToResolve
			}
			return ErrAlreadyResolved
		}
		return tx.Model(&ReconciliationItem{}).
			Where("order_id = ? AND check_name = ? AND account = ? AND result = ? AND actual = ? AND resolved_at IS NULL",
				item.OrderID, item.CheckName, item.Account, item.Result, item.Actual).
			Updates(note).Error
	})
	return item, err
}

// KnownAccounts reports which of these addresses this service has a record
// of: a deposit account given to an order, or one in the pool waiting for an
// order. The pool's accounts are sponsored before any order exists, so leaving
// them out would report every ready account as a stranger.
func KnownAccounts(db *gorm.DB, addresses []string) (map[string]bool, error) {
	known := make(map[string]bool, len(addresses))
	if len(addresses) == 0 {
		return known, nil
	}
	var fromOrders, fromPool []string
	if err := db.Model(&Order{}).Where("deposit_address IN ?", addresses).
		Pluck("deposit_address", &fromOrders).Error; err != nil {
		return nil, err
	}
	if err := db.Model(&PoolAccount{}).Where("address IN ?", addresses).
		Pluck("address", &fromPool).Error; err != nil {
		return nil, err
	}
	for _, a := range append(fromOrders, fromPool...) {
		known[a] = true
	}
	return known, nil
}

// ResolvedAccounts reports which of these accounts already have a resolved
// item for an account-only check: someone has looked at them and written down
// why, and flagging them again every cycle would bury anything new.
func ResolvedAccounts(db *gorm.DB, check string, accounts []string) (map[string]bool, error) {
	resolved := make(map[string]bool, len(accounts))
	if len(accounts) == 0 {
		return resolved, nil
	}
	var found []string
	err := db.Model(&ReconciliationItem{}).
		Where("check_name = ? AND order_id = '' AND account IN ? AND resolved_at IS NOT NULL", check, accounts).
		Distinct().Pluck("account", &found).Error
	if err != nil {
		return nil, err
	}
	for _, a := range found {
		resolved[a] = true
	}
	return resolved, nil
}

// LatestCycles returns the most recent cycles, newest first.
func LatestCycles(db *gorm.DB, limit int) ([]ReconciliationCycle, error) {
	return ListCycles(db, 0, limit)
}

// ListCycles returns up to limit cycles older than before, newest first. A
// before of zero starts from the newest.
func ListCycles(db *gorm.DB, before int64, limit int) ([]ReconciliationCycle, error) {
	q := db.Order(clause.OrderByColumn{Column: clause.Column{Name: "id"}, Desc: true}).Limit(limit)
	if before > 0 {
		q = q.Where("id < ?", before)
	}
	var cycles []ReconciliationCycle
	err := q.Find(&cycles).Error
	return cycles, err
}

// ErrCycleNotFound means there is no reconciliation cycle with that ID.
var ErrCycleNotFound = errors.New("store: reconciliation cycle not found")

// GetCycle returns one cycle.
func GetCycle(db *gorm.DB, id int64) (ReconciliationCycle, error) {
	var cycle ReconciliationCycle
	err := db.First(&cycle, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return cycle, ErrCycleNotFound
	}
	return cycle, err
}

// CycleItems returns a cycle's items in the order they were checked, or only
// its mismatches.
func CycleItems(db *gorm.DB, cycleID int64, mismatchesOnly bool) ([]ReconciliationItem, error) {
	q := db.Where("cycle_id = ?", cycleID)
	if mismatchesOnly {
		q = q.Where("result = ?", ReconcileMismatch)
	}
	var items []ReconciliationItem
	err := q.Order("id").Find(&items).Error
	return items, err
}

// problemKey is what makes mismatch items the same problem: the same answer to
// the same check on the same order and account. An unexplained problem is
// recorded again every cycle, and a resolution goes on every copy (see
// ResolveReconciliationItem), so pages count and list problems, not copies.
var problemKey = []string{"order_id", "account", "check_name", "actual"}

// ListMismatches returns up to limit problems older than before, newest first:
// each once, as most recently recorded, which carries its resolution when it
// has one. A before of zero starts from the newest.
func ListMismatches(db *gorm.DB, before int64, limit int) ([]ReconciliationItem, error) {
	latest := db.Model(&ReconciliationItem{}).Select("MAX(id)").
		Where("result = ?", ReconcileMismatch).Group(strings.Join(problemKey, ", "))
	q := db.Where("id IN (?)", latest).
		Order(clause.OrderByColumn{Column: clause.Column{Name: "id"}, Desc: true}).Limit(limit)
	if before > 0 {
		q = q.Where("id < ?", before)
	}
	var items []ReconciliationItem
	err := q.Find(&items).Error
	return items, err
}

// MismatchCounts counts problems, not copies: every problem ever found, and
// those with no resolution on any copy.
func MismatchCounts(db *gorm.DB) (total, unresolved int64, err error) {
	problems := func(unresolvedOnly bool) *gorm.DB {
		q := db.Model(&ReconciliationItem{}).Distinct(problemKey).Where("result = ?", ReconcileMismatch)
		if unresolvedOnly {
			q = q.Where(`NOT EXISTS (SELECT 1 FROM reconciliation_items r WHERE r.result = ? AND r.resolved_at IS NOT NULL
				AND r.order_id = reconciliation_items.order_id AND r.account = reconciliation_items.account
				AND r.check_name = reconciliation_items.check_name AND r.actual = reconciliation_items.actual)`, ReconcileMismatch)
		}
		return q
	}
	if err = db.Table("(?) AS problems", problems(false)).Count(&total).Error; err != nil {
		return 0, 0, err
	}
	err = db.Table("(?) AS problems", problems(true)).Count(&unresolved).Error
	return total, unresolved, err
}

// CompletedCycles counts the cycles that ran to the end.
func CompletedCycles(db *gorm.DB) (int64, error) {
	var n int64
	err := db.Model(&ReconciliationCycle{}).Where("finished_at IS NOT NULL AND error = ''").Count(&n).Error
	return n, err
}
