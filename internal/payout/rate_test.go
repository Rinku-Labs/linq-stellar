package payout

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func rateServer(t *testing.T, status int, body string) *LinqAPI {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/b2b/rate" || r.Method != http.MethodGet {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-Internal-Secret") != "" {
			t.Error("the internal secret was sent to a public route")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	api, err := NewLinqAPI(srv.URL, "secret")
	if err != nil {
		t.Fatalf("new api: %v", err)
	}
	return api
}

// The body is the shape b2b_linq.GetB2BOfframpRate writes.
func TestOfframpRateReadsTheBackendsReply(t *testing.T) {
	api := rateServer(t, http.StatusOK, `{"coin":"USDSUI","currency":"NGN","rate":1523.5}`)
	rate, err := api.OfframpRate()
	if err != nil {
		t.Fatalf("rate: %v", err)
	}
	if rate != 1523.5 {
		t.Errorf("rate = %v, want 1523.5", rate)
	}
}

func TestOfframpRateRefusesAnythingItCannotPriceWith(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"zero":         {http.StatusOK, `{"rate":0}`},
		"negative":     {http.StatusOK, `{"rate":-1500}`},
		"missing":      {http.StatusOK, `{"currency":"NGN"}`},
		"a string":     {http.StatusOK, `{"rate":"1500"}`},
		"not json":     {http.StatusOK, `<html>`},
		"too large":    {http.StatusOK, `{"pad":"` + strings.Repeat("x", maxRateBody) + `","rate":1500}`},
		"unavailable":  {http.StatusServiceUnavailable, `{"message":"Unable to fetch current rate"}`},
		"a 2xx not ok": {http.StatusAccepted, `{"rate":1500}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if rate, err := rateServer(t, tc.status, tc.body).OfframpRate(); err == nil {
				t.Errorf("rate = %v, want an error", rate)
			}
		})
	}
}

func TestARateIsReusedOnlyUntilItAges(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	fetches := 0
	cache := &CachedRate{
		Fetch:  func() (float64, error) { fetches++; return 1500 + float64(fetches), nil },
		MaxAge: 30 * time.Second,
		Now:    func() time.Time { return now },
	}

	for i := 0; i < 3; i++ {
		if rate, err := cache.Rate(); err != nil || rate != 1501 {
			t.Fatalf("rate = %v, %v", rate, err)
		}
	}
	now = now.Add(30 * time.Second)
	if rate, err := cache.Rate(); err != nil || rate != 1502 {
		t.Errorf("after expiry: %v, %v, want 1502", rate, err)
	}
	if fetches != 2 {
		t.Errorf("fetches = %d, want 2", fetches)
	}
}

// Serving the old rate through an outage would price orders on a number nobody
// can vouch for.
func TestAnExpiredRateIsNotServedWhenTheFetchFails(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	down := false
	cache := &CachedRate{
		Fetch: func() (float64, error) {
			if down {
				return 0, errors.New("unreachable")
			}
			return 1500, nil
		},
		MaxAge: 30 * time.Second,
		Now:    func() time.Time { return now },
	}
	if _, err := cache.Rate(); err != nil {
		t.Fatalf("first rate: %v", err)
	}

	down = true
	now = now.Add(time.Minute)
	if rate, err := cache.Rate(); err == nil {
		t.Errorf("rate = %v during an outage, want an error", rate)
	}

	down = false
	if rate, err := cache.Rate(); err != nil || rate != 1500 {
		t.Errorf("after recovery: %v, %v", rate, err)
	}
}

func TestConcurrentMissesShareOneFetch(t *testing.T) {
	var fetches atomic.Int32
	release := make(chan struct{})
	cache := &CachedRate{
		Fetch: func() (float64, error) {
			fetches.Add(1)
			<-release
			return 1500, nil
		},
		MaxAge: time.Minute,
	}

	var started, done sync.WaitGroup
	for i := 0; i < 50; i++ {
		started.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			started.Done()
			if rate, err := cache.Rate(); err != nil || rate != 1500 {
				t.Errorf("rate = %v, %v", rate, err)
			}
		}()
	}
	started.Wait()
	time.Sleep(20 * time.Millisecond) // let them reach the fetch
	close(release)
	done.Wait()

	if got := fetches.Load(); got != 1 {
		t.Errorf("fetches = %d, want 1", got)
	}
}

func statusServer(t *testing.T, status int, body string) *LinqAPI {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/payout" || r.Method != http.MethodGet || r.URL.Query().Get("reference") != "ord_1" {
			t.Errorf("request = %s %s", r.Method, r.URL.String())
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	api, err := NewLinqAPI(srv.URL, "secret")
	if err != nil {
		t.Fatalf("new api: %v", err)
	}
	return api
}

// The bodies are the shapes b2b_linq.GetInternalPayout writes.
func TestStatusSaysWhenThereIsNoSuchPayout(t *testing.T) {
	_, err := statusServer(t, http.StatusNotFound, `{"message":"No payout for that reference"}`).Status("ord_1")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("404: %v", err)
	}
	_, err = statusServer(t, http.StatusBadGateway, `{"message":"upstream"}`).Status("ord_1")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("a failure to answer is not 'no such payout': %v", err)
	}
}

func TestStatusReadsTheAmountWhenSent(t *testing.T) {
	without, err := statusServer(t, http.StatusOK, `{"payoutId":"p1","status":"paid","provider":"nomba"}`).Status("ord_1")
	if err != nil || without.Status != StatusPaid || without.AmountNGN != nil || without.Reference != "p1" {
		t.Errorf("without an amount: %+v %v", without, err)
	}
	with, err := statusServer(t, http.StatusOK, `{"payoutId":"p1","status":"paid","provider":"nomba","amountNGN":10000.5}`).Status("ord_1")
	if err != nil || with.AmountNGN == nil || *with.AmountNGN != 10000.5 {
		t.Errorf("with an amount: %+v %v", with, err)
	}
}
