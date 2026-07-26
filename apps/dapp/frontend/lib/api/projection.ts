// lib/api/projection.ts
// Client for the Monte Carlo savings projection endpoint (issue #843):
// POST /api/v1/vaults/{id}/projection on the Go API. Returns a P50 + P10-P90
// outcome band (instead of a single deterministic number), a goal success
// probability, and a deposit-amount sensitivity grid.

const API_BASE = process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:8080";

export interface ProjectionBand {
  p10: string;
  p50: string;
  p90: string;
}

export interface ProjectionSensitivityCell {
  deposit_amount: string;
  deadline_months?: number;
  success_probability?: number;
  median_balance?: string;
}

export interface ProjectionAssumptions {
  yield_mean_annual_pct: number;
  yield_stddev_annual_pct: number;
  yield_source_history_points: number;
  contribution_skip_probability: number;
  contribution_amount_cv: number;
  contribution_history_deposits: number;
  new_user_prior_used: boolean;
  paths: number;
}

export interface ProjectionResult {
  vault_id: string;
  horizon_months: number;
  ending_balance: ProjectionBand;
  success_probability?: number;
  sensitivity?: ProjectionSensitivityCell[];
  seed: number;
  assumptions: ProjectionAssumptions;
  generated_at: string;
}

export interface ProjectionGoalInput {
  target_amount: string;
  /** RFC3339 timestamp. */
  deadline: string;
}

export interface ProjectionRequestInput {
  deposit_amount: string;
  deposit_cadence_days?: number;
  horizon_months?: number;
  goal?: ProjectionGoalInput;
}

/** Pull the JWT from wherever the app stores it (localStorage key used by auth). */
function getStoredToken(): string {
  if (typeof window === "undefined") return "";
  return localStorage.getItem("nester_token") ?? "";
}

export async function getSavingsProjection(
  vaultId: string,
  input: ProjectionRequestInput
): Promise<ProjectionResult> {
  const res = await fetch(`${API_BASE}/api/v1/vaults/${vaultId}/projection`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${getStoredToken()}`,
    },
    body: JSON.stringify(input),
    // Adjusting a slider in the planner should always hit the (cached,
    // server-side) endpoint rather than a stale browser cache.
    cache: "no-store",
  });

  if (!res.ok) {
    throw new Error(`Projection API error ${res.status}`);
  }

  const json = await res.json();
  if (!json.success) {
    throw new Error(json.error?.message ?? "projection request failed");
  }
  return json.data as ProjectionResult;
}
