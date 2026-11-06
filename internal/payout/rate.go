package payout

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// RateMaxAge is how long a fetched rate may price orders. Short against how far
// the rate moves, long enough that a burst of orders costs one provider call.
const RateMaxAge = 30 * time.Second

const (
	// rateTimeout covers the Linq backend trying its first rate provider and
	// falling back to its second.
	rateTimeout = 15 * time.Second
	// maxRateBody is far above the three fields the reply carries.
	maxRateBody = 4 << 10
)

// OfframpRate fetches the NGN per USDC the Linq backend is quoting now, from the
// same GET /b2b/rate its own off-ramp and the Linq-B2B checkout use, so a Stellar
// order is priced the same as every other chain. The route is public, so the
// internal secret is not sent to it.
//
// Anything but a positive finite rate is an error. The rate is what turns a
// deposit into naira, so a reply this cannot read must stop the order rather
// than price it.
func (l *LinqAPI) OfframpRate() (float64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), rateTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.BaseURL+"/b2b/rate", nil)
	if err != nil {
		return 0, fmt.Errorf("payout: build rate request: %w", err)
	}
	res, err := l.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("payout: fetch rate: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("payout: rate returned %d", res.StatusCode)
	}
	var body struct {
		Rate float64 `json:"rate"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, maxRateBody)).Decode(&body); err != nil {
		return 0, fmt.Errorf("payout: decode rate: %w", err)
	}
	if body.Rate <= 0 || math.IsInf(body.Rate, 0) || math.IsNaN(body.Rate) {
		return 0, fmt.Errorf("payout: unusable rate %v", body.Rate)
	}
	return body.Rate, nil
}

// CachedRate holds a rate for a short while, because the Linq backend asks its
// provider afresh on every call and order bursts must not become provider
// bursts. Concurrent misses share one fetch.
//
// A rate past its age is never served, even when the fetch that would replace
// it fails: an order refused is retried, and a stale rate is paid.
type CachedRate struct {
	// Fetch gets the current rate.
	Fetch func() (float64, error)
	// MaxAge is how long a fetched rate may be used.
	MaxAge time.Duration
	// Now is the clock. Nil means time.Now.
	Now func() time.Time

	mu      sync.Mutex
	rate    float64
	fetched time.Time
	flight  singleflight.Group
}

// Rate returns a rate no older than MaxAge.
func (c *CachedRate) Rate() (float64, error) {
	if rate, ok := c.fresh(); ok {
		return rate, nil
	}
	rate, err, _ := c.flight.Do("rate", func() (any, error) {
		// A caller that missed just as another fetch landed has its answer.
		if rate, ok := c.fresh(); ok {
			return rate, nil
		}
		// Aged from before the fetch, so the wait never adds to a rate's life.
		started := c.now()
		rate, err := c.Fetch()
		if err != nil {
			return 0.0, err
		}
		c.mu.Lock()
		c.rate, c.fetched = rate, started
		c.mu.Unlock()
		return rate, nil
	})
	if err != nil {
		return 0, err
	}
	return rate.(float64), nil
}

func (c *CachedRate) fresh() (float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rate > 0 && c.now().Sub(c.fetched) < c.MaxAge {
		return c.rate, true
	}
	return 0, false
}

func (c *CachedRate) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}
