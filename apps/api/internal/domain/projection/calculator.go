package projection

import (
	"hash/fnv"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"strings"
)

// minPeriodReturn floors a single period's yield draw at -50%. Real vault
// yields are near-never sharply negative (they are stablecoin lending/LP
// yields, not leveraged/volatile positions), but an unbounded normal draw
// could otherwise occasionally produce a nonsensical >100% single-period
// loss; this bound keeps pathological tail draws from producing negative
// balances while still allowing meaningfully wide bands for volatile
// sources.
const minPeriodReturn = -0.5

// SimParams are the pure inputs to a single Monte Carlo simulation run.
// Nothing in this struct or in Simulate performs I/O; every parameter must
// already be derived (by the service layer) from real data or a documented
// prior. See README.md for how each field is derived in production.
type SimParams struct {
	// StartingBalance is the vault balance at t=0.
	StartingBalance float64
	// ContributionAmount is the scheduled per-period deposit amount.
	ContributionAmount float64
	// ContributionSkipProbability is the modeled chance a scheduled deposit
	// is skipped in any given period (0..1).
	ContributionSkipProbability float64
	// ContributionAmountCV is the coefficient of variation (stddev/mean) of
	// the deposit amount when a deposit IS made — captures that users vary
	// deposit size even when they don't skip outright.
	ContributionAmountCV float64
	// YieldMeanPerPeriod and YieldStdDevPerPeriod parameterize a normal
	// distribution over each period's fractional yield (e.g. 0.004 = 0.4%
	// for a month at ~5% APY).
	YieldMeanPerPeriod   float64
	YieldStdDevPerPeriod float64
	// Periods is the number of periods (months) to simulate.
	Periods int
	// Paths is the number of independent simulated paths.
	Paths int
	// HasGoal / GoalTarget / DeadlinePeriod: when HasGoal is true, a path
	// "succeeds" if its running balance reaches GoalTarget at or before
	// period index DeadlinePeriod (0-based, < Periods).
	HasGoal        bool
	GoalTarget     float64
	DeadlinePeriod int
}

// Draws is a pre-generated matrix of random numbers, one row per path, one
// column per period. Separating draw generation from path evaluation is
// what makes the sensitivity grid (README.md) exact rather than
// approximately monotonic: every grid cell re-evaluates the SAME underlying
// random draws (common random numbers / CRN) and only varies the
// deterministic parameters (deposit amount, deadline), so a larger deposit
// can only ever produce a balance >= the balance under a smaller deposit,
// path by path.
type Draws struct {
	// Yield[path][period] is a standard-normal draw for that path/period's
	// yield shock.
	Yield [][]float64
	// SkipUniform[path][period] is a uniform(0,1) draw; a contribution is
	// skipped when this value is < ContributionSkipProbability.
	SkipUniform [][]float64
	// ContribVariance[path][period] is a standard-normal draw applied to the
	// contribution size (see ContributionAmountCV).
	ContribVariance [][]float64
}

// GenerateDraws builds a Draws matrix for the given number of paths and
// periods, seeded deterministically from seed. Same seed + paths + periods
// always produces bit-identical draws.
func GenerateDraws(seed int64, paths, periods int) Draws {
	rng := rand.New(rand.NewSource(seed))
	d := Draws{
		Yield:           make([][]float64, paths),
		SkipUniform:     make([][]float64, paths),
		ContribVariance: make([][]float64, paths),
	}
	for p := 0; p < paths; p++ {
		yieldRow := make([]float64, periods)
		skipRow := make([]float64, periods)
		contribRow := make([]float64, periods)
		for t := 0; t < periods; t++ {
			yieldRow[t] = rng.NormFloat64()
			skipRow[t] = rng.Float64()
			contribRow[t] = rng.NormFloat64()
		}
		d.Yield[p] = yieldRow
		d.SkipUniform[p] = skipRow
		d.ContribVariance[p] = contribRow
	}
	return d
}

// PathOutcome is the result of evaluating a single simulated path.
type PathOutcome struct {
	EndingBalance float64
	// ReachedGoalAtPeriod is the first period index (0-based) at which the
	// running balance reached GoalTarget, or -1 if it never did (or no goal
	// was configured).
	ReachedGoalAtPeriod int
}

// evaluatePath runs one deterministic path given pre-generated draws.
//
// Order of operations per period: yield is applied to the balance carried
// in from the previous period first (money already in the vault compounds
// for the whole period), then that period's contribution — if not skipped
// — is added at period end (a deposit made during the period is
// conservatively modeled as not yet having earned that period's yield).
func evaluatePath(p SimParams, yieldDraws, skipDraws, contribDraws []float64) PathOutcome {
	balance := p.StartingBalance
	reachedAt := -1
	if p.HasGoal && balance >= p.GoalTarget {
		reachedAt = 0
	}
	for t := 0; t < p.Periods; t++ {
		r := p.YieldMeanPerPeriod + p.YieldStdDevPerPeriod*yieldDraws[t]
		if r < minPeriodReturn {
			r = minPeriodReturn
		}
		balance *= 1 + r

		if skipDraws[t] >= p.ContributionSkipProbability {
			amount := p.ContributionAmount * (1 + p.ContributionAmountCV*contribDraws[t])
			if amount < 0 {
				amount = 0
			}
			balance += amount
		}

		if p.HasGoal && reachedAt == -1 && balance >= p.GoalTarget {
			reachedAt = t
		}
	}
	return PathOutcome{EndingBalance: balance, ReachedGoalAtPeriod: reachedAt}
}

// SimResult is the aggregated outcome distribution across all paths.
type SimResult struct {
	P10, P50, P90      float64
	SuccessProbability float64 // meaningful only when SimParams.HasGoal
	Outcomes           []PathOutcome
}

// Simulate runs SimParams.Paths independent paths using rows [0, Paths) of
// draws and returns the aggregated outcome distribution. Simulate is a pure
// function: identical params + draws always produce an identical result,
// and it performs no I/O.
func Simulate(p SimParams, draws Draws) SimResult {
	outcomes := make([]PathOutcome, p.Paths)
	endings := make([]float64, p.Paths)
	successes := 0
	for i := 0; i < p.Paths; i++ {
		o := evaluatePath(p, draws.Yield[i][:p.Periods], draws.SkipUniform[i][:p.Periods], draws.ContribVariance[i][:p.Periods])
		outcomes[i] = o
		endings[i] = o.EndingBalance
		if p.HasGoal && o.ReachedGoalAtPeriod != -1 && o.ReachedGoalAtPeriod <= p.DeadlinePeriod {
			successes++
		}
	}
	sort.Float64s(endings)

	result := SimResult{
		P10:      percentile(endings, 0.10),
		P50:      percentile(endings, 0.50),
		P90:      percentile(endings, 0.90),
		Outcomes: outcomes,
	}
	if p.HasGoal && p.Paths > 0 {
		result.SuccessProbability = float64(successes) / float64(p.Paths)
	}
	return result
}

// percentile returns the p-th percentile (0..1) of an already-sorted slice
// using linear interpolation between the two nearest ranks.
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	rank := p * float64(n-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return sorted[lo]
	}
	frac := rank - float64(lo)
	return sorted[lo] + frac*(sorted[hi]-sorted[lo])
}

// GridPoint is one input configuration in a sensitivity grid, paired with
// its resulting SimResult.
type GridPoint struct {
	ContributionAmount float64
	Periods            int
	DeadlinePeriod     int
	Result             SimResult
}

// SensitivityGrid evaluates base across every combination of
// depositAmounts x deadlinePeriods, reusing the SAME draws for every cell
// (common random numbers). depositAmounts and deadlinePeriods may each have
// length 1 to hold that dimension fixed. draws must have at least
// max(deadlinePeriods)+1 columns; the caller (service layer) is responsible
// for generating a Draws matrix wide enough for the whole grid.
//
// Monotonicity guarantee: for a fixed deadline, SuccessProbability and P50
// are non-decreasing as depositAmounts increases, because every cell
// replays identical yield/skip/variance draws — a larger contribution can
// only ever produce a balance >= the balance a smaller contribution would
// have produced on the same path, so a path that succeeds at a lower
// deposit also succeeds at a higher one.
func SensitivityGrid(base SimParams, draws Draws, depositAmounts []float64, deadlinePeriods []int) []GridPoint {
	points := make([]GridPoint, 0, len(depositAmounts)*len(deadlinePeriods))
	for _, deadline := range deadlinePeriods {
		periods := deadline + 1
		for _, deposit := range depositAmounts {
			p := base
			p.ContributionAmount = deposit
			p.Periods = periods
			p.DeadlinePeriod = deadline
			res := Simulate(p, draws)
			points = append(points, GridPoint{
				ContributionAmount: deposit,
				Periods:            periods,
				DeadlinePeriod:     deadline,
				Result:             res,
			})
		}
	}
	return points
}

// SeedFromInputs derives a deterministic int64 RNG seed from stable request
// inputs (user id, goal id, target amount, deadline, deposit amount/cadence,
// etc. — pass each as its canonical string form). Identical inputs always
// produce an identical seed, and therefore an identical simulated band,
// which is what makes results cacheable and reproducible within a caching
// window.
func SeedFromInputs(parts ...string) int64 {
	h := fnv.New64a()
	for i, part := range parts {
		if i > 0 {
			_, _ = h.Write([]byte{0})
		}
		_, _ = h.Write([]byte(part))
	}
	sum := h.Sum64()
	// fnv64a is unsigned; convert to a valid rand.Source seed (any int64 is
	// valid, but strconv round-tripping below keeps this obviously
	// deterministic and easy to reason about/debug).
	return int64(sum)
}

// FormatFloat is a small helper used by callers building the string parts
// passed to SeedFromInputs, so numeric inputs hash consistently regardless
// of how they were formatted upstream (e.g. decimal.Decimal.String()).
func FormatFloat(f float64) string {
	return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(f, 'f', 8, 64), "0"), ".")
}
