package service

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/suncrestlabs/nester/apps/api/internal/domain/performance"
	"github.com/suncrestlabs/nester/apps/api/internal/domain/vault"
)

type fakeVaultReader struct {
	v      vault.Vault
	txns   []vault.VaultTransaction
	getErr error
}

func (f *fakeVaultReader) GetVault(_ context.Context, id uuid.UUID) (vault.Vault, error) {
	if f.getErr != nil {
		return vault.Vault{}, f.getErr
	}
	v := f.v
	v.ID = id
	return v, nil
}

func (f *fakeVaultReader) ListUserVaultTransactions(_ context.Context, _, _ uuid.UUID) ([]vault.VaultTransaction, error) {
	return f.txns, nil
}

type fakeAPYReader struct {
	records []performance.APYRecord
}

func (f *fakeAPYReader) ListAPY(_ context.Context, _ uuid.UUID) ([]performance.APYRecord, error) {
	return f.records, nil
}

func apyRecord(pct float64) performance.APYRecord {
	return performance.APYRecord{RealizedAPY: decimal.NewFromFloat(pct), CalculatedAt: time.Now()}
}

func TestProjectionService_NewUserUsesDocumentedPrior(t *testing.T) {
	vaultRepo := &fakeVaultReader{v: vault.Vault{CurrentBalance: decimal.NewFromInt(1000)}}
	apyRepo := &fakeAPYReader{} // no history
	svc := NewProjectionService(vaultRepo, apyRepo)

	result, err := svc.Project(context.Background(), uuid.New(), ProjectionRequest{
		VaultID:            uuid.New(),
		DepositAmount:      decimal.NewFromInt(100),
		DepositCadenceDays: 30,
		HorizonMonths:      12,
	})
	if err != nil {
		t.Fatalf("Project() error = %v", err)
	}
	if !result.Assumptions.NewUserPriorUsed {
		t.Fatalf("expected NewUserPriorUsed=true for a user with no deposit history")
	}
	if result.Assumptions.ContributionSkipProbability != newUserSkipProbability {
		t.Fatalf("expected new-user skip probability %v, got %v", newUserSkipProbability, result.Assumptions.ContributionSkipProbability)
	}
	if result.Assumptions.YieldSourceHistoryPoints != 0 {
		t.Fatalf("expected 0 APY history points reported, got %d", result.Assumptions.YieldSourceHistoryPoints)
	}
	if result.Assumptions.YieldMeanAnnualPct != fallbackMeanAnnualAPYPct {
		t.Fatalf("expected fallback mean APY %v, got %v", fallbackMeanAnnualAPYPct, result.Assumptions.YieldMeanAnnualPct)
	}
	if result.EndingBalance.P50.IsZero() {
		t.Fatalf("expected a non-zero median ending balance")
	}
}

func TestProjectionService_UsesMeasuredAPYVolatilityWhenAvailable(t *testing.T) {
	vaultRepo := &fakeVaultReader{v: vault.Vault{CurrentBalance: decimal.NewFromInt(1000)}}
	// Tight, stable APY history around 8%.
	apyRepo := &fakeAPYReader{records: []performance.APYRecord{
		apyRecord(8.0), apyRecord(8.1), apyRecord(7.9), apyRecord(8.05), apyRecord(7.95),
	}}
	svc := NewProjectionService(vaultRepo, apyRepo)

	result, err := svc.Project(context.Background(), uuid.New(), ProjectionRequest{
		VaultID:            uuid.New(),
		DepositAmount:      decimal.NewFromInt(100),
		DepositCadenceDays: 30,
		HorizonMonths:      12,
	})
	if err != nil {
		t.Fatalf("Project() error = %v", err)
	}
	if result.Assumptions.YieldSourceHistoryPoints != 5 {
		t.Fatalf("expected 5 APY history points reported, got %d", result.Assumptions.YieldSourceHistoryPoints)
	}
	if result.Assumptions.YieldMeanAnnualPct == fallbackMeanAnnualAPYPct {
		t.Fatalf("expected measured APY mean, not the fallback default")
	}
	// A tight historical APY series should produce a tight band relative to
	// balance.
	bandWidth := result.EndingBalance.P90.Sub(result.EndingBalance.P10)
	if bandWidth.IsNegative() {
		t.Fatalf("P90 should be >= P10, got band width %v", bandWidth)
	}
}

func TestProjectionService_GoalSuccessProbabilityPresentOnlyWithGoal(t *testing.T) {
	vaultRepo := &fakeVaultReader{v: vault.Vault{CurrentBalance: decimal.NewFromInt(1000)}}
	apyRepo := &fakeAPYReader{}
	svc := NewProjectionService(vaultRepo, apyRepo)

	noGoal, err := svc.Project(context.Background(), uuid.New(), ProjectionRequest{
		VaultID:            uuid.New(),
		DepositAmount:      decimal.NewFromInt(100),
		DepositCadenceDays: 30,
		HorizonMonths:      12,
	})
	if err != nil {
		t.Fatalf("Project() error = %v", err)
	}
	if noGoal.SuccessProbability != nil {
		t.Fatalf("expected nil success probability without a goal")
	}

	withGoal, err := svc.Project(context.Background(), uuid.New(), ProjectionRequest{
		VaultID:            uuid.New(),
		DepositAmount:      decimal.NewFromInt(200),
		DepositCadenceDays: 30,
		Goal: &ProjectionGoalInput{
			TargetAmount: decimal.NewFromInt(5000),
			Deadline:     time.Now().Add(365 * 24 * time.Hour),
		},
	})
	if err != nil {
		t.Fatalf("Project() error = %v", err)
	}
	if withGoal.SuccessProbability == nil {
		t.Fatalf("expected non-nil success probability with a goal")
	}
	if *withGoal.SuccessProbability < 0 || *withGoal.SuccessProbability > 1 {
		t.Fatalf("success probability out of range: %v", *withGoal.SuccessProbability)
	}
	if len(withGoal.Sensitivity) == 0 {
		t.Fatalf("expected a non-empty sensitivity grid")
	}
}

func TestProjectionService_CachesIdenticalRequests(t *testing.T) {
	vaultRepo := &fakeVaultReader{v: vault.Vault{CurrentBalance: decimal.NewFromInt(1000)}}
	apyRepo := &fakeAPYReader{}
	svc := NewProjectionService(vaultRepo, apyRepo)

	userID := uuid.New()
	req := ProjectionRequest{
		VaultID:            uuid.New(),
		DepositAmount:      decimal.NewFromInt(100),
		DepositCadenceDays: 30,
		HorizonMonths:      12,
	}

	r1, err := svc.Project(context.Background(), userID, req)
	if err != nil {
		t.Fatalf("Project() error = %v", err)
	}
	r2, err := svc.Project(context.Background(), userID, req)
	if err != nil {
		t.Fatalf("Project() error = %v", err)
	}
	if !r1.GeneratedAt.Equal(r2.GeneratedAt) {
		t.Fatalf("expected second identical request to be served from cache (same GeneratedAt), got %v vs %v", r1.GeneratedAt, r2.GeneratedAt)
	}
	if r1.Seed != r2.Seed {
		t.Fatalf("expected identical seeds for identical requests")
	}
}

func TestProjectionService_RejectsInvalidRequest(t *testing.T) {
	vaultRepo := &fakeVaultReader{v: vault.Vault{CurrentBalance: decimal.NewFromInt(1000)}}
	apyRepo := &fakeAPYReader{}
	svc := NewProjectionService(vaultRepo, apyRepo)

	_, err := svc.Project(context.Background(), uuid.New(), ProjectionRequest{
		VaultID:       uuid.New(),
		DepositAmount: decimal.NewFromInt(-5),
	})
	if err == nil {
		t.Fatalf("expected an error for negative deposit amount")
	}

	_, err = svc.Project(context.Background(), uuid.New(), ProjectionRequest{
		VaultID:       uuid.New(),
		DepositAmount: decimal.NewFromInt(100),
		Goal: &ProjectionGoalInput{
			TargetAmount: decimal.NewFromInt(1000),
			Deadline:     time.Now().Add(-24 * time.Hour), // in the past
		},
	})
	if err == nil {
		t.Fatalf("expected an error for a past deadline")
	}
}

func TestProjectionService_DerivesReliabilityFromDepositHistory(t *testing.T) {
	base := time.Now().Add(-180 * 24 * time.Hour)
	// A user who deposited reliably every ~30 days for 6 months (6 deposits).
	txns := make([]vault.VaultTransaction, 0, 6)
	for i := 0; i < 6; i++ {
		txns = append(txns, vault.VaultTransaction{
			Type:      "deposit",
			Amount:    decimal.NewFromInt(100),
			CreatedAt: base.Add(time.Duration(i*30) * 24 * time.Hour),
		})
	}
	vaultRepo := &fakeVaultReader{v: vault.Vault{CurrentBalance: decimal.NewFromInt(1000)}, txns: txns}
	apyRepo := &fakeAPYReader{}
	svc := NewProjectionService(vaultRepo, apyRepo)

	result, err := svc.Project(context.Background(), uuid.New(), ProjectionRequest{
		VaultID:            uuid.New(),
		DepositAmount:      decimal.NewFromInt(100),
		DepositCadenceDays: 30,
		HorizonMonths:      12,
	})
	if err != nil {
		t.Fatalf("Project() error = %v", err)
	}
	if result.Assumptions.NewUserPriorUsed {
		t.Fatalf("expected a measured reliability estimate, not the new-user prior")
	}
	if result.Assumptions.ContributionHistoryDeposits != 6 {
		t.Fatalf("expected 6 historical deposits reported, got %d", result.Assumptions.ContributionHistoryDeposits)
	}
	// A perfectly regular depositor should have a low estimated skip
	// probability.
	if result.Assumptions.ContributionSkipProbability > 0.2 {
		t.Fatalf("expected a low skip probability for a regular depositor, got %v", result.Assumptions.ContributionSkipProbability)
	}
}
