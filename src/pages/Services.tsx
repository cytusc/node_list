import { useEffect, useState } from "react";
import {
  Activity,
  ArrowLeft,
  ArrowUpRight,
  CircleAlert,
  ChevronDown,
  Clock3,
  Database,
  Gauge,
  Moon,
  Network,
  RefreshCw,
  Server,
  Sun,
  Workflow,
} from "lucide-react";
import { CartesianGrid, Line, LineChart, XAxis, YAxis } from "recharts";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  ChartContainer,
  ChartTooltip,
  ChartTooltipContent,
  type ChartConfig,
} from "@/components/ui/chart";
import {
  DropdownMenu,
  DropdownMenuTrigger,
  DropdownMenuContent,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
} from "@/components/ui/dropdown-menu";
import { Progress } from "@/components/ui/progress";
import { Skeleton } from "@/components/ui/skeleton";
import {
  percent,
  queueGroups,
  totals,
  type MonitorData,
  type Service,
} from "@/lib/monitor";

const nf = new Intl.NumberFormat("zh-CN");
const number = (v: number | null | undefined) =>
  v == null ? "—" : nf.format(v);
const time = (v: string | null | undefined) =>
  v
    ? new Date(v).toLocaleString("zh-CN", {
        month: "2-digit",
        day: "2-digit",
        hour: "2-digit",
        minute: "2-digit",
        second: "2-digit",
        hour12: false,
      })
    : "—";
const duration = (seconds: number) =>
  seconds < 60
    ? `${Math.round(seconds)} 秒`
    : seconds < 3600
      ? `${Math.round(seconds / 60)} 分钟`
      : `${(seconds / 3600).toFixed(1)} 小时`;
const stateLabel = {
  healthy: "运行正常",
  degraded: "需要关注",
  stale: "心跳超时",
  unknown: "暂无心跳",
};
const queueConfig: ChartConfig = {
  due: { label: "已到期待执行", color: "var(--primary)" },
  scheduled: { label: "未来计划", color: "var(--chart-2)" },
};
const coverageConfig: ChartConfig = {
  connectivity: { label: "有连通性记录", color: "var(--primary)" },
  exit: { label: "有核验出口", color: "var(--chart-2)" },
  attributes: { label: "有 IP 分类", color: "var(--chart-3)" },
};
const runningConfig: ChartConfig = {
  running: { label: "执行中任务", color: "var(--chart-3)" },
};

function StatusBadge({ status }: { status: Service["status"] }) {
  return (
    <Badge
      variant="outline"
      className={
        status === "healthy"
          ? "border-emerald-500/25 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300"
          : status === "unknown"
            ? "text-muted-foreground"
            : "border-amber-500/30 bg-amber-500/10 text-amber-800 dark:text-amber-300"
      }
    >
      <span className="size-1.5 rounded-full bg-current" />
      {stateLabel[status]}
    </Badge>
  );
}
function Meter({
  label,
  value,
  total,
  note,
}: {
  label: string;
  value: number | null;
  total: number | null;
  note: string;
}) {
  const measured = value != null && total != null && total > 0;
  return (
    <div className="space-y-2.5">
      <div className="flex items-center justify-between gap-3 text-sm">
        <span className="font-medium">{label}</span>
        <span className="font-mono text-xs tabular-nums text-muted-foreground">
          {number(value)} / {number(total)}
          {measured ? ` · ${percent(value, total).toFixed(1)}%` : ""}
        </span>
      </div>
      <Progress
        value={measured ? percent(value, total) : 0}
        aria-label={label}
        aria-valuetext={
          measured ? `${number(value)} / ${number(total)}` : "暂无可计算进度"
        }
      />
      <p className="text-xs leading-relaxed text-muted-foreground">{note}</p>
    </div>
  );
}
function Trend({
  title,
  description,
  rows,
  config,
  available,
}: {
  title: string;
  description: string;
  rows: Record<string, unknown>[];
  config: ChartConfig;
  available: boolean;
}) {
  return (
    <Card className="min-w-0">
      <CardHeader>
        <CardTitle className="text-base">{title}</CardTitle>
        <CardDescription>{description}</CardDescription>
      </CardHeader>
      <CardContent>
        {available ? (
          <>
            <ChartContainer
              config={config}
              role="img"
              aria-label={`${title}，每小时均值曲线`}
            >
              <LineChart
                accessibilityLayer
                data={rows}
                margin={{ left: 0, right: 12, top: 8, bottom: 0 }}
              >
                <CartesianGrid vertical={false} strokeDasharray="3 4" />
                <XAxis
                  dataKey="hour"
                  tickFormatter={(v) =>
                    new Date(v).toLocaleString("zh-CN", {
                      month: "2-digit",
                      day: "2-digit",
                      hour: "2-digit",
                      hour12: false,
                    })
                  }
                  minTickGap={55}
                  tickLine={false}
                  axisLine={false}
                  tickMargin={12}
                />
                <YAxis
                  width={48}
                  tickLine={false}
                  axisLine={false}
                  tickFormatter={(v) =>
                    Math.abs(v) >= 1000
                      ? `${(v / 1000).toFixed(1)}k`
                      : String(v)
                  }
                  allowDecimals={false}
                />
                <ChartTooltip
                  content={<ChartTooltipContent />}
                  labelFormatter={(v) => time(String(v))}
                />
                {Object.entries(config).map(([key]) => (
                  <Line
                    key={key}
                    type="linear"
                    dataKey={key}
                    stroke={`var(--color-${key})`}
                    strokeWidth={2}
                    dot={{ r: 2, strokeWidth: 0, fill: `var(--color-${key})` }}
                    activeDot={{ r: 4 }}
                    connectNulls={false}
                    isAnimationActive={false}
                  />
                ))}
              </LineChart>
            </ChartContainer>
            <div className="mt-4 flex flex-wrap gap-x-5 gap-y-2">
              {Object.entries(config).map(([key, item]) => (
                <span
                  key={key}
                  className="flex items-center gap-2 text-xs text-muted-foreground"
                >
                  <span
                    className="h-0.5 w-4 rounded"
                    style={{ background: item.color }}
                  />
                  {item.label}
                </span>
              ))}
            </div>
          </>
        ) : (
          <div className="flex h-64 flex-col items-center justify-center gap-3 text-muted-foreground">
            <Activity className="size-8 opacity-40" />
            <p className="text-sm">尚无历史采样</p>
            <p className="text-xs">
              后台每分钟记录一次，首个样本到达后自动显示。
            </p>
          </div>
        )}
      </CardContent>
    </Card>
  );
}

export default function Services() {
  const [hours, setHours] = useState(24);
  const [group, setGroup] = useState("all");
  const [refresh, setRefresh] = useState(0);
  const [data, setData] = useState<MonitorData | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [dark, setDark] = useState(() => {
    try {
      return localStorage.getItem("node-theme") === "dark";
    } catch {
      return false;
    }
  });
  useEffect(() => {
    document.documentElement.classList.toggle("dark", dark);
    try {
      localStorage.setItem("node-theme", dark ? "dark" : "light");
    } catch {
      /* storage may be disabled */
    }
  }, [dark]);
  useEffect(() => {
    document.title = "服务监控 · Node Explorer";
    let active = true;
    let busy = false;
    let controller: AbortController | null = null;
    async function load() {
      if (busy || document.hidden) return;
      busy = true;
      setLoading(true);
      controller = new AbortController();
      const timeout = window.setTimeout(() => controller?.abort(), 12000);
      try {
        const response = await fetch(`/api/services?hours=${hours}`, {
          signal: controller.signal,
          cache: "no-store",
        });
        if (!response.ok) throw new Error("服务监控暂时无法读取，请稍后重试。");
        const result: MonitorData = await response.json();
        if (!Array.isArray(result.services) || !Array.isArray(result.hourly))
          throw new Error("监控响应格式不正确。");
        if (active) {
          setData(result);
          setError("");
        }
      } catch {
        if (active)
          setError("监控刷新失败，以下保留上次数据。请检查服务连接后重试。");
      } finally {
        window.clearTimeout(timeout);
        busy = false;
        if (active) setLoading(false);
      }
    }
    void load();
    const interval = window.setInterval(() => {
      void load();
    }, 15000);
    const visible = () => {
      if (!document.hidden) void load();
    };
    document.addEventListener("visibilitychange", visible);
    return () => {
      active = false;
      controller?.abort();
      window.clearInterval(interval);
      document.removeEventListener("visibilitychange", visible);
    };
  }, [hours, refresh]);
  const queues = data?.current?.queues ?? [];
  const work = totals(queues);
  const coverage = data?.current?.coverage;
  const healthy =
    data?.services.filter((s) => s.status === "healthy").length ?? 0;
  const history = data?.hours === hours ? data.hourly : [];
  const available = history.some((p) => p.samples > 0);
  const trendRows = history.map((p) => {
    const q =
      p.queues == null
        ? null
        : totals(
            Object.entries(p.queues).map(([kind, value]) => ({
              kind,
              ...value,
              expired: 0,
              oldest_seconds: 0,
            })),
            group,
          );
    return {
      hour: p.hour,
      samples: p.samples,
      due: q?.due ?? null,
      scheduled: q?.scheduled ?? null,
      running: q?.running ?? null,
      connectivity: p.coverage?.connectivity ?? null,
      exit: p.coverage?.exit ?? null,
      attributes: p.coverage?.attributes ?? null,
    };
  });
  const sweep = data?.sweep;
  const phase = sweep?.running
    ? ({ probing: "正在检测", committing: "正在提交", loading: "准备扫描" }[
        sweep.phase ?? ""
      ] ?? "扫描进行中")
    : ({
        completed: "本轮已完成",
        partial: "部分完成，等待补测",
        failed: "本轮失败",
        cancelled: "本轮已取消",
      }[sweep?.outcome ?? ""] ?? "等待下一轮");
  const managerHealthy =
    data?.services.find((s) => s.id === "mihomo-manager")?.status === "healthy";
  const sampleStale =
    data?.sampleStale ||
    (data?.sampledAt != null &&
      Date.now() - Date.parse(data.sampledAt) > 180000);
  return (
    <div className="min-h-screen">
      <header className="site-header">
        <div className="header-inner">
          <a href="/" className="brand" aria-label="节点浏览器首页">
            <span className="brand-mark">
              <Network className="size-5" />
            </span>
            <span>
              Node<span className="brand-light"> Explorer</span>
            </span>
          </a>
          <div className="flex items-center gap-2">
            <Button variant="ghost" size="sm" asChild>
              <a href="/">
                <ArrowLeft className="size-4" />
                <span className="hidden sm:inline">节点列表</span>
              </a>
            </Button>
            <Button
              variant="ghost"
              size="icon"
              onClick={() => setDark((v) => !v)}
              aria-label={dark ? "切换浅色主题" : "切换深色主题"}
            >
              {dark ? <Sun /> : <Moon />}
            </Button>
          </div>
        </div>
      </header>
      <main className="mx-auto w-full max-w-[1496px] space-y-6 px-4 py-8 sm:px-8 lg:px-12">
        <div className="flex flex-wrap items-start justify-between gap-4">
          <div>
            <div className="mb-2 flex items-center gap-2 text-xs font-medium tracking-widest text-muted-foreground">
              <Activity className="size-3.5" />
              运行概览
            </div>
            <h1 className="text-3xl font-semibold tracking-tight">服务监控</h1>
            <p className="mt-2 text-sm text-muted-foreground">
              从采集到维护，查看每一环的运行状态。
            </p>
          </div>
          <div className="flex flex-col items-end gap-2">
            <Button
              variant="outline"
              onClick={() => setRefresh((v) => v + 1)}
              disabled={loading}
            >
              <RefreshCw className={loading ? "animate-spin" : ""} />
              刷新状态
            </Button>
            <p className="text-xs text-muted-foreground">
              每 15 秒刷新 · {data ? time(data.generatedAt) : "连接中"}
            </p>
          </div>
        </div>
        {error && (
          <div
            role="alert"
            className="flex items-center gap-3 rounded-lg border border-destructive/30 bg-destructive/5 p-4 text-sm text-destructive"
          >
            <CircleAlert className="size-4 shrink-0" />
            {data ? error : "暂时无法读取服务监控。请点击刷新状态重试。"}
          </div>
        )}
        {data && sampleStale && (
          <div
            role="status"
            className="flex items-center gap-3 rounded-lg border border-amber-500/30 bg-amber-500/5 p-4 text-sm text-amber-800 dark:text-amber-300"
          >
            <Clock3 className="size-4 shrink-0" />
            {data.sampledAt
              ? `采样已过期（最后采样 ${time(data.sampledAt)}）。队列与覆盖率为历史数据。`
              : "正在等待首个后台样本，队列与覆盖率暂不可用。"}
          </div>
        )}
        {!data ? (
          loading ? (
            <div
              aria-label="正在加载服务监控"
              className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4"
            >
              {[1, 2, 3, 4].map((i) => (
                <Skeleton key={i} className="h-36 rounded-xl" />
              ))}
            </div>
          ) : (
            <Card>
              <CardContent className="flex flex-col items-center gap-3 py-10 text-muted-foreground">
                <Server className="size-10" />
                <p>服务监控暂不可用</p>
                <Button
                  variant="outline"
                  onClick={() => setRefresh((v) => v + 1)}
                >
                  重新连接
                </Button>
              </CardContent>
            </Card>
          )
        ) : (
          <>
            <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
              {[
                {
                  label: "服务心跳",
                  value: `${healthy} / ${data.services.length}`,
                  note:
                    healthy === data.services.length
                      ? "全部服务心跳正常"
                      : "部分服务需要关注",
                  icon: Server,
                },
                {
                  label: "到期待执行",
                  value: data.current ? number(work.due) : "—",
                  note: data.current
                    ? `未来计划 ${number(work.scheduled)} 条，不计入积压`
                    : "等待后台采样",
                  icon: Workflow,
                },
                {
                  label: "执行中任务",
                  value: data.current ? number(work.running) : "—",
                  note: data.current
                    ? `租约过期 ${number(work.expired)} 条`
                    : "等待后台采样",
                  icon: Gauge,
                },
                {
                  label: "当前可用来源节点",
                  value: number(coverage?.total),
                  note: "来源仍有效的注册节点，含待复核节点",
                  icon: Database,
                },
              ].map((item) => (
                <Card key={item.label} className="gap-3 py-5">
                  <CardContent>
                    <div className="flex items-center justify-between text-sm text-muted-foreground">
                      {item.label}
                      <item.icon className="size-4" />
                    </div>
                    <div className="my-3 font-mono text-3xl font-semibold tracking-tight tabular-nums">
                      {item.value}
                    </div>
                    <p className="text-xs text-muted-foreground">{item.note}</p>
                  </CardContent>
                </Card>
              ))}
            </div>
            <section aria-labelledby="service-heading">
              <div className="mb-4 flex items-center justify-between">
                <h2 id="service-heading" className="text-lg font-semibold">
                  服务状态
                </h2>
                <span className="text-xs text-muted-foreground">
                  90 秒心跳窗口
                </span>
              </div>
              <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-4">
                {data.services.map((service) => {
                  const q = queueGroups[service.id]
                    ? totals(queues, service.id)
                    : null;
                  return (
                    <Card key={service.id} className="gap-4 py-5">
                      <CardContent className="space-y-4">
                        <div className="flex flex-wrap items-center justify-between gap-2">
                          <h3 className="font-semibold">{service.name}</h3>
                          <StatusBadge status={service.status} />
                        </div>
                        <p className="text-xs text-muted-foreground">
                          {service.description}
                        </p>
                        {q && data.current ? (
                          <div className="grid grid-cols-3 gap-2 rounded-lg bg-muted/60 p-3">
                            {[
                              ["到期", q.due],
                              ["执行中", q.running],
                              ["计划", q.scheduled],
                            ].map(([label, value]) => (
                              <div key={label}>
                                <div className="text-xs text-muted-foreground">
                                  {label}
                                </div>
                                <div className="mt-1 font-mono font-semibold tabular-nums">
                                  {number(Number(value))}
                                </div>
                              </div>
                            ))}
                          </div>
                        ) : (
                          <div className="rounded-lg bg-muted/60 px-3 py-3 text-xs text-muted-foreground">
                            {service.id === "mihomo-manager"
                              ? `${phase} · 已加载 ${number(data.kernel.loaded)} 个节点`
                              : service.id === "source-ingest"
                                ? "持续采集 · 按各来源周期同步"
                                : service.id === "api"
                                  ? "按请求提供查询服务"
                                  : "等待队列采样"}
                          </div>
                        )}
                        <div className="flex flex-wrap justify-between gap-2 text-xs text-muted-foreground">
                          <span>
                            {service.status === "healthy"
                              ? "最近心跳"
                              : service.message}
                          </span>
                          <time>{time(service.updatedAt)}</time>
                        </div>
                      </CardContent>
                    </Card>
                  );
                })}
              </div>
            </section>
            <div className="grid gap-4 lg:grid-cols-2">
              <Card>
                <CardHeader>
                  <div className="flex items-center justify-between gap-2">
                    <CardTitle className="text-base">扫描与执行进度</CardTitle>
                    <Badge variant="secondary">
                      {managerHealthy ? phase : "manager 状态待确认"}
                    </Badge>
                  </div>
                  <CardDescription>
                    检测进度与落库状态分别显示。
                  </CardDescription>
                </CardHeader>
                <CardContent className="space-y-6">
                  <Meter
                    label={
                      sweep?.kind === "retry"
                        ? "补测轮次"
                        : "当前 / 最近一轮扫描"
                    }
                    value={sweep?.processed ?? null}
                    total={sweep?.candidates ?? null}
                    note={`${number(sweep?.observations)} 条有效观测 · ${number(sweep?.skipped)} 条跳过 · ${number(sweep?.pending)} 条待补测。已处理数量含跳过，提交完成前不代表已落库。`}
                  />
                  <Meter
                    label="出口执行槽占用"
                    value={
                      data.current
                        ? queues
                            .filter((q) => q.kind === "exit")
                            .reduce((n, q) => n + q.running, 0)
                        : null
                    }
                    total={data.kernel.exitCapacity}
                    note="当前出口任务占用 / 配置容量；这是并发占用率，不是积压完成率。"
                  />
                  <div className="flex flex-wrap justify-between gap-2 border-t pt-4 text-xs text-muted-foreground">
                    <span>下一轮 {time(sweep?.next_run_at)}</span>
                    <span>
                      最近重载暂停{" "}
                      {data.kernel.reloadPauseMillis == null
                        ? "—"
                        : `${(data.kernel.reloadPauseMillis / 1000).toFixed(1)} 秒`}
                    </span>
                  </div>
                </CardContent>
              </Card>
              <Card>
                <CardHeader>
                  <CardTitle className="text-base">数据覆盖进度</CardTitle>
                  <CardDescription>
                    当前来源有效的节点中，已有各阶段记录的比例。
                  </CardDescription>
                </CardHeader>
                <CardContent className="space-y-6">
                  <Meter
                    label="连通性检测记录"
                    value={coverage?.connectivity ?? null}
                    total={coverage?.total ?? null}
                    note="包含检测成功与失败记录，不等于可连接节点数量。"
                  />
                  <Meter
                    label="已核验出口"
                    value={coverage?.exit ?? null}
                    total={coverage?.total ?? null}
                    note="最新出口记录具有已核验的 IP。"
                  />
                  <Meter
                    label="已有 IP 分类"
                    value={coverage?.attributes ?? null}
                    total={coverage?.total ?? null}
                    note="已核验出口已关联分类记录；覆盖率不保证记录仍然新鲜。"
                  />
                </CardContent>
              </Card>
            </div>
            <section aria-labelledby="trends-heading" className="space-y-4">
              <div className="flex flex-wrap items-end justify-between gap-4">
                <div>
                  <h2 id="trends-heading" className="text-lg font-semibold">
                    每小时走势
                  </h2>
                  <p className="mt-1 text-xs text-muted-foreground">
                    每分钟采样，曲线展示小时均值。缺失时段留空，当前小时尚未完整。
                  </p>
                </div>
                <div className="flex flex-wrap items-center gap-3">
                  <DropdownMenu>
                    <DropdownMenuTrigger asChild>
                      <Button variant="outline" aria-label="任务范围">
                        {queueGroups[group].label}
                        <ChevronDown className="size-4" />
                      </Button>
                    </DropdownMenuTrigger>
                    <DropdownMenuContent align="end">
                      <DropdownMenuRadioGroup
                        value={group}
                        onValueChange={setGroup}
                      >
                        {Object.entries(queueGroups).map(([key, g]) => (
                          <DropdownMenuRadioItem key={key} value={key}>
                            {g.label}
                          </DropdownMenuRadioItem>
                        ))}
                      </DropdownMenuRadioGroup>
                    </DropdownMenuContent>
                  </DropdownMenu>
                  <div
                    role="group"
                    aria-label="历史时间范围"
                    className="flex gap-1 rounded-lg border bg-card p-1"
                  >
                    {[
                      [24, "24 小时"],
                      [168, "7 天"],
                    ].map(([value, label]) => (
                      <Button
                        key={value}
                        size="sm"
                        variant={hours === value ? "secondary" : "ghost"}
                        aria-pressed={hours === value}
                        onClick={() => setHours(Number(value))}
                      >
                        {label}
                      </Button>
                    ))}
                  </div>
                </div>
              </div>
              <div className="grid gap-4 lg:grid-cols-2">
                <Trend
                  title="队列积压与计划"
                  description={`${queueGroups[group].label} · 到期任务与未来计划分开统计`}
                  rows={trendRows}
                  config={queueConfig}
                  available={available}
                />
                <Trend
                  title="数据覆盖走势"
                  description="全部来源有效节点 · 已有记录数量的小时均值"
                  rows={trendRows}
                  config={coverageConfig}
                  available={available}
                />
              </div>
              <Trend
                title="任务执行走势"
                description={`${queueGroups[group].label} · 每小时平均执行中任务数，不代表每小时完成量`}
                rows={trendRows}
                config={runningConfig}
                available={available}
              />
            </section>
            <Card className="gap-4">
              <CardHeader>
                <CardTitle className="text-base">维护与等待</CardTitle>
              </CardHeader>
              <CardContent className="grid gap-5 sm:grid-cols-3">
                <div>
                  <p className="text-xs text-muted-foreground">
                    最老到期任务等待
                  </p>
                  <p className="mt-2 text-xl font-semibold">
                    {data.current ? duration(work.oldest_seconds) : "—"}
                  </p>
                </div>
                <div>
                  <p className="text-xs text-muted-foreground">
                    进行中的恢复评估
                  </p>
                  <p className="mt-2 text-xl font-semibold">
                    {number(data.current?.recovery.active)}
                  </p>
                </div>
                <div>
                  <p className="text-xs text-muted-foreground">
                    恢复评估超过 10 分钟
                  </p>
                  <p
                    className={`mt-2 text-xl font-semibold ${data.current?.recovery.stalled ? "text-amber-600 dark:text-amber-300" : ""}`}
                  >
                    {number(data.current?.recovery.stalled)}
                  </p>
                </div>
              </CardContent>
            </Card>
            <footer className="flex flex-wrap justify-between gap-2 border-t py-4 text-xs text-muted-foreground">
              <span>
                队列采样 {time(data.sampledAt)} · 历史保留 7 天 ·
                时间按浏览器时区显示
              </span>
              <a
                href="/"
                className="flex items-center gap-1 hover:text-foreground"
              >
                查看节点数据
                <ArrowUpRight className="size-3" />
              </a>
            </footer>
          </>
        )}
      </main>
    </div>
  );
}
