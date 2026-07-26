// Package service: ProjectionService turns a planner request (deposit
// amount/cadence, optional goal) into a Monte Carlo savings projection
// (issue #843). It is the orchestration layer around the pure simulation
// core in internal/domain/projection: it resolves the vault's real
// historical APY volatility and the user's real deposit-history reliability
// into concrete SimParams, derives a deterministic seed, checks/populates a
// short-lived in-process cache, and runs the simulation (plus, for a goal,
// a deposit/deadline sensitivity grid) inline within the request.
//
// See internal/domain/projection/README.md for the full list of documented
// distributional assumptions this file's derivation logic implements.
package service

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/suncrestlabs/nester/apps/api/internal/domain/performance"
	"github.com/suncrestlabs/nester/apps/api/internal/domain/projection"
	"github.com/suncrestlabs/nester/apps/api/internal/domain/vault"
)

// defaultSimulationPaths is the number of independent Monte Carlo paths run
// per projection. See README.md ("Simulation size") for why 3000 (within
// the 2000-5000 range called out in the issue) is enough for stable
// percentiles while staying fast enough to run inline in the request.
const defaultSimulationPaths = 3000

// maxProjectionPeriods bounds the simulated horizon (in months) so a
// pathological input (e.g. a goal deadline decades away) can't blow up the
// per-request compute cost; #843 requires this to stay interactive.
const maxProjectionPeriods = 360 // 30 years

// minHistoryForMeasuredParams mirrors internal/forecast's minHistory
// threshold: fewer than this many real data points is too noisy to trust
// over the documented prior.
const minHistoryForMeasuredParams = 3

// Fallback yield assumption used when a vault has too little realized-APY
// history to measure its own volatility. See README.md ("Yield distribution
// model") for the rationale.
const (
	fallbackMeanAnnualAPYPct   = 5.0
	fallbackStdDevAnnualAPYPct = 3.0
)

// New-user contribution-behavior prior, used when a user has too little
// deposit history on this vault to fit a personal reliability estimate. See
// README.md ("Contribution behavior model") for the rationale.
const (
	newUserSkipProbability = 0.10
	newUserContributionCV  = 0.15
	maxSkipProbability     = 0.90 // never assume near-certain failure
)

// projectionCacheTTL is short enough that a genuine change in APY or
// deposit history shows up promptly, while making repeated renders of the
// same planner state (the common "adjust a slider, look, adjust it back"
// interaction) cheap. There is no shared/generic cache package in this
// repo; this follows the same in-process map+mutex+TTL pattern already
// used by internal/service/vault_analytics_service.go's analyticsCache and
// internal/oracle/cache.go's RateCache.
const projectionCacheTTL = 5 * time.Minute

// VaultReader is the narrow slice of vault.Repository the projection
// service needs: the vault's current balance/currency, and the requesting
// user's own deposit history on it (used to estimate contribution
// reliability).
type VaultReader interface {
	GetVault(ctx context.Context, id uuid.UUID) (vault.Vault, error)
	ListUserVaultTransactions(ctx context.Context, userID, vaultID uuid.UUID) ([]vault.VaultTransaction, error)
}

// APYHistoryReader is the narrow slice of performance.SnapshotRepository the
// projection service needs: the vault's realized APY history, used to
// ground the simulated yield distribution's volatility in real data.
type APYHistoryReader interface {
	ListAPY(ctx context.Context, vaultID uuid.UUID) ([]performance.APYRecord, error)
}

// ProjectionGoalInput is the optional target+deadline a projection is
// evaluated against.
type ProjectionGoalInput struct {
	TargetAmount decimal.Decimal
	Deadline     time.Time
}

// ProjectionRequest is the input to ProjectionService.Project.
type ProjectionRequest struct {
	VaultID uuid.UUID
	// DepositAmount is the user's scheduled recurring deposit.
	DepositAmount decimal.Decimal
	// DepositCadenceDays is how often the user intends to deposit (e.g. 30
	// for monthly). Used both as the simulation's period length and, when
	// the user has too little deposit history to infer their own cadence,
	// as the cadence the skip-rate estimate is measured against.
	DepositCadenceDays int
	// HorizonMonths is the projection horizon when no goal deadline is
	// given. Ignored (the goal deadline is used instead) when Goal != nil.
	HorizonMonths int
	Goal          *ProjectionGoalInput
}

type projectionCacheEntry struct {
	result    projection.Result
	expiresAt time.Time
}

// ProjectionService computes Monte Carlo savings projections.
type ProjectionService struct {
	vaults VaultReader
	apy    APYHistoryReader

	mu    sync.RWMutex
	cache map[string]projectionCacheEntry
}

// NewProjectionService constructs a ProjectionService.
func NewProjectionService(vaults VaultReader, apy APYHistoryReader) *ProjectionService {
	return &ProjectionService{
		vaults: vaults,
		apy:    apy,
		cache:  make(map[string]projectionCacheEntry),
	}
}

// Project runs a Monte Carlo projection for the given user + request,
// grounding yield volatility in the vault's historical APY and
// contribution reliability in the user's own deposit history on this
// vault. Results are cached for projectionCacheTTL keyed on the full set
// of inputs (so an identical request from the same or a different caller
// within the caching window returns the same cached result without
// re-simulating).
func (s *ProjectionService) Project(ctx context.Context, userID uuid.UUID, req ProjectionRequest) (projection.Result, error) {
	if err := validateProjectionRequest(req); err != nil {
		return projection.Result{}, err
	}

	v, err := s.vaults.GetVault(ctx, req.VaultID)
	if err != nil {
		return projection.Result{}, err
	}

	cadenceDays := req.DepositCadenceDays
	if cadenceDays <= 0 {
		cadenceDays = 30
	}

	periods, deadlinePeriod, hasGoal := resolveHorizon(req, cadenceDays)

	cacheKey := buildCacheKey(userID, req, periods)
	if cached, ok := s.getCached(cacheKey); ok {
		return cached, nil
	}

	apyRecords, err := s.apy.ListAPY(ctx, req.VaultID)
	if err != nil {
		return projection.Result{}, fmt.Errorf("projection: fetch APY history: %w", err)
	}
	meanAnnualAPY, stdDevAnnualAPY, apyPoints := deriveYieldParams(apyRecords)

	deposits, err := s.vaults.ListUserVaultTransactions(ctx, userID, req.VaultID)
	if err != nil {
		return projection.Result{}, fmt.Errorf("projection: fetch deposit history: %w", err)
	}
	skipProb, contribCV, depositCount, newUserPrior := deriveContributionReliability(deposits, cadenceDays)

	startingBalance, _ := v.CurrentBalance.Float64()
	depositAmount, _ := req.DepositAmount.Float64()

	simParams := projection.SimParams{
		StartingBalance:             startingBalance,
		ContributionAmount:          depositAmount,
		ContributionSkipProbability: skipProb,
		ContributionAmountCV:        contribCV,
		YieldMeanPerPeriod:          meanAnnualAPY / 100 / 12,
		YieldStdDevPerPeriod:        stdDevAnnualAPY / 100 / math.Sqrt(12),
		Periods:                     periods,
		Paths:                       defaultSimulationPaths,
		HasGoal:                     hasGoal,
		DeadlinePeriod:              deadlinePeriod,
	}
	if hasGoal {
		target, _ := req.Goal.TargetAmount.Float64()
		simParams.GoalTarget = target
	}

	seed := seedForRequest(userID, req, periods)
	draws := projection.GenerateDraws(seed, simParams.Paths, periods)
	simResult := projection.Simulate(simParams, draws)

	result := projection.Result{
		VaultID:       req.VaultID,
		HorizonMonths: periods,
		EndingBalance: projection.Band{
			P10: decimal.NewFromFloat(simResult.P10).Round(2),
			P50: decimal.NewFromFloat(simResult.P50).Round(2),
			P90: decimal.NewFromFloat(simResult.P90).Round(2),
		},
		Seed: seed,
		Assumptions: projection.Assumptions{
			YieldMeanAnnualPct:          round4(meanAnnualAPY),
			YieldStdDevAnnualPct:        round4(stdDevAnnualAPY),
			YieldSourceHistoryPoints:    apyPoints,
			ContributionSkipProbability: round4(skipProb),
			ContributionAmountCV:        round4(contribCV),
			ContributionHistoryDeposits: depositCount,
			NewUserPriorUsed:            newUserPrior,
			Paths:                       simParams.Paths,
		},
		GeneratedAt: time.Now().UTC(),
	}
	if hasGoal {
		p := simResult.SuccessProbability
		result.SuccessProbability = &p
	}

	result.Sensitivity = s.sensitivityGrid(simParams, draws, hasGoal)

	s.setCached(cacheKey, result)
	return result, nil
}

// resolveHorizon determines the number of monthly periods to simulate and,
// when a goal is present, which period index is the deadline.
func resolveHorizon(req ProjectionRequest, cadenceDays int) (periods int, deadlinePeriod int, hasGoal bool) {
	if req.Goal != nil {
		months := monthsUntil(req.Goal.Deadline)
		if months < 1 {
			months = 1
		}
		if months > maxProjectionPeriods {
			months = maxProjectionPeriods
		}
		return months, months - 1, true
	}
	horizon := req.HorizonMonths
	if horizon <= 0 {
		horizon = 12
	}
	if horizon > maxProjectionPeriods {
		horizon = maxProjectionPeriods
	}
	return horizon, horizon - 1, false
}

func monthsUntil(deadline time.Time) int {
	now := time.Now().UTC()
	d := deadline.UTC()
	if !d.After(now) {
		return 1
	}
	months := (d.Year()-now.Year())*12 + int(d.Month()) - int(now.Month())
	if d.Day() > now.Day() {
		months++ // round up a partial month
	}
	if months < 1 {
		months = 1
	}
	return months
}

// deriveYieldParams computes the mean and (annualized) standard deviation
// of a vault's realized APY history. Mirrors the recency-agnostic mean/
// stddev approach in internal/forecast/forecast.go; falls back to a
// documented conservative default when there are too few points to trust
// (see README.md).
func deriveYieldParams(records []performance.APYRecord) (meanAnnualPct, stdDevAnnualPct float64, points int) {
	if len(records) < minHistoryForMeasuredParams {
		return fallbackMeanAnnualAPYPct, fallbackStdDevAnnualAPYPct, len(records)
	}
	values := make([]float64, 0, len(records))
	for _, r := range records {
		v, _ := r.RealizedAPY.Float64()
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		values = append(values, v)
	}
	if len(values) < minHistoryForMeasuredParams {
		return fallbackMeanAnnualAPYPct, fallbackStdDevAnnualAPYPct, len(values)
	}
	mean := meanOf(values)
	std := stdDevOf(values, mean)
	return mean, std, len(values)
}

// deriveContributionReliability estimates a per-user skip probability and
// contribution-size coefficient of variation from their historical deposit
// transactions, falling back to the documented new-user prior when there
// isn't enough history to measure (see README.md).
func deriveContributionReliability(txns []vault.VaultTransaction, requestedCadenceDays int) (skipProb, contribCV float64, depositCount int, newUserPrior bool) {
	deposits := make([]vault.VaultTransaction, 0, len(txns))
	for _, t := range txns {
		if t.Type == "deposit" {
			deposits = append(deposits, t)
		}
	}
	sort.Slice(deposits, func(i, j int) bool { return deposits[i].CreatedAt.Before(deposits[j].CreatedAt) })
	depositCount = len(deposits)

	if depositCount < minHistoryForMeasuredParams {
		return newUserSkipProbability, newUserContributionCV, depositCount, true
	}

	// Median gap between consecutive deposits = the user's actual cadence.
	gaps := make([]float64, 0, depositCount-1)
	for i := 1; i < depositCount; i++ {
		days := deposits[i].CreatedAt.Sub(deposits[i-1].CreatedAt).Hours() / 24
		if days > 0 {
			gaps = append(gaps, days)
		}
	}
	medianGapDays := requestedCadenceDays
	if len(gaps) > 0 {
		medianGapDays = int(medianOf(gaps))
		if medianGapDays <= 0 {
			medianGapDays = requestedCadenceDays
		}
	}

	spanDays := deposits[depositCount-1].CreatedAt.Sub(deposits[0].CreatedAt).Hours() / 24
	expected := spanDays / float64(medianGapDays)
	actual := float64(depositCount - 1)
	skipProb = 0
	if expected > 0 {
		skipProb = 1 - actual/expected
	}
	if skipProb < 0 {
		skipProb = 0
	}
	if skipProb > maxSkipProbability {
		skipProb = maxSkipProbability
	}

	amounts := make([]float64, 0, depositCount)
	for _, d := range deposits {
		v, _ := d.Amount.Float64()
		amounts = append(amounts, v)
	}
	amtMean := meanOf(amounts)
	amtStd := stdDevOf(amounts, amtMean)
	contribCV = newUserContributionCV
	if amtMean > 0 {
		contribCV = amtStd / amtMean
	}

	return skipProb, contribCV, depositCount, false
}

// sensitivityGrid runs the same simulation across a small grid of deposit
// amounts (holding the base deadline fixed) using the same Draws matrix
// (common random numbers), so the resulting success-probability (or, when
// there's no goal, median-balance) curve is guaranteed monotonic in
// deposit amount. See projection.SensitivityGrid's doc comment.
func (s *ProjectionService) sensitivityGrid(base projection.SimParams, draws projection.Draws, hasGoal bool) []projection.SensitivityCell {
	multipliers := []float64{0.5, 0.75, 1.0, 1.25, 1.5, 2.0}
	deposits := make([]float64, len(multipliers))
	for i, m := range multipliers {
		deposits[i] = base.ContributionAmount * m
	}

	points := projection.SensitivityGrid(base, draws, deposits, []int{base.DeadlinePeriod})

	cells := make([]projection.SensitivityCell, 0, len(points))
	for _, pt := range points {
		cell := projection.SensitivityCell{
			DepositAmount:  decimal.NewFromFloat(pt.ContributionAmount).Round(2),
			DeadlineMonths: pt.Periods,
		}
		if hasGoal {
			p := pt.Result.SuccessProbability
			cell.SuccessProbability = &p
		} else {
			cell.MedianBalance = decimal.NewFromFloat(pt.Result.P50).Round(2)
		}
		cells = append(cells, cell)
	}
	return cells
}

func seedForRequest(userID uuid.UUID, req ProjectionRequest, periods int) int64 {
	deadline := ""
	if req.Goal != nil {
		deadline = req.Goal.Deadline.UTC().Format(time.RFC3339)
	}
	target := ""
	if req.Goal != nil {
		target = req.Goal.TargetAmount.String()
	}
	return projection.SeedFromInputs(
		userID.String(),
		req.VaultID.String(),
		req.DepositAmount.String(),
		fmt.Sprintf("%d", req.DepositCadenceDays),
		target,
		deadline,
		fmt.Sprintf("%d", periods),
	)
}

func buildCacheKey(userID uuid.UUID, req ProjectionRequest, periods int) string {
	deadline := ""
	target := ""
	if req.Goal != nil {
		deadline = req.Goal.Deadline.UTC().Format(time.RFC3339)
		target = req.Goal.TargetAmount.String()
	}
	return fmt.Sprintf("%s|%s|%s|%d|%s|%s|%d",
		userID, req.VaultID, req.DepositAmount.String(), req.DepositCadenceDays, target, deadline, periods)
}

func (s *ProjectionService) getCached(key string) (projection.Result, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.cache[key]
	if !ok || time.Now().After(entry.expiresAt) {
		return projection.Result{}, false
	}
	return entry.result, true
}

func (s *ProjectionService) setCached(key string, result projection.Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache[key] = projectionCacheEntry{result: result, expiresAt: time.Now().Add(projectionCacheTTL)}
}

func validateProjectionRequest(req ProjectionRequest) error {
	if req.VaultID == uuid.Nil {
		return fmt.Errorf("%w: vault_id is required", projection.ErrInvalidRequest)
	}
	if req.DepositAmount.IsNegative() {
		return fmt.Errorf("%w: deposit_amount must not be negative", projection.ErrInvalidRequest)
	}
	if req.Goal != nil {
		if !req.Goal.TargetAmount.IsPositive() {
			return fmt.Errorf("%w: goal target_amount must be positive", projection.ErrInvalidRequest)
		}
		if !req.Goal.Deadline.After(time.Now().UTC()) {
			return fmt.Errorf("%w: goal deadline must be in the future", projection.ErrInvalidRequest)
		}
	}
	return nil
}

func meanOf(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

func stdDevOf(xs []float64, mean float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	var ss float64
	for _, x := range xs {
		d := x - mean
		ss += d * d
	}
	return math.Sqrt(ss / float64(len(xs)))
}

func medianOf(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

func round4(v float64) float64 {
	return math.Round(v*10000) / 10000
}
