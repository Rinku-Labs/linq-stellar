package store

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// The deposit-account pool.
//
// A Stellar address cannot receive USDC until it exists on-chain and holds a
// trustline, and putting it there means submitting a transaction and waiting
// for a ledger to close. That wait — five to eight seconds — used to sit inside
// POST /orders, which meant every payer watched a spinner for it before they
// were shown anything to pay.
//
// None of that work is specific to the order that waited for it. Any
// provisioned account would have done. So they are made in advance, in batches,
// and an order takes one: the eight seconds move off the checkout screen and
// onto a worker that nobody is waiting for.
//
// The pool is a queue of usable accounts, not a cache of addresses. An entry is
// only offered once — Claim is a compare-and-swap — because two orders sharing
// a deposit address would credit one payer's money to the other's merchant.

// ErrPoolEmpty means no ready account was available. The caller should
// provision one inline rather than fail: an empty pool is a capacity problem,
// and refusing a payer over it would be a worse answer than a slow one.
var ErrPoolEmpty = errors.New("store: no pooled deposit account available")

// claimAttempts bounds the compare-and-swap retry. Each miss means another
// process took the row between the read and the write, so a handful of attempts
// covers far more concurrency than this service will ever see; past that,
// provisioning inline is quicker than continuing to argue.
const claimAttempts = 5

// PoolAccount is a deposit account provisioned ahead of any order.
type PoolAccount struct {
	ID uint `gorm:"primaryKey"`
	// Address is the Stellar account (G...).
	Address string `gorm:"uniqueIndex;size:64"`
	// EncryptedSeed is its secret, sealed the same way an order's is.
	EncryptedSeed string
	// Ready means the account exists on-chain with its USDC trustline and may
	// be handed to a payer. A row is written before its transaction is
	// submitted and only flipped once that has landed, because an address
	// published before it exists bounces the payment it was given for.
	Ready bool `gorm:"index"`
	// ProvisionTxHash is the transaction that created the account and its
	// trustline. Informational: it is empty for an account recovered by the
	// keeper, which can confirm from Horizon that an account exists without
	// knowing which submission created it.
	ProvisionTxHash string
	CreatedAt       time.Time
	// ClaimedAt and OrderID record which order took this account, so an
	// address can be traced back to the batch it came from during
	// reconciliation.
	ClaimedAt *time.Time `gorm:"index"`
	OrderID   string     `gorm:"size:255"`
}

func (PoolAccount) TableName() string { return "stellar_deposit_pool" }

// ClaimPoolAccount takes one ready account out of the pool for an order.
//
// The claim is the UPDATE's own WHERE clause, so two callers racing for the
// last account cannot both win it: the loser's update matches no rows and it
// tries the next one.
func ClaimPoolAccount(db *gorm.DB, orderID string) (*PoolAccount, error) {
	for attempt := 0; attempt < claimAttempts; attempt++ {
		var candidate PoolAccount
		err := db.
			Where("claimed_at IS NULL").
			Where("ready = ?", true).
			Order("id ASC").
			First(&candidate).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrPoolEmpty
		}
		if err != nil {
			return nil, err
		}

		now := time.Now().UTC()
		res := db.Model(&PoolAccount{}).
			Where("id = ? AND claimed_at IS NULL", candidate.ID).
			Updates(map[string]any{"claimed_at": &now, "order_id": orderID})
		if res.Error != nil {
			return nil, res.Error
		}
		if res.RowsAffected == 1 {
			candidate.ClaimedAt = &now
			candidate.OrderID = orderID
			return &candidate, nil
		}
	}
	return nil, ErrPoolEmpty
}

// ReleasePoolAccount hands an account back after the order it was claimed for
// could not be saved.
//
// Without this, every failed create would strand a provisioned account — paid
// for, usable, and invisible to the pool for good.
func ReleasePoolAccount(db *gorm.DB, id uint) error {
	return db.Model(&PoolAccount{}).
		Where("id = ?", id).
		Updates(map[string]any{"claimed_at": nil, "order_id": ""}).Error
}

// ReservePoolAccounts writes accounts that are about to be provisioned.
//
// Written before the transaction is submitted, deliberately. The seeds are the
// only way to reach these accounts, and a crash between submitting and
// recording would otherwise strand their reserves with no record they ever
// existed. A row with no transaction hash is not claimable, so the cost of
// writing early is nothing.
func ReservePoolAccounts(db *gorm.DB, accounts []PoolAccount) error {
	if len(accounts) == 0 {
		return nil
	}
	return db.Create(&accounts).Error
}

// MarkPoolReady makes reserved accounts claimable.
func MarkPoolReady(db *gorm.DB, ids []uint, txHash string) error {
	if len(ids) == 0 {
		return nil
	}
	return db.Model(&PoolAccount{}).
		Where("id IN ?", ids).
		Updates(map[string]any{"ready": true, "provision_tx_hash": txHash}).Error
}

// PendingPoolAccounts returns accounts whose provisioning was never confirmed.
func PendingPoolAccounts(db *gorm.DB, limit int) ([]PoolAccount, error) {
	var accounts []PoolAccount
	err := db.
		Where("ready = ?", false).
		Where("claimed_at IS NULL").
		Order("id ASC").
		Limit(limit).
		Find(&accounts).Error
	return accounts, err
}

// DropPoolAccount removes a reserved row whose account was never created.
func DropPoolAccount(db *gorm.DB, id uint) error {
	return db.Delete(&PoolAccount{}, id).Error
}

// ReadyPoolAccounts counts the accounts an order could be given right now.
func ReadyPoolAccounts(db *gorm.DB) (int64, error) {
	var n int64
	err := db.Model(&PoolAccount{}).
		Where("claimed_at IS NULL").
		Where("ready = ?", true).
		Count(&n).Error
	return n, err
}
