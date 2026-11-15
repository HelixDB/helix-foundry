import {
  Bar,
  BarChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import type { AutomationRunDay } from "../../../shared/wfe";
import { calendarDate, formatNumber } from "../../kit";

// The overview's runs by day (recharts, lazy-loaded by the overview so it
// stays out of the main bundle): a bar a day, stacked by how its runs went,
// those that succeeded in --hf-chart-1, failed in --hf-chart-5 and the rest
// (live, cancelled, skipped) in a neutral token, with a tooltip of the
// exact counts. Colours are automations.css's.

type Datum = {
  date: string;
  succeeded: number;
  failed: number;
  other: number;
};

/** "Nov 12" from YYYY-MM-DD, long with the year. */
function dayText(date: string, long = false) {
  const [y, m, d] = date.split("-").map(Number);
  return calendarDate(y!, m!, d!).toLocaleDateString(undefined, {
    month: "short",
    day: "numeric",
    ...(long && { weekday: "short", year: "numeric" }),
  });
}

function Tip({
  active,
  payload,
}: {
  active?: boolean;
  payload?: readonly { payload?: unknown }[];
}) {
  const d = active ? (payload?.[0]?.payload as Datum | undefined) : undefined;
  if (!d) return null;
  return (
    <div className="au-chart-tip">
      <span className="au-chart-tip-value">
        {formatNumber(d.succeeded + d.failed + d.other)} runs
      </span>
      <span>{dayText(d.date, true)}</span>
      <span>
        {formatNumber(d.succeeded)} succeeded · {formatNumber(d.failed)} failed
        {d.other ? ` · ${formatNumber(d.other)} other` : ""}
      </span>
    </div>
  );
}

export default function RunsChart({ days }: { days: AutomationRunDay[] }) {
  const data: Datum[] = days.map((d) => ({
    date: d.date || "",
    succeeded: d.succeeded || 0,
    failed: d.failed || 0,
    other: d.other || 0,
  }));
  return (
    <div className="au-chart">
      <ResponsiveContainer width="100%" height={168}>
        <BarChart data={data} margin={{ top: 8, right: 4, bottom: 0, left: 0 }}>
          <CartesianGrid vertical={false} />
          <XAxis
            dataKey="date"
            tickFormatter={(d) => dayText(String(d))}
            tickLine={false}
            minTickGap={16}
            height={24}
          />
          <YAxis
            allowDecimals={false}
            width={32}
            axisLine={false}
            tickLine={false}
          />
          <Tooltip
            isAnimationActive={false}
            cursor={{ className: "au-chart-cursor" }}
            content={(p) => <Tip active={p.active} payload={p.payload} />}
          />
          <Bar
            dataKey="succeeded"
            stackId="runs"
            className="au-chart-succeeded"
            maxBarSize={28}
            isAnimationActive={false}
          />
          <Bar
            dataKey="failed"
            stackId="runs"
            className="au-chart-failed"
            maxBarSize={28}
            isAnimationActive={false}
          />
          <Bar
            dataKey="other"
            stackId="runs"
            className="au-chart-other"
            maxBarSize={28}
            isAnimationActive={false}
          />
        </BarChart>
      </ResponsiveContainer>
    </div>
  );
}
