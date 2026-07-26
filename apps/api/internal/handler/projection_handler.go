package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/suncrestlabs/nester/apps/api/internal/auth"
	"github.com/suncrestlabs/nester/apps/api/internal/domain/projection"
	"github.com/suncrestlabs/nester/apps/api/internal/domain/vault"
	"github.com/suncrestlabs/nester/apps/api/internal/service"
	logpkg "github.com/suncrestlabs/nester/apps/api/pkg/logger"
	"github.com/suncrestlabs/nester/apps/api/pkg/response"
)

// ProjectionRunner is the service surface the handler depends on (issue
// #843: Monte Carlo savings forecasting).
type ProjectionRunner interface {
	Project(ctx context.Context, userID uuid.UUID, req service.ProjectionRequest) (projection.Result, error)
}

// ProjectionHandler serves the Monte Carlo savings projection endpoint.
type ProjectionHandler struct {
	svc ProjectionRunner
}

func NewProjectionHandler(svc ProjectionRunner) *ProjectionHandler {
	return &ProjectionHandler{svc: svc}
}

func (h *ProjectionHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/vaults/{id}/projection", h.project)
}

type projectionGoalRequest struct {
	TargetAmount json.Number `json:"target_amount"`
	Deadline     string      `json:"deadline"`
}

type projectionRequestBody struct {
	DepositAmount      json.Number            `json:"deposit_amount"`
	DepositCadenceDays int                    `json:"deposit_cadence_days"`
	HorizonMonths      int                    `json:"horizon_months"`
	Goal               *projectionGoalRequest `json:"goal"`
}

func (h *ProjectionHandler) project(w http.ResponseWriter, r *http.Request) {
	user, ok := auth.GetUserFromContext(r.Context())
	if !ok {
		response.WriteJSON(w, http.StatusUnauthorized, response.Err(http.StatusUnauthorized, "UNAUTHORIZED", "authentication required"))
		return
	}
	userID, err := uuid.Parse(user.ID)
	if err != nil {
		response.WriteJSON(w, http.StatusUnauthorized, response.Err(http.StatusUnauthorized, "UNAUTHORIZED", "invalid token subject"))
		return
	}

	vaultID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		response.WriteJSON(w, http.StatusBadRequest, response.ValidationErr("vault id must be a valid UUID"))
		return
	}

	var body projectionRequestBody
	if err := decodeJSON(r, &body); err != nil {
		response.WriteJSON(w, http.StatusBadRequest, response.ValidationErr("invalid request body: "+err.Error()))
		return
	}

	depositAmount, err := decimal.NewFromString(body.DepositAmount.String())
	if err != nil {
		response.WriteJSON(w, http.StatusBadRequest, response.ValidationErr("deposit_amount must be a valid number"))
		return
	}

	req := service.ProjectionRequest{
		VaultID:            vaultID,
		DepositAmount:      depositAmount,
		DepositCadenceDays: body.DepositCadenceDays,
		HorizonMonths:      body.HorizonMonths,
	}

	if body.Goal != nil {
		target, err := decimal.NewFromString(body.Goal.TargetAmount.String())
		if err != nil {
			response.WriteJSON(w, http.StatusBadRequest, response.ValidationErr("goal.target_amount must be a valid number"))
			return
		}
		deadline, err := time.Parse(time.RFC3339, body.Goal.Deadline)
		if err != nil {
			response.WriteJSON(w, http.StatusBadRequest, response.ValidationErr("goal.deadline must be RFC3339"))
			return
		}
		req.Goal = &service.ProjectionGoalInput{TargetAmount: target, Deadline: deadline}
	}

	result, err := h.svc.Project(r.Context(), userID, req)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	response.WriteJSON(w, http.StatusOK, response.OK(result))
}

func (h *ProjectionHandler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, projection.ErrInvalidRequest):
		response.WriteJSON(w, http.StatusBadRequest, response.ValidationErr(err.Error()))
	case errors.Is(err, vault.ErrVaultNotFound):
		response.WriteJSON(w, http.StatusNotFound, response.NotFound("vault"))
	default:
		logpkg.FromContext(r.Context()).Error("projection handler failed", "error", err.Error())
		response.WriteJSON(w, http.StatusInternalServerError, response.Err(http.StatusInternalServerError, "INTERNAL_ERROR", "projection failed"))
	}
}
