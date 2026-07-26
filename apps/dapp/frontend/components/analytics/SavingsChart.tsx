"use client";

import {
  ComposedChart,
  Area,
  Line,
  XAxis,
  YAxis,
  CartesianGrid,
  Tooltip,
  Legend,
  ResponsiveContainer,
} from "recharts";

/**
 * SavingsChart renders a Monte Carlo savings projection as a confidence
 * BAND (P10-P90 shaded area, P50 median line) instead of a single
 * deterministic line — issue #843. `currentBalance` anchors the left edge
 * of the chart at today's actual balance (a zero-width "band"); the right
 * edge is the simulated outcome distribution at the projection horizon.
 */
export interface SavingsChartProps {
  currentBalance: number;
  horizonMonths: number;
  p10: number;
  p50: number;
  p90: number;
  /** Optional currency prefix for axis/tooltip labels, e.g. "$". */
  currencySymbol?: string;
}

export default function SavingsChart({
  currentBalance,
  horizonMonths,
  p10,
  p50,
  p90,
  currencySymbol = "$",
}: SavingsChartProps) {
  const data = [
    {
      label: "Today",
      band: [currentBalance, currentBalance] as [number, number],
      median: currentBalance,
    },
    {
      label: `In ${horizonMonths} mo`,
      band: [p10, p90] as [number, number],
      median: p50,
    },
  ];

  const format = (value: number) => `${currencySymbol}${Math.round(value).toLocaleString()}`;

  return (
    <div className="w-full">
      <ResponsiveContainer width="100%" height={280}>
        <ComposedChart data={data} margin={{ top: 20, right: 30, left: 0, bottom: 5 }}>
          <CartesianGrid strokeDasharray="3 3" />
          <XAxis dataKey="label" tick={{ fontSize: 12 }} />
          <YAxis tickFormatter={format} tick={{ fontSize: 12 }} width={80} />
          <Tooltip
            formatter={(value, name) => {
              if (name === "band" && Array.isArray(value)) {
                const [low, high] = value as [number, number];
                return [`${format(Number(low))} – ${format(Number(high))}`, "P10 – P90"];
              }
              return [format(Number(value)), "Median (P50)"];
            }}
          />
          <Legend verticalAlign="top" height={36} />
          {/* Shaded confidence band (P10–P90). */}
          <Area
            type="monotone"
            dataKey="band"
            name="P10–P90 range"
            stroke="none"
            fill="#8884d8"
            fillOpacity={0.25}
          />
          {/* Median (P50) projection line. */}
          <Line type="monotone" dataKey="median" name="Median (P50)" stroke="#4f46e5" strokeWidth={2} dot />
        </ComposedChart>
      </ResponsiveContainer>
    </div>
  );
}
