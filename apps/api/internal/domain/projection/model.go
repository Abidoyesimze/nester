// Package projection implements Monte Carlo savings forecasting (issue #843).
//
// A deterministic point projection ("your balance will be X in 12 months")
// is a false certainty: realized yield varies period to period and users
// skip scheduled deposits. This package instead simulates many independent
// randomized paths over the horizon and reports the resulting *distribution*
// of outcomes — a median (P50), a confidence band (P10-P90), and, for a goal
// with a target amount and deadline, the fraction of simulated paths that
// reach the target in time (the "success probability").
//
// See README.md in this directory for the full list of documented
// distributional assumptions (required as an acceptance criterion for #843).
package projection

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

var (
	// ErrInvalidRequest is returned when a projection request fails basic
	// validation (non-positive amounts, zero horizon, past deadline, etc).
	ErrInvalidRequest = errors.New("projection: invalid request")
)

// GoalTarget is the optional target amount + deadline a projection is
// evaluated against. When present, the result includes a SuccessProbability:
// the fraction of simulated paths whose balance reaches TargetAmount at or
// before Deadline.
type GoalTarget struct {
	TargetAmount decimal.Decimal `json:"target_amount"`
	Deadline     time.Time       `json:"deadline"`
}

// Band is a percentile distribution: a median plus a confidence interval.
type Band struct {
	P10 decimal.Decimal `json:"p10"`
	P50 decimal.Decimal `json:"p50"`
	P90 decimal.Decimal `json:"p90"`
}

// SensitivityCell is one point in the deposit/deadline sensitivity grid: the
// success probability (or, absent a goal, the median ending balance) if the
// user made this deposit amount on this deadline instead of the base
// request's values.
type SensitivityCell struct {
	DepositAmount      decimal.Decimal `json:"deposit_amount"`
	DeadlineMonths     int             `json:"deadline_months,omitempty"`
	SuccessProbability *float64        `json:"success_probability,omitempty"`
	MedianBalance      decimal.Decimal `json:"median_balance,omitempty"`
}

// Assumptions documents, per response, the concrete distributional
// parameters this particular projection used — so the numbers are never
// presented as unexplained. See README.md for the model rationale.
type Assumptions struct {
	YieldMeanAnnualPct          float64 `json:"yield_mean_annual_pct"`
	YieldStdDevAnnualPct        float64 `json:"yield_stddev_annual_pct"`
	YieldSourceHistoryPoints    int     `json:"yield_source_history_points"`
	ContributionSkipProbability float64 `json:"contribution_skip_probability"`
	ContributionAmountCV        float64 `json:"contribution_amount_cv"`
	ContributionHistoryDeposits int     `json:"contribution_history_deposits"`
	NewUserPriorUsed            bool    `json:"new_user_prior_used"`
	Paths                       int     `json:"paths"`
}

// Result is the full output of a Monte Carlo projection.
type Result struct {
	VaultID            uuid.UUID         `json:"vault_id"`
	HorizonMonths      int               `json:"horizon_months"`
	EndingBalance      Band              `json:"ending_balance"`
	SuccessProbability *float64          `json:"success_probability,omitempty"`
	Sensitivity        []SensitivityCell `json:"sensitivity,omitempty"`
	Seed               int64             `json:"seed"`
	Assumptions        Assumptions       `json:"assumptions"`
	GeneratedAt        time.Time         `json:"generated_at"`
}
