import * as React from "react";
import { ResponsiveContainer, Tooltip } from "recharts";
import { cn } from "@/lib/utils";

// shadcn chart composition: theme-aware Recharts inside a reusable container.
export type ChartConfig = Record<string, { label: string; color: string }>;
const ChartContext = React.createContext<ChartConfig>({});
export function ChartContainer({
  config,
  children,
  className,
  ...props
}: React.ComponentProps<"div"> & {
  config: ChartConfig;
  children: React.ComponentProps<typeof ResponsiveContainer>["children"];
}) {
  const variables = Object.fromEntries(
    Object.entries(config).map(([key, item]) => [`--color-${key}`, item.color]),
  ) as React.CSSProperties;
  return (
    <ChartContext.Provider value={config}>
      <div
        data-slot="chart"
        className={cn(
          "h-64 min-w-0 text-xs [&_.recharts-cartesian-axis-tick_text]:fill-muted-foreground [&_.recharts-cartesian-grid_line]:stroke-border [&_.recharts-curve.recharts-tooltip-cursor]:stroke-border",
          className,
        )}
        style={variables}
        {...props}
      >
        <ResponsiveContainer width="100%" height="100%" minWidth={0}>
          {children}
        </ResponsiveContainer>
      </div>
    </ChartContext.Provider>
  );
}
export const ChartTooltip = Tooltip;
export function ChartTooltipContent({
  active,
  payload,
  label,
}: {
  active?: boolean;
  payload?: readonly {
    name?: string | number;
    value?: string | number | (string | number)[];
    dataKey?: string | number;
    color?: string;
    payload?: { samples?: number };
  }[];
  label?: React.ReactNode;
}) {
  const config = React.useContext(ChartContext);
  if (!active || !payload?.length) return null;
  return (
    <div className="min-w-40 rounded-lg border bg-popover p-3 text-xs text-popover-foreground shadow-lg">
      <div className="mb-2 font-medium">{label}</div>
      {payload.map((item) => (
        <div
          key={String(item.dataKey)}
          className="flex items-center gap-2 py-1"
        >
          <span
            className="size-2 shrink-0 rounded-sm"
            style={{ background: item.color }}
          />
          <span className="text-muted-foreground">
            {config[String(item.dataKey)]?.label ?? item.name}
          </span>
          <span className="ml-auto font-mono font-medium tabular-nums">
            {Number(item.value).toLocaleString("zh-CN", {
              maximumFractionDigits: 1,
            })}
          </span>
        </div>
      ))}
      <div className="mt-2 border-t pt-2 text-muted-foreground">
        {payload[0]?.payload?.samples ?? 0} 个分钟样本 · 小时均值
      </div>
    </div>
  );
}
