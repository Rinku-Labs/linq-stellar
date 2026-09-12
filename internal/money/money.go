// Package money holds the rounding rules for converting between NGN and USDC.
//
// It exists because "just divide" is where a merchant's money goes missing. A
// ₦100 invoice at ₦1364.21/USDC is 0.0733018... USDC, a figure with no exact
// decimal form. Every place that shortens it — a quote, a QR code, a screen —
// has to choose a direction, and choosing "nearest" or "down" takes the
// difference out of the merchant's payout. Rounding a quote to two decimals
// costs up to $0.005, which is 6.8% of a ₦100 order.
//
// So there is exactly one rule here, and it is directional: a quote rounds UP,
// everything else rounds to a representable unit. The payer is asked for a
// hair more than the invoice rather than a hair less, and the residual — never
// more than a ten-thousandth of a cent — settles to treasury instead of coming
// off what the merchant was promised.
package money

import (
	"fmt"
	"math"
)

const (
	// NetworkDecimals is Stellar's maximum asset precision. An amount with more
	// decimals than this is rejected by Horizon as malformed.
	NetworkDecimals = 7

	// QuoteDecimals is the precision a quote is published at.
	//
	// Two, rounded up. A payer is asked for a figure they can read and type —
	// 0.08 rather than 0.073302 — and because QuoteUSDC rounds up, that figure
	// is never less than what the invoice needs.
	//
	// The cost is borne by the payer, not the merchant: they send at most 0.01
	// USDC more than the strict conversion, roughly ₦14 at current rates, which
	// on a very small order is a noticeable share of it. That was a deliberate
	// trade for legibility. What must never come back is two decimals rounded
	// to *nearest*: that told a payer to send 0.07 against a 0.0733 invoice and
	// paid a merchant ₦95.49 for ₦100.
	//
	// Every surface quotes this same figure — the screen, the SEP-7 URI and the
	// Linq backend's own ceil(amountNGN/rate*100)/100 — so they agree exactly.
	QuoteDecimals = 2

	// NairaDecimals is kobo. Bank rails settle in kobo, so an NGN figure that
	// carries more precision than this is a number no payout can actually pay.
	NairaDecimals = 2

	// DustUSDC is the largest shortfall treated as an exact payment, worth
	// roughly ₦0.0014.
	//
	// Deliberately NOT one unit at quote precision, which is now 0.01. A
	// tolerance that wide would read a 0.07 deposit against a 0.08 quote as
	// paid in full — the underpayment this package exists to catch.
	//
	// It absorbs a wallet that dropped the last digit, and nothing more. A
	// wider tolerance would be Linq quietly covering the gap between what a
	// payer sent and what a merchant was promised, which is the failure this
	// package exists to stop.
	DustUSDC = 1e-6
)

// QuoteUSDC returns the USDC a payer must send to settle an NGN invoice.
//
// Rounded up, always. The payer sends at most one unit at QuoteDecimals more
// than the strict conversion, and the merchant is never short.
func QuoteUSDC(ngn, rate float64) (float64, error) {
	if !finite(ngn) || ngn <= 0 {
		return 0, fmt.Errorf("money: ngn amount must be positive, got %v", ngn)
	}
	if !finite(rate) || rate <= 0 {
		return 0, fmt.Errorf("money: rate must be positive, got %v", rate)
	}
	quoted := CeilUSDC(ngn / rate)
	if quoted <= 0 {
		// Only reachable for an invoice worth less than a millionth of a
		// dollar, where there is no positive amount to ask for.
		return 0, fmt.Errorf("money: %v NGN at %v is too small to quote", ngn, rate)
	}
	return quoted, nil
}

// QuoteNGN returns the NGN a given amount of USDC is worth, in whole kobo.
func QuoteNGN(usdc, rate float64) float64 {
	if !finite(usdc) || !finite(rate) {
		return 0
	}
	return RoundNGN(usdc * rate)
}

// CeilUSDC rounds up to QuoteDecimals. Used wherever the direction has to
// favour the merchant.
func CeilUSDC(v float64) float64 { return ceilTo(v, QuoteDecimals) }

// RoundUSDC rounds to the network's precision. Used for amounts that describe
// what already happened on-chain, where there is no direction to favour.
func RoundUSDC(v float64) float64 { return roundTo(v, NetworkDecimals) }

// RoundNGN rounds to kobo.
func RoundNGN(v float64) float64 { return roundTo(v, NairaDecimals) }

// Covers reports whether a deposit settles a quote.
//
// True when the deposit reaches the quote, or falls short of it by no more
// than DustUSDC. A zero or negative quote is an open-amount order with nothing
// to cover, and is reported as covered so callers do not have to special-case
// it.
func Covers(deposited, quoted float64) bool {
	if quoted <= 0 {
		return true
	}
	return deposited >= quoted-DustUSDC
}

// ceilTo rounds up to a number of decimal places.
//
// The snap-to-integer step is not decoration. 0.07 scaled by a million is
// 70000.00000000001 in float64, and a bare math.Ceil would turn an amount that
// is already exact into 0.070001 — rounding up a whole unit for a value that
// needed no rounding at all. Anything within a millionth of an integer at this
// scale is that integer; the float64 error at these magnitudes is ten thousand
// times smaller.
func ceilTo(v float64, decimals int) float64 {
	if !finite(v) {
		return v
	}
	scale := math.Pow(10, float64(decimals))
	scaled := v * scale
	if nearest := math.Round(scaled); math.Abs(scaled-nearest) < 1e-6 {
		scaled = nearest
	}
	return math.Ceil(scaled) / scale
}

func roundTo(v float64, decimals int) float64 {
	if !finite(v) {
		return v
	}
	scale := math.Pow(10, float64(decimals))
	return math.Round(v*scale) / scale
}

func finite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}
