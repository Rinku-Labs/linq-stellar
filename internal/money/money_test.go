package money

import (
	"math"
	"testing"
)

// The bug this package was written for: a ₦100 invoice quoted at two decimals
// becomes 0.07 USDC, and the merchant is paid ₦95.49 for a ₦100 order.
func TestQuoteCoversTheInvoice(t *testing.T) {
	cases := []struct {
		name string
		ngn  float64
		rate float64
	}{
		{"the reported order", 100, 1364.21},
		{"the second reported order", 150, 1362.55},
		{"a rate that divides evenly", 1000, 1000},
		{"a small invoice", 1, 1655},
		{"a large invoice", 5_000_000, 1364.21},
		{"a rate with long decimals", 100, 1364.2137891},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			quoted, err := QuoteUSDC(tc.ngn, tc.rate)
			if err != nil {
				t.Fatalf("QuoteUSDC(%v, %v): %v", tc.ngn, tc.rate, err)
			}

			// The whole point: converting the quote back must never land below
			// the invoice.
			if back := quoted * tc.rate; back < tc.ngn {
				t.Errorf("quote %v USDC is worth %v NGN, short of the %v invoice",
					quoted, back, tc.ngn)
			}

			// And it must not overshoot by more than the rounding it needed —
			// one unit at quote precision, converted to naira.
			overshoot := quoted*tc.rate - tc.ngn
			if limit := tc.rate / math.Pow(10, QuoteDecimals); overshoot > limit {
				t.Errorf("quote overshoots by %v NGN, want at most %v", overshoot, limit)
			}

			if quoted != CeilUSDC(quoted) {
				t.Errorf("quote %v carries more than %d decimals", quoted, QuoteDecimals)
			}
		})
	}
}

// The specific figure from the incident, spelled out so a regression is
// obvious rather than arithmetic.
func TestQuoteForTheReportedOrder(t *testing.T) {
	quoted, err := QuoteUSDC(100, 1364.21)
	if err != nil {
		t.Fatalf("QuoteUSDC: %v", err)
	}
	if want := 0.073303; quoted != want {
		t.Fatalf("quoted %v USDC, want %v", quoted, want)
	}
	// What the old two-decimal quote paid out, for contrast.
	if paid := QuoteNGN(0.07, 1364.21); paid >= 100 {
		t.Fatalf("0.07 USDC pays %v NGN; the test's premise is wrong", paid)
	}
	if paid := QuoteNGN(quoted, 1364.21); paid < 100 {
		t.Fatalf("quote pays %v NGN, short of the 100 invoice", paid)
	}
}

// An amount already exact at quote precision must survive CeilUSDC unchanged.
// Float64 scaling makes this less obvious than it looks: 0.07 * 1e6 is
// 70000.00000000001, which a bare Ceil turns into 0.070001.
func TestCeilLeavesExactAmountsAlone(t *testing.T) {
	for _, v := range []float64{0, 0.07, 0.1, 0.11, 1, 1.5, 0.073303, 12.345678, 1000000} {
		if got := CeilUSDC(v); got != v {
			t.Errorf("CeilUSDC(%v) = %v, want it unchanged", v, got)
		}
	}
}

func TestCeilRoundsUp(t *testing.T) {
	cases := []struct{ in, want float64 }{
		{0.0733018, 0.073302},
		{0.0000001, 0.000001},
		{1.0000001, 1.000001},
		{0.07330180000001, 0.073302},
	}
	for _, tc := range cases {
		if got := CeilUSDC(tc.in); got != tc.want {
			t.Errorf("CeilUSDC(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestRoundNGNIsKobo(t *testing.T) {
	cases := []struct{ in, want float64 }{
		{95.49470000000001, 95.49},
		{149.88049999999998, 149.88},
		{100, 100},
		{0.005, 0.01},
		{0.004, 0},
	}
	for _, tc := range cases {
		if got := RoundNGN(tc.in); got != tc.want {
			t.Errorf("RoundNGN(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestQuoteRejectsUnusableInputs(t *testing.T) {
	cases := []struct {
		name      string
		ngn, rate float64
	}{
		{"zero amount", 0, 1655},
		{"negative amount", -1, 1655},
		{"zero rate", 100, 0},
		{"negative rate", 100, -1655},
		{"NaN rate", 100, math.NaN()},
		{"infinite rate", 100, math.Inf(1)},
		{"NaN amount", math.NaN(), 1655},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := QuoteUSDC(tc.ngn, tc.rate); err == nil {
				t.Errorf("QuoteUSDC(%v, %v) returned no error", tc.ngn, tc.rate)
			}
		})
	}
}

func TestCovers(t *testing.T) {
	cases := []struct {
		name              string
		deposited, quoted float64
		want              bool
	}{
		{"exact", 0.073303, 0.073303, true},
		{"over", 1, 0.073303, true},
		{"one dust unit short", 0.073302, 0.073303, true},
		{"the two-decimal underpayment", 0.07, 0.073303, false},
		{"nothing sent", 0, 0.073303, false},
		{"open-amount order", 0.07, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Covers(tc.deposited, tc.quoted); got != tc.want {
				t.Errorf("Covers(%v, %v) = %v, want %v",
					tc.deposited, tc.quoted, got, tc.want)
			}
		})
	}
}
