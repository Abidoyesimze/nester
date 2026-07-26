package projection

import (
	"math"
	"testing"
)

func baseParams() SimParams {
	return SimParams{
		StartingBalance:             1000,
		ContributionAmount:          100,
		ContributionSkipProbability: 0.1,
		ContributionAmountCV:        0.1,
		YieldMeanPerPeriod:          0.004,
		YieldStdDevPerPeriod:        0.002,
		Periods:                     12,
		Paths:                       3000,
	}
}

// 1. Percentile stability: the same seed must always produce the same
// P10/P50/P90 (determinism is what makes results cacheable/reproducible).
func TestSimulate_PercentileStabilityAcrossRuns(t *testing.T) {
	p := baseParams()
	seed := SeedFromInputs("user-1", "vault-1", "1000", "100", "30")

	draws1 := GenerateDraws(seed, p.Paths, p.Periods)
	res1 := Simulate(p, draws1)

	draws2 := GenerateDraws(seed, p.Paths, p.Periods)
	res2 := Simulate(p, draws2)

	if res1.P10 != res2.P10 || res1.P50 != res2.P50 || res1.P90 != res2.P90 {
		t.Fatalf("same seed produced different percentiles: run1={%v %v %v} run2={%v %v %v}",
			res1.P10, res1.P50, res1.P90, res2.P10, res2.P50, res2.P90)
	}

	// A different seed should (with overwhelming probability, given these
	// params) produce a different result -- guards against GenerateDraws
	// accidentally ignoring the seed.
	draws3 := GenerateDraws(seed+1, p.Paths, p.Periods)
	res3 := Simulate(p, draws3)
	if res3.P50 == res1.P50 {
		t.Fatalf("different seeds produced identical P50 (%v) -- seed may not be wired through", res1.P50)
	}
}

// 2. A zero-volatility input must collapse the band to the single
// deterministic compound-interest projection: P10 == P50 == P90, and that
// value must equal a hand-computed compounding of contributions and yield.
func TestSimulate_ZeroVolatilityCollapsesToDeterministicProjection(t *testing.T) {
	p := SimParams{
		StartingBalance:             1000,
		ContributionAmount:          100,
		ContributionSkipProbability: 0, // never skip
		ContributionAmountCV:        0, // no size variance
		YieldMeanPerPeriod:          0.005,
		YieldStdDevPerPeriod:        0, // zero volatility
		Periods:                     12,
		Paths:                       500,
	}
	draws := GenerateDraws(42, p.Paths, p.Periods)
	res := Simulate(p, draws)

	if res.P10 != res.P50 || res.P50 != res.P90 {
		t.Fatalf("zero-volatility band did not collapse: P10=%v P50=%v P90=%v", res.P10, res.P50, res.P90)
	}

	want := 1000.0
	for t := 0; t < 12; t++ {
		want *= 1.005
		want += 100
	}
	if math.Abs(res.P50-want) > 1e-6 {
		t.Fatalf("deterministic projection mismatch: got %v want %v", res.P50, want)
	}
}

// 3. Goal-success probability must match a hand-computed simple case.
//
// Construct a scenario where reaching the goal by the deadline requires
// EVERY period's contribution to land (no yield, no size variance, and the
// target set exactly to "all contributions made"). Then success requires
// zero skips across `periods` independent Bernoulli(skipProb) draws, so the
// analytically expected success probability is (1-skipProb)^periods.
func TestSimulate_GoalSuccessProbabilityMatchesHandComputedCase(t *testing.T) {
	const periods = 3
	const skipProb = 0.5
	const contribution = 100.0

	p := SimParams{
		StartingBalance:             0,
		ContributionAmount:          contribution,
		ContributionSkipProbability: skipProb,
		ContributionAmountCV:        0,
		YieldMeanPerPeriod:          0,
		YieldStdDevPerPeriod:        0,
		Periods:                     periods,
		Paths:                       20000, // large n to keep sampling error small
		HasGoal:                     true,
		GoalTarget:                  contribution * periods, // only reachable if ALL periods contribute
		DeadlinePeriod:              periods - 1,
	}
	draws := GenerateDraws(7, p.Paths, p.Periods)
	res := Simulate(p, draws)

	want := math.Pow(1-skipProb, periods) // = 0.125
	// Binomial standard error at n=20000, p=0.125: sqrt(0.125*0.875/20000) ~= 0.00234.
	// Allow a generous 6-sigma tolerance to keep this test non-flaky.
	tolerance := 0.02
	if math.Abs(res.SuccessProbability-want) > tolerance {
		t.Fatalf("success probability %.4f not within %.4f of hand-computed %.4f", res.SuccessProbability, tolerance, want)
	}
}

// Zero-skip-probability sanity check: with no volatility and no skipping,
// reaching an achievable goal should succeed on every path (probability 1),
// and an unreachable goal should never succeed (probability 0).
func TestSimulate_GoalSuccessProbabilityDeterministicEdgeCases(t *testing.T) {
	base := SimParams{
		StartingBalance:             0,
		ContributionAmount:          100,
		ContributionSkipProbability: 0,
		ContributionAmountCV:        0,
		YieldMeanPerPeriod:          0,
		YieldStdDevPerPeriod:        0,
		Periods:                     10,
		Paths:                       200,
		HasGoal:                     true,
		DeadlinePeriod:              9,
	}

	achievable := base
	achievable.GoalTarget = 500 // reached at period 4 (5 * 100)
	draws := GenerateDraws(1, achievable.Paths, achievable.Periods)
	res := Simulate(achievable, draws)
	if res.SuccessProbability != 1.0 {
		t.Fatalf("expected certain success, got %v", res.SuccessProbability)
	}

	unreachable := base
	unreachable.GoalTarget = 100000 // never reached in 10 periods of 100
	res2 := Simulate(unreachable, draws)
	if res2.SuccessProbability != 0.0 {
		t.Fatalf("expected impossible success, got %v", res2.SuccessProbability)
	}
}

// 4. Sensitivity grid monotonicity: for a fixed deadline, increasing the
// deposit amount must never lower the success probability. This is
// guaranteed by construction (common random numbers across grid cells,
// see calculator.go doc comment on SensitivityGrid) -- this test is the
// required correctness check for that guarantee.
func TestSensitivityGrid_MonotonicInDeposit(t *testing.T) {
	p := SimParams{
		StartingBalance:             500,
		ContributionSkipProbability: 0.3,
		ContributionAmountCV:        0.2,
		YieldMeanPerPeriod:          0.004,
		YieldStdDevPerPeriod:        0.01,
		Paths:                       4000,
		HasGoal:                     true,
		GoalTarget:                  3000,
	}
	deposits := []float64{50, 75, 100, 150, 200, 300, 500}
	deadlines := []int{11, 23} // 12 and 24 months

	maxPeriods := 24
	draws := GenerateDraws(SeedFromInputs("user-x", "vault-y", "3000", "500", "30"), p.Paths, maxPeriods)

	points := SensitivityGrid(p, draws, deposits, deadlines)

	// Group by deadline and assert non-decreasing success probability as
	// deposit increases.
	byDeadline := map[int][]GridPoint{}
	for _, pt := range points {
		byDeadline[pt.DeadlinePeriod] = append(byDeadline[pt.DeadlinePeriod], pt)
	}

	for deadline, cells := range byDeadline {
		for i := 1; i < len(cells); i++ {
			prev, cur := cells[i-1], cells[i]
			if cur.Result.SuccessProbability < prev.Result.SuccessProbability-1e-12 {
				t.Fatalf("deadline=%d: success probability decreased when deposit rose from %v (%v) to %v (%v)",
					deadline, prev.ContributionAmount, prev.Result.SuccessProbability, cur.ContributionAmount, cur.Result.SuccessProbability)
			}
			if cur.Result.P50 < prev.Result.P50-1e-9 {
				t.Fatalf("deadline=%d: median ending balance decreased when deposit rose from %v (%v) to %v (%v)",
					deadline, prev.ContributionAmount, prev.Result.P50, cur.ContributionAmount, cur.Result.P50)
			}
		}
	}

	// Sanity: the grid should actually show meaningful lift, not just a flat
	// line (otherwise the monotonicity check above would be vacuous).
	cells := byDeadline[11]
	first, last := cells[0], cells[len(cells)-1]
	if last.Result.SuccessProbability <= first.Result.SuccessProbability {
		t.Fatalf("expected success probability to meaningfully increase across the deposit grid, got %v -> %v",
			first.Result.SuccessProbability, last.Result.SuccessProbability)
	}
}

func TestPercentile_Basic(t *testing.T) {
	sorted := []float64{10, 20, 30, 40, 50}
	if got := percentile(sorted, 0.5); got != 30 {
		t.Fatalf("median = %v, want 30", got)
	}
	if got := percentile(sorted, 0); got != 10 {
		t.Fatalf("p0 = %v, want 10", got)
	}
	if got := percentile(sorted, 1); got != 50 {
		t.Fatalf("p100 = %v, want 50", got)
	}
}

func TestSeedFromInputs_Deterministic(t *testing.T) {
	a := SeedFromInputs("u1", "v1", "1000", "2027-01-01")
	b := SeedFromInputs("u1", "v1", "1000", "2027-01-01")
	if a != b {
		t.Fatalf("same inputs produced different seeds: %v vs %v", a, b)
	}
	c := SeedFromInputs("u2", "v1", "1000", "2027-01-01")
	if a == c {
		t.Fatalf("different inputs produced the same seed")
	}
}
