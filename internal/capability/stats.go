package capability

import "math"

// z95 and z90 are the two-sided normal quantiles for 95% and 90% intervals.
const (
	z95 = 1.959963984540054
	z90 = 1.6448536269514722
)

// Wilson is the 95% Wilson score interval for k successes in n trials: unlike the normal
// interval it stays inside [0, 1] and does not collapse to a point at k = 0 or k = n.
func Wilson(k, n int) (lo, hi float64) {
	if n <= 0 {
		return 0, 1
	}
	p, nf := float64(k)/float64(n), float64(n)
	den := 1 + z95*z95/nf
	mid := (p + z95*z95/(2*nf)) / den
	half := z95 * math.Sqrt(p*(1-p)/nf+z95*z95/(4*nf*nf)) / den
	return math.Max(0, mid-half), math.Min(1, mid+half)
}

// DiffInterval is the 90% Agresti–Caffo interval for pA − pB: one success and one failure
// are added to each arm before the normal interval, which keeps it honest at small n and at
// pass rates of 0 or 1, where the plain interval would claim certainty it does not have.
func DiffInterval(kA, nA, kB, nB int) (lo, hi float64) {
	pA := float64(kA+1) / float64(nA+2)
	pB := float64(kB+1) / float64(nB+2)
	se := math.Sqrt(pA*(1-pA)/float64(nA+2) + pB*(1-pB)/float64(nB+2))
	d := pA - pB
	return d - z90*se, d + z90*se
}

// Verdict is how two pass rates compare at an equivalence margin.
type Verdict string

const (
	// Equivalent: the 90% interval of the difference lies inside (−δ, +δ), which is the
	// two one-sided tests (TOST) procedure at α = 0.05.
	Equivalent Verdict = "equivalent"
	// Better and Worse: the interval excludes zero and does not lie inside the margin.
	Better Verdict = "better"
	Worse  Verdict = "worse"
	// Inconclusive: neither; more items are needed to say anything.
	Inconclusive Verdict = "inconclusive"
)

// Compare says how A's pass rate stands against B's at margin delta (e.g. 0.05).
func Compare(kA, nA, kB, nB int, delta float64) Verdict {
	lo, hi := DiffInterval(kA, nA, kB, nB)
	switch {
	case lo > -delta && hi < delta:
		return Equivalent
	case lo > 0:
		return Better
	case hi < 0:
		return Worse
	}
	return Inconclusive
}
