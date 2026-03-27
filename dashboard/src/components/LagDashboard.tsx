import { useState, useEffect, useRef } from "react";
import { gql } from "@apollo/client";
import { useQuery, useSubscription } from "@apollo/client/react";
import {
  AreaChart,
  Area,
  XAxis,
  YAxis,
  CartesianGrid,
  Tooltip,
  ReferenceLine,
  ResponsiveContainer,
  Legend,
} from "recharts";
import { AlertTriangle, TrendingUp, Activity } from "lucide-react";
import type { LagPoint, LagUpdate } from "../types/conduit";

// ── GraphQL ───────────────────────────────────────────────────────────────────

const LAG_HISTORY_QUERY = gql`
  query GetLagHistory(
    $topic: String!
    $consumerGroup: String!
    $durationMinutes: Int!
  ) {
    lagHistory(
      topic: $topic
      consumerGroup: $consumerGroup
      durationMinutes: $durationMinutes
    ) {
      timestamp
      lag
    }
  }
`;

const LAG_SUBSCRIPTION = gql`
  subscription OnLagUpdate {
    lagUpdates {
      topic
      consumerGroup
      totalLag
      partitions {
        partition
        lag
        latestOffset
        committedOffset
      }
      timestamp
      isAlert
    }
  }
`;

const LATEST_LAG_QUERY = gql`
  query GetLatestLag($topic: String!, $consumerGroup: String!) {
    latestLag(topic: $topic, consumerGroup: $consumerGroup)
  }
`;

// ── Types ─────────────────────────────────────────────────────────────────────

interface ChartPoint {
  time: string;
  lag: number;
  timestamp: number;
}

interface Props {
  topic: string;
  consumerGroup: string;
  alertThreshold?: number;
}

// ── Helpers ───────────────────────────────────────────────────────────────────

function formatTime(isoString: string): string {
  const date = new Date(isoString);
  return date.toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

function formatLag(lag: number): string {
  if (lag >= 1_000_000) return `${(lag / 1_000_000).toFixed(1)}M`;
  if (lag >= 1_000) return `${(lag / 1_000).toFixed(1)}K`;
  return lag.toString();
}

const MAX_CHART_POINTS = 360;

// ── Component ─────────────────────────────────────────────────────────────────

export default function LagDashboard({
  topic,
  consumerGroup,
  alertThreshold = 1000,
}: Props) {
  const [chartData, setChartData] = useState<ChartPoint[]>([]);
  const [isAlert, setIsAlert] = useState(false);
  const [currentLag, setCurrentLag] = useState<number>(0);
  const [peakLag, setPeakLag] = useState<number>(0);
  const isFirstLoad = useRef(true);

  const { data: historyData } = useQuery<{ lagHistory: LagPoint[] }>(
    LAG_HISTORY_QUERY,
    {
      variables: { topic, consumerGroup, durationMinutes: 30 },
      fetchPolicy: "network-only",
    }
  );

  const { data: latestLagData } = useQuery<{ latestLag: number }>(
    LATEST_LAG_QUERY,
    { variables: { topic, consumerGroup } }
  );

  useEffect(() => {
    if (latestLagData?.latestLag !== undefined) {
      setCurrentLag(latestLagData.latestLag);
    }
  }, [latestLagData]);

  useEffect(() => {
    if (!historyData?.lagHistory || !isFirstLoad.current) return;
    isFirstLoad.current = false;

    const points: ChartPoint[] = historyData.lagHistory.map((p) => ({
      time: formatTime(p.timestamp),
      lag: p.lag,
      timestamp: new Date(p.timestamp).getTime(),
    }));

    points.sort((a, b) => a.timestamp - b.timestamp);
    setChartData(points);

    const peak = Math.max(...points.map((p) => p.lag), 0);
    setPeakLag(peak);
  }, [historyData]);

  useSubscription<{ lagUpdates: LagUpdate }>(LAG_SUBSCRIPTION, {
    onData: ({
      data,
    }: {
      data: { data?: { lagUpdates?: LagUpdate } };
    }) => {
      const update = data.data?.lagUpdates;
      if (!update || update.topic !== topic) return;

      const newPoint: ChartPoint = {
        time: formatTime(update.timestamp),
        lag: update.totalLag,
        timestamp: new Date(update.timestamp).getTime(),
      };

      setChartData((prev) => {
        const updated = [...prev, newPoint];
        return updated.slice(-MAX_CHART_POINTS);
      });

      setCurrentLag(update.totalLag);
      setIsAlert(update.isAlert);
      setPeakLag((prev) => Math.max(prev, update.totalLag));
    },
  });

  const alertColor = "#ef4444";
  const normalColor = "#6366f1";
  const chartColor = isAlert ? alertColor : normalColor;

  return (
    <div className="bg-slate-900 rounded-xl border border-slate-700 p-4">
      <div className="flex items-center justify-between mb-4">
        <div className="flex items-center gap-2">
          <Activity size={18} className="text-slate-400" />
          <h2 className="text-slate-100 font-semibold text-lg">
            Consumer Lag
          </h2>
          <span className="text-xs font-mono text-slate-500 bg-slate-800 px-2 py-0.5 rounded">
            {topic}
          </span>
        </div>

        {isAlert && (
          <div className="flex items-center gap-1.5 text-red-400 text-sm font-medium animate-pulse">
            <AlertTriangle size={16} />
            <span>Lag Alert — {formatLag(currentLag)} messages behind</span>
          </div>
        )}
      </div>

      <div className="grid grid-cols-3 gap-3 mb-4">
        <SummaryCard
          label="Current Lag"
          value={formatLag(currentLag)}
          isAlert={isAlert}
        />
        <SummaryCard
          label="Peak (session)"
          value={formatLag(peakLag)}
          icon={<TrendingUp size={14} />}
        />
        <SummaryCard
          label="Alert Threshold"
          value={formatLag(alertThreshold)}
          sublabel="messages"
        />
      </div>

      <ResponsiveContainer width="100%" height={280}>
        <AreaChart
          data={chartData}
          margin={{ top: 10, right: 10, left: 0, bottom: 0 }}
        >
          <defs>
            <linearGradient
              id={`lagGradient-${topic}`}
              x1="0"
              y1="0"
              x2="0"
              y2="1"
            >
              <stop offset="5%" stopColor={chartColor} stopOpacity={0.3} />
              <stop offset="95%" stopColor={chartColor} stopOpacity={0.02} />
            </linearGradient>
          </defs>

          <CartesianGrid strokeDasharray="3 3" stroke="#1e293b" />

          <XAxis
            dataKey="time"
            tick={{ fill: "#64748b", fontSize: 11, fontFamily: "monospace" }}
            tickLine={false}
            axisLine={{ stroke: "#1e293b" }}
            interval={Math.floor(chartData.length / 6) || 0}
          />

          <YAxis
            tick={{ fill: "#64748b", fontSize: 11, fontFamily: "monospace" }}
            tickLine={false}
            axisLine={false}
            tickFormatter={(v: number) => formatLag(v)}
            width={50}
          />

          <Tooltip
            contentStyle={{
              background: "#0f172a",
              border: "1px solid #1e293b",
              borderRadius: "8px",
              fontFamily: "monospace",
              fontSize: "12px",
            }}
            labelStyle={{ color: "#94a3b8" }}
            itemStyle={{ color: chartColor }}
            // eslint-disable-next-line @typescript-eslint/no-explicit-any
            formatter={(value: any) => [formatLag(Number(value ?? 0)), "Lag"]}
          />

          <Legend
            formatter={() => `${consumerGroup} lag`}
            wrapperStyle={{ fontSize: "12px", color: "#64748b" }}
          />

          <ReferenceLine
            y={alertThreshold}
            stroke={alertColor}
            strokeDasharray="6 3"
            label={{
              value: `Alert: ${formatLag(alertThreshold)}`,
              fill: alertColor,
              fontSize: 11,
              fontFamily: "monospace",
            }}
          />

          <Area
            type="monotone"
            dataKey="lag"
            stroke={chartColor}
            strokeWidth={2}
            fill={`url(#lagGradient-${topic})`}
            dot={false}
            activeDot={{ r: 4, fill: chartColor }}
            isAnimationActive={false}
          />
        </AreaChart>
      </ResponsiveContainer>

      {chartData.length === 0 && (
        <div className="flex items-center justify-center h-20 text-slate-500 text-sm">
          Waiting for lag data...
        </div>
      )}
    </div>
  );
}

// ── Sub-components ────────────────────────────────────────────────────────────

function SummaryCard({
  label,
  value,
  isAlert,
  sublabel,
  icon,
}: {
  label: string;
  value: string;
  isAlert?: boolean;
  sublabel?: string;
  icon?: React.ReactNode;
}) {
  return (
    <div className="bg-slate-800 rounded-lg p-3 border border-slate-700">
      <div className="text-xs text-slate-500 mb-1 flex items-center gap-1">
        {icon}
        {label}
      </div>
      <div
        className={`text-xl font-mono font-bold ${
          isAlert ? "text-red-400" : "text-slate-100"
        }`}
      >
        {value}
      </div>
      {sublabel && (
        <div className="text-xs text-slate-600 mt-0.5">{sublabel}</div>
      )}
    </div>
  );
}