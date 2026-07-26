package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/suncrestlabs/nester/apps/api/internal/auth"
	"github.com/suncrestlabs/nester/apps/api/internal/domain/projection"
	"github.com/suncrestlabs/nester/apps/api/internal/domain/vault"
	"github.com/suncrestlabs/nester/apps/api/internal/service"
)

type mockProjectionRunner struct {
	result projection.Result
	err    error
	// lastReq captures the last request seen, for assertions.
	lastReq service.ProjectionRequest
}

func (m *mockProjectionRunner) Project(_ context.Context, _ uuid.UUID, req service.ProjectionRequest) (projection.Result, error) {
	m.lastReq = req
	return m.result, m.err
}

func withProjectionAuthUser(next http.Handler, userID uuid.UUID) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := auth.User{ID: userID.String(), WalletAddress: "GTEST"}
		next.ServeHTTP(w, r.WithContext(auth.NewContext(r.Context(), u)))
	})
}

func TestProjectionHandler_ReturnsProjection(t *testing.T) {
	userID := uuid.New()
	vaultID := uuid.New()
	successProb := 0.82
	runner := &mockProjectionRunner{
		result: projection.Result{
			VaultID:            vaultID,
			HorizonMonths:      12,
			EndingBalance:      projection.Band{P10: decimal.NewFromInt(1000), P50: decimal.NewFromInt(1200), P90: decimal.NewFromInt(1400)},
			SuccessProbability: &successProb,
			Seed:               42,
		},
	}
	h := NewProjectionHandler(runner)
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(withProjectionAuthUser(mux, userID))
	defer srv.Close()

	body := `{"deposit_amount":"100","deposit_cadence_days":30,"goal":{"target_amount":"5000","deadline":"` +
		time.Now().Add(365*24*time.Hour).UTC().Format(time.RFC3339) + `"}}`

	resp, err := http.Post(srv.URL+"/api/v1/vaults/"+vaultID.String()+"/projection", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var decoded map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded["success"] != true {
		t.Fatalf("expected success=true, got %v", decoded)
	}
	data, ok := decoded["data"].(map[string]any)
	if !ok {
		t.Fatalf("expected data object, got %v", decoded["data"])
	}
	if data["seed"] != float64(42) {
		t.Fatalf("expected seed 42, got %v", data["seed"])
	}

	if runner.lastReq.VaultID != vaultID {
		t.Fatalf("service received vault id %v, want %v", runner.lastReq.VaultID, vaultID)
	}
	if runner.lastReq.Goal == nil {
		t.Fatalf("expected goal to be parsed through")
	}
}

func TestProjectionHandler_RejectsUnauthenticated(t *testing.T) {
	h := NewProjectionHandler(&mockProjectionRunner{})
	mux := http.NewServeMux()
	h.Register(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/vaults/"+uuid.New().String()+"/projection", bytes.NewBufferString(`{"deposit_amount":"10"}`))
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestProjectionHandler_RejectsInvalidVaultID(t *testing.T) {
	userID := uuid.New()
	h := NewProjectionHandler(&mockProjectionRunner{})
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(withProjectionAuthUser(mux, userID))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/v1/vaults/not-a-uuid/projection", "application/json", bytes.NewBufferString(`{"deposit_amount":"10"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestProjectionHandler_MapsInvalidRequestError(t *testing.T) {
	userID := uuid.New()
	runner := &mockProjectionRunner{err: projection.ErrInvalidRequest}
	h := NewProjectionHandler(runner)
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(withProjectionAuthUser(mux, userID))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/v1/vaults/"+uuid.New().String()+"/projection", "application/json", bytes.NewBufferString(`{"deposit_amount":"10"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestProjectionHandler_MapsVaultNotFound(t *testing.T) {
	userID := uuid.New()
	runner := &mockProjectionRunner{err: vault.ErrVaultNotFound}
	h := NewProjectionHandler(runner)
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(withProjectionAuthUser(mux, userID))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/v1/vaults/"+uuid.New().String()+"/projection", "application/json", bytes.NewBufferString(`{"deposit_amount":"10"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
