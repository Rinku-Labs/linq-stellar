package store

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func poolDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:pool_%d?mode=memory&cache=shared", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func seedPool(t *testing.T, db *gorm.DB, n int, ready bool) {
	t.Helper()
	for i := 0; i < n; i++ {
		// Addresses are unique in the table, as they must be on the network.
		address := fmt.Sprintf("GPOOL%s%d", map[bool]string{true: "READY", false: "PENDING"}[ready], i)
		account := PoolAccount{
			Address:       address,
			EncryptedSeed: "seed-" + address,
			Ready:         ready,
		}
		if ready {
			account.ProvisionTxHash = "tx-" + address
		}
		if err := db.Create(&account).Error; err != nil {
			t.Fatalf("seed pool: %v", err)
		}
	}
}

// The one thing a pool of deposit addresses must never do. Two orders sharing
// an address means one payer's USDC settles the other's invoice, and no amount
// of care downstream can untangle it afterwards.
func TestAnAccountIsOnlyEverClaimedOnce(t *testing.T) {
	db := poolDB(t)
	seedPool(t, db, 10, true)

	const racers = 25
	var wg sync.WaitGroup
	var mu sync.Mutex
	claimed := map[string]string{} // address -> order

	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			orderID := fmt.Sprintf("order-%d", i)
			account, err := ClaimPoolAccount(db, orderID)
			if errors.Is(err, ErrPoolEmpty) {
				return
			}
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if previous, taken := claimed[account.Address]; taken {
				t.Errorf("%s was handed to both %s and %s", account.Address, previous, orderID)
			}
			claimed[account.Address] = orderID
		}(i)
	}
	wg.Wait()

	if len(claimed) == 0 {
		t.Fatal("nothing was claimed at all")
	}
	if len(claimed) > 10 {
		t.Fatalf("claimed %d accounts from a pool of 10", len(claimed))
	}
}

// An empty pool is a capacity problem, not a failure: the caller provisions
// inline instead. It has to be able to tell that apart from a broken database.
func TestEmptyPoolIsNamedRatherThanAnError(t *testing.T) {
	db := poolDB(t)
	if _, err := ClaimPoolAccount(db, "order-1"); !errors.Is(err, ErrPoolEmpty) {
		t.Fatalf("err = %v, want ErrPoolEmpty", err)
	}
}

// An account whose provisioning was never confirmed may not exist on-chain, and
// an address that does not exist bounces the payment it was published for.
func TestUnreadyAccountsAreNeverHandedOut(t *testing.T) {
	db := poolDB(t)
	seedPool(t, db, 3, false)

	if _, err := ClaimPoolAccount(db, "order-1"); !errors.Is(err, ErrPoolEmpty) {
		t.Fatalf("err = %v, want ErrPoolEmpty — unconfirmed accounts are not usable", err)
	}
}

// A create that fails after claiming must put the account back. Otherwise every
// failed order permanently strands one provisioned account, reserves and all.
func TestReleaseReturnsAnAccountToThePool(t *testing.T) {
	db := poolDB(t)
	seedPool(t, db, 1, true)

	first, err := ClaimPoolAccount(db, "order-1")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := ClaimPoolAccount(db, "order-2"); !errors.Is(err, ErrPoolEmpty) {
		t.Fatalf("the pool should be empty now, got %v", err)
	}

	if err := ReleasePoolAccount(db, first.ID); err != nil {
		t.Fatalf("release: %v", err)
	}

	again, err := ClaimPoolAccount(db, "order-2")
	if err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	if again.Address != first.Address {
		t.Errorf("got %s, want the released account %s", again.Address, first.Address)
	}
}

// The keeper decides how many to mint from this count, so it must not include
// accounts already spoken for or still unconfirmed.
func TestReadyCountsOnlyWhatCanBeHandedOut(t *testing.T) {
	db := poolDB(t)
	seedPool(t, db, 4, true)
	seedPool(t, db, 2, false)
	if _, err := ClaimPoolAccount(db, "order-1"); err != nil {
		t.Fatalf("claim: %v", err)
	}

	n, err := ReadyPoolAccounts(db)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Errorf("ready = %d, want 3 (4 provisioned, 1 taken, 2 unconfirmed)", n)
	}
}

// Reserved rows are written before the transaction is submitted, so the seeds
// survive a crash mid-provision. The keeper finds them by this query.
func TestPendingAccountsAreTheUnconfirmedOnes(t *testing.T) {
	db := poolDB(t)
	seedPool(t, db, 2, true)
	seedPool(t, db, 3, false)

	pending, err := PendingPoolAccounts(db, 10)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 3 {
		t.Fatalf("pending = %d, want 3", len(pending))
	}

	ids := []uint{pending[0].ID}
	if err := MarkPoolReady(db, ids, "tx-recovered"); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	after, err := PendingPoolAccounts(db, 10)
	if err != nil {
		t.Fatalf("pending after: %v", err)
	}
	if len(after) != 2 {
		t.Errorf("pending = %d after adopting one, want 2", len(after))
	}
}
