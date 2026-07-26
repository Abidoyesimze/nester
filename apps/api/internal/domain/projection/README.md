# Monte Carlo savings projection (#843)

Replaces a single deterministic point projection with a simulated outcome
*distribution*: many independent randomized paths over the horizon, each
applying a random yield draw and a randomly skipped/varied contribution per
period, compounded month over month. The distribution's percentiles (and,
for a goal, the fraction of paths that hit the target in time) are reported
instead of one number pretending to be certain.

This document is the required explicit list of every distributional
assumption the model makes (an unexplained-parameter Monte Carlo is not
acceptable per the issue).

## What this package contains

- **`model.go`** — response/request domain types (`Band`, `GoalTarget`,
  `SensitivityCell`, `Assumptions`, `Result`). Plain structs, no logic.
- **`calculator.go`** — the pure simulation core: `SimParams`, `Draws`,
  `GenerateDraws`, `Simulate`, `SensitivityGrid`, `SeedFromInputs`. No I/O,
  fully unit-testable, deterministic given a seed.
- **`calculator_test.go`** — the four required correctness properties (see
  below).

Parameter *derivation* from real data (historical APY, deposit history) is
service-layer orchestration, not pure math — that lives in
`internal/service/projection_service.go`, not here.

## Yield distribution model

Each period's yield is drawn from `Normal(YieldMeanPerPeriod,
YieldStdDevPerPeriod)`, applied multiplicatively to the running balance
(`balance *= 1 + draw`), and floored at -50% per period (`minPeriodReturn`
in calculator.go) so a rare extreme tail draw can't produce a nonsensical
one-period wipeout — stablecoin vault yields don't behave like leveraged
instruments, so this bound is a safety rail, not a modeling choice that
meaningfully truncates the real distribution.

**Parameters are grounded in the vault's own historical realized APY**
(`apy_history` table / `performance.APYRecord`, read via
`performance.SnapshotRepository.ListAPY`):

- `YieldMeanPerPeriod` = mean of the vault's realized APY records (annual %,
  converted to a monthly fraction: `mean/100/12`).
- `YieldStdDevPerPeriod` = population standard deviation of those same
  realized APY records, annualized-to-monthly via `/sqrt(12)` (the standard
  scaling for i.i.d. period returns), then converted to a fraction.

This directly satisfies "a stable source shows a tight band, a volatile one
shows a wide band" — the band width is literally the vault's own APY
volatility, not a fixed constant.

**Fallback for a vault with too little APY history** (fewer than 3 realized
APY records — mirrors `forecast.minHistory` in `internal/forecast/`, which
uses the same threshold for the same reason: fewer points are too noisy to
trust): a documented conservative default of **5% mean annual APY, 3%
annual standard deviation**, matching typical stablecoin lending yield and
volatility on the vaults this product targets. This is a prior, not a
measurement, and `Assumptions.YieldSourceHistoryPoints` in the response
always reports how many real data points (if any) actually backed the
numbers, so a caller can tell a measured band from a defaulted one.

## Contribution (deposit) behavior model

Each period, the scheduled deposit is either skipped or made:

- `SkipUniform[path][period] < ContributionSkipProbability` → skipped
  (contributes $0 that period).
- Otherwise, the deposit is made, but its size varies:
  `amount = ContributionAmount * (1 + ContributionAmountCV * N(0,1))`,
  floored at 0. `ContributionAmountCV` is the coefficient of variation
  (stddev / mean) of the user's own historical deposit amounts.

**`ContributionSkipProbability` is derived from the user's own deposit
history** for the vault (`vault.VaultTransaction` rows with
`Type == "deposit"`, via `vault.Repository.ListUserVaultTransactions`):

1. Compute the user's median gap (in days) between consecutive deposits —
   this is their *actual* cadence, which need not match the cadence the
   planner input asks about.
2. Over the span from their first to most recent deposit, estimate how many
   deposits *would* have happened at that cadence
   (`expected = spanDays / medianGapDays`), and compare to how many actually
   happened (`actual = len(deposits) - 1`).
3. `skipProbability = clamp(1 - actual/expected, 0, 0.9)` — a simple
   skip-rate estimate. Clamped so a single missed period in a short history
   doesn't imply near-certain future skipping.

**New-user prior** (fewer than 3 recorded deposits — not enough to fit a
per-user cadence at all): `ContributionSkipProbability = 0.10` (90% chance
of making a scheduled deposit on time) and `ContributionAmountCV = 0.15`.
Rationale: this product's onboarding steers users toward automated/recurring
deposits (DCA-style), for which a ~90% on-time rate is a reasonable
industry-typical assumption for automated recurring transfers — neither
naively optimistic (100%) nor punitively pessimistic. `Assumptions.
NewUserPriorUsed` in the response flags whenever this prior (rather than a
measured per-user rate) was used, again so the caller can tell the two
apart.

## Simulation size

**3000 independent paths per projection** (`DefaultPaths` /
`ProjectionService` default), within the 2000-5000 range called out in the
issue. Percentile estimates from Monte Carlo sampling have standard error
roughly proportional to `sqrt(p(1-p)/n)`; at `n = 3000` the P10/P50/P90
estimates are stable to within roughly 1% of the outcome range run to run
for the seeds exercised in this package's tests — enough precision for a
planner UI showing rounded currency bands, while keeping the simulation (a
few thousand paths x a few hundred periods at most) comfortably sub-
millisecond in Go, so it can run inline in the request (see "Interactivity"
below).

## RNG seeding / determinism

`SeedFromInputs` hashes the caller-supplied string parts (canonical string
forms of: user id, vault id, goal target amount, goal deadline, deposit
amount, deposit cadence) with FNV-1a 64-bit and casts the sum to `int64`.
Identical inputs always hash to the identical seed, which
`GenerateDraws(seed, paths, periods)` turns into bit-identical random draws,
which `Simulate` turns into a bit-identical result. This is what makes the
same planner inputs always show the same band (required for the caching
window below and for reproducibility/tests), while any change to an input
(a different deposit amount, a different deadline) produces an
independent — but still fully deterministic — simulation.

## Caching

`ProjectionService` keeps a small in-process
`map[string]cacheEntry{Result, expiresAt}` guarded by a `sync.RWMutex`, TTL
5 minutes, keyed on the same canonical input string used to derive the
seed (following the existing in-repo pattern for this,
e.g. `internal/service/vault_analytics_service.go`'s `analyticsCache` and
`internal/oracle/cache.go`'s `RateCache` — there is no shared/generic cache
package in this repo to reuse instead). A 5-minute TTL is short enough that
a real change in the user's deposit history or the vault's APY is reflected
promptly, while still making repeated renders of the same planner state
(no dependency on a real cache invalidation signal) cheap.

## Interactivity / no new queue infrastructure

At 3000 paths x a bounded number of periods (capped — see
`ProjectionService.maxPeriods`), `Simulate` runs in low single-digit
milliseconds, so the common case (one goal's projection, or a full
sensitivity grid re-using one `Draws` matrix via common random numbers) runs
inline within the HTTP request handler. This repo has no durable job queue
today; #846 (tracked separately, handled in a parallel effort) is where real
queue infrastructure would land if a future use case needs it. Building one
here for a fast-enough inline computation would be premature infrastructure,
so it is intentionally out of scope for this issue.

## Sensitivity grid

`SensitivityGrid` re-evaluates the exact same `Draws` matrix (common random
numbers / CRN) across a grid of deposit amounts and deadlines — see the doc
comment on `SensitivityGrid` in calculator.go for the monotonicity argument
this construction guarantees: because every path replays identical
yield/skip/variance draws across the grid, a larger deposit can only ever
produce a balance greater than or equal to what a smaller deposit produced
on that same path. That is what the required
`TestSensitivityGrid_MonotonicInDeposit` test in calculator_test.go checks.
