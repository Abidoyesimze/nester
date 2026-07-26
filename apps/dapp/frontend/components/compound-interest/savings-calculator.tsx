"use client";

import { useState } from "react";
import { getSavingsProjection, type ProjectionResult } from "@/lib/api/projection";
import SavingsChart from "@/components/analytics/SavingsChart";

/**
 * SavingsCalculator — an interactive "what happens if I save X per month"
 * planner (issue #843). Calls the Go API's Monte Carlo projection endpoint
 * and renders the resulting outcome BAND (P10/P50/P90), the probability of
 * hitting an optional goal by its deadline, and a deposit-amount
 * sensitivity grid ("increasing your monthly deposit raises your success
 * probability from X% to Y%").
 */
export interface SavingsCalculatorProps {
  vaultId: string;
  currentBalance: number;
}

export default function SavingsCalculator({ vaultId, currentBalance }: SavingsCalculatorProps) {
  const [depositAmount, setDepositAmount] = useState("100");
  const [cadenceDays, setCadenceDays] = useState("30");
  const [horizonMonths, setHorizonMonths] = useState("12");
  const [goalEnabled, setGoalEnabled] = useState(false);
  const [targetAmount, setTargetAmount] = useState("5000");
  const [deadline, setDeadline] = useState("");

  const [result, setResult] = useState<ProjectionResult | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const runProjection = async () => {
    setLoading(true);
    setError(null);
    try {
      const projection = await getSavingsProjection(vaultId, {
        deposit_amount: depositAmount,
        deposit_cadence_days: Number(cadenceDays) || 30,
        horizon_months: Number(horizonMonths) || 12,
        goal:
          goalEnabled && deadline
            ? { target_amount: targetAmount, deadline: new Date(deadline).toISOString() }
            : undefined,
      });
      setResult(projection);
    } catch {
      setError("Couldn't run the projection. Please try again.");
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="rounded-2xl border border-border bg-white p-4">
      <p className="mb-3 text-sm font-medium text-foreground">Savings projection</p>

      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <label className="flex flex-col gap-1 text-xs text-muted-foreground">
          Monthly deposit
          <input
            type="number"
            min="0"
            step="1"
            value={depositAmount}
            onChange={(e) => setDepositAmount(e.target.value)}
            className="rounded-lg border border-border px-2 py-1 text-sm"
          />
        </label>
        <label className="flex flex-col gap-1 text-xs text-muted-foreground">
          Cadence (days)
          <input
            type="number"
            min="1"
            step="1"
            value={cadenceDays}
            onChange={(e) => setCadenceDays(e.target.value)}
            className="rounded-lg border border-border px-2 py-1 text-sm"
          />
        </label>
        <label className="flex flex-col gap-1 text-xs text-muted-foreground">
          Horizon (months)
          <input
            type="number"
            min="1"
            step="1"
            value={horizonMonths}
            onChange={(e) => setHorizonMonths(e.target.value)}
            disabled={goalEnabled}
            className="rounded-lg border border-border px-2 py-1 text-sm disabled:opacity-50"
          />
        </label>
        <label className="flex items-end gap-2 text-xs text-muted-foreground">
          <input
            type="checkbox"
            checked={goalEnabled}
            onChange={(e) => setGoalEnabled(e.target.checked)}
          />
          Set a goal
        </label>
      </div>

      {goalEnabled && (
        <div className="mt-3 grid grid-cols-2 gap-3">
          <label className="flex flex-col gap-1 text-xs text-muted-foreground">
            Target amount
            <input
              type="number"
              min="0"
              step="1"
              value={targetAmount}
              onChange={(e) => setTargetAmount(e.target.value)}
              className="rounded-lg border border-border px-2 py-1 text-sm"
            />
          </label>
          <label className="flex flex-col gap-1 text-xs text-muted-foreground">
            Deadline
            <input
              type="date"
              value={deadline}
              onChange={(e) => setDeadline(e.target.value)}
              className="rounded-lg border border-border px-2 py-1 text-sm"
            />
          </label>
        </div>
      )}

      <button
        type="button"
        onClick={runProjection}
        disabled={loading}
        className="mt-3 rounded-lg bg-foreground px-3 py-1.5 text-xs font-medium text-white disabled:opacity-50"
      >
        {loading ? "Running simulation…" : "Run projection"}
      </button>

      {error && <p className="mt-3 text-xs text-red-600">{error}</p>}

      {result && (
        <div className="mt-4">
          <SavingsChart
            currentBalance={currentBalance}
            horizonMonths={result.horizon_months}
            p10={Number(result.ending_balance.p10)}
            p50={Number(result.ending_balance.p50)}
            p90={Number(result.ending_balance.p90)}
          />

          {typeof result.success_probability === "number" && (
            <p className="mt-2 text-sm text-foreground">
              Probability of reaching your goal by the deadline:{" "}
              <span className="font-semibold">{Math.round(result.success_probability * 100)}%</span>
            </p>
          )}

          {result.sensitivity && result.sensitivity.length > 0 && (
            <div className="mt-4 overflow-x-auto">
              <p className="mb-2 text-xs font-medium text-muted-foreground">
                What if you deposited a different amount each period?
              </p>
              <table className="w-full text-left text-xs">
                <thead>
                  <tr className="text-muted-foreground">
                    <th className="pb-1 pr-4">Deposit</th>
                    <th className="pb-1">
                      {result.success_probability !== undefined ? "Success probability" : "Median balance"}
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {result.sensitivity.map((cell) => (
                    <tr key={cell.deposit_amount} className="border-t border-border">
                      <td className="py-1 pr-4">${Number(cell.deposit_amount).toLocaleString()}</td>
                      <td className="py-1">
                        {cell.success_probability !== undefined
                          ? `${Math.round(cell.success_probability * 100)}%`
                          : cell.median_balance
                            ? `$${Number(cell.median_balance).toLocaleString()}`
                            : "—"}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}

          <p className="mt-3 text-[10px] text-muted-foreground">
            Based on {result.assumptions.paths.toLocaleString()} simulated paths using{" "}
            {result.assumptions.new_user_prior_used
              ? "a default deposit-reliability assumption (no deposit history yet)"
              : `${result.assumptions.contribution_history_deposits} past deposits`}{" "}
            and{" "}
            {result.assumptions.yield_source_history_points > 0
              ? "the vault's historical APY volatility"
              : "a conservative default APY volatility (limited vault history)"}
            .
          </p>
        </div>
      )}
    </div>
  );
}
