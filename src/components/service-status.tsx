import { Fragment, useEffect, useState } from "react";
import {
  Activity,
  AlertTriangle,
  Check,
  ChevronDown,
  ChevronRight,
  Clock3,
  Info,
  RefreshCw,
} from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@/components/ui/collapsible";
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import type { ServiceStatus as Service } from "@/lib/data";

const serviceNames: Record<string, string> = {
  "mihomo-manager": "共享测活内核",
  api: "查询与投影",
  "source-ingest": "来源导入",
  "ip-service": "IP 属性",
  connectivity: "连通性检测",
  "residential-recovery": "住宅 IP 恢复",
  "normal-recovery": "非住宅节点恢复",
  "node-cleanup": "失效节点清理",
  "node-lifecycle": "节点状态",
};
const serviceOrder = [
  "mihomo-manager",
  "connectivity",
  "ip-service",
  "residential-recovery",
  "normal-recovery",
  "node-lifecycle",
  "node-cleanup",
  "source-ingest",
  "api",
];
const number = new Intl.NumberFormat("zh-CN");
const time = new Intl.DateTimeFormat("zh-CN", {
  hour: "2-digit",
  minute: "2-digit",
  second: "2-digit",
  hour12: false,
});
const isRecovery = (s: Service) =>
  s.service === "residential-recovery" || s.service === "normal-recovery";
function waitLabel(seconds: number) {
  if (seconds <= 0) return "—";
  if (seconds < 60) return `${Math.max(1, Math.floor(seconds))} 秒`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)} 分钟`;
  return `${number.format(Math.floor(seconds / 3600))} 小时`;
}
function warnings(service: Service) {
  const result: string[] = [];
  if (service.kernel?.quarantined)
    result.push(
      `${number.format(service.kernel.quarantined)} 个节点配置被隔离，暂不能检测`,
    );
  if (service.blocked)
    result.push(
      `${number.format(service.blocked)} 个任务因节点配置被拒绝而延后重试`,
    );
  if (service.expired)
    result.push(
      `${number.format(service.expired)} 个任务执行超时，等待重新领取`,
    );
  if (service.kernel && !service.kernel.healthy && !service.error)
    result.push("检测内核尚未就绪");
  return result;
}
const hasWorkQueue = (s: Service) =>
  !["api", "source-ingest", "mihomo-manager"].includes(s.service);
function Metric({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="service-detail-metric">
      <dt>{label}</dt>
      <dd>{value}</dd>
    </div>
  );
}
function ServiceDetails({ service }: { service: Service }) {
  return (
    <div className="service-detail-content">
      {isRecovery(service) && (
        <>
          <dl className="service-detail-metrics">
            <Metric
              label="核验中会话"
              value={number.format(service.activeAssessments ?? 0)}
            />
            <Metric
              label="近 1 小时核验"
              value={`${number.format(service.checkedLastHour ?? 0)} 次`}
            />
            <Metric
              label="已记录核验"
              value={`${number.format(service.checked ?? 0)} 次`}
            />
            <Metric
              label="机房 ASN 跳过"
              value={`${number.format(service.skipped ?? 0)} 次`}
            />
          </dl>
          <p className="service-detail-caption">
            核验中包含等待前置检测的会话。次数包含失败和配置异常结果，机房跳过单列。早期未留存记录不计入。
          </p>
          {(service.drainEtaSeconds ?? 0) > 0 && (
            <p className="service-detail-caption">
              按近一小时吞吐估算，到期任务约{" "}
              {waitLabel(service.drainEtaSeconds ?? 0)}{" "}
              处理完；新增和重试会影响耗时。
            </p>
          )}
        </>
      )}
      {service.kernel && (
        <dl className="service-detail-metrics">
          <Metric
            label="已加载节点"
            value={number.format(service.kernel.loaded)}
          />
          <Metric
            label="隔离节点"
            value={number.format(service.kernel.quarantined)}
          />
          <Metric
            label="配置代次"
            value={service.kernel.generation.split(":").at(-1)}
          />
          <Metric
            label="本次启动后重载"
            value={`${number.format(service.kernel.reloads)} 次`}
          />
          <Metric
            label="最近重载耗时"
            value={`${(service.kernel.reloadMillis / 1000).toFixed(1)} 秒`}
          />
        </dl>
      )}
      {service.fast && (
        <div className="service-probe-results">
          <p className="service-detail-caption">快速测活结果</p>
          <Table aria-label="快速测活结果">
            <TableHeader>
              <TableRow>
                <TableHead>统计窗口</TableHead>
                <TableHead>完成</TableHead>
                <TableHead>成功</TableHead>
                <TableHead>失败</TableHead>
                <TableHead>异常</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {(
                [
                  ["上一完整分钟", service.fast.minute],
                  ["最近 60 个分钟桶", service.fast.hour],
                ] as const
              ).map(([label, count]) => (
                <TableRow key={label}>
                  <TableCell>{label}</TableCell>
                  <TableCell>{number.format(count.completed)}</TableCell>
                  <TableCell className="service-good-text">
                    {number.format(count.success)}
                  </TableCell>
                  <TableCell>{number.format(count.failed)}</TableCell>
                  <TableCell>{number.format(count.abnormal)}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          <p className="service-detail-caption">
            小时统计包含当前分钟，窗口长度为 59–60
            分钟。节点检测失败与服务运行异常分别统计。
          </p>
        </div>
      )}
      {service.service === "node-lifecycle" && (
        <dl className="service-detail-metrics">
          <Metric
            label="累计恢复次数"
            value={number.format(service.restored)}
          />
          <Metric
            label="涉及不同节点"
            value={number.format(service.restoredNodes ?? 0)}
          />
        </dl>
      )}
      <dl className="service-detail-metrics service-detail-secondary">
        {hasWorkQueue(service) && (
          <Metric
            label="定时等待"
            value={number.format(service.scheduled ?? 0)}
          />
        )}
        {(service.cooldown ?? 0) > 0 && (
          <Metric
            label="其中冷却等待"
            value={number.format(service.cooldown ?? 0)}
          />
        )}
        {hasWorkQueue(service) && (
          <Metric
            label="最长到期等待"
            value={waitLabel(service.oldestWaitSeconds)}
          />
        )}
        <Metric
          label="最近服务心跳"
          value={
            Number.isFinite(Date.parse(service.updatedAt)) &&
            !service.updatedAt.startsWith("0001-")
              ? time.format(new Date(service.updatedAt))
              : "尚未上报"
          }
        />
      </dl>
    </div>
  );
}

export function ServiceStatusPanel({
  services,
  observedAt,
  error = "",
  loading = false,
  onRefresh,
}: {
  services: Service[];
  observedAt?: string | null;
  error?: string;
  loading?: boolean;
  onRefresh?: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [expanded, setExpanded] = useState<Set<string>>(() => new Set());
  const [now, setNow] = useState(Date.now);
  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 5000);
    return () => window.clearInterval(timer);
  }, []);
  const snapshotTime = observedAt ? Date.parse(observedAt) : NaN;
  const stale =
    Boolean(error) ||
    !Number.isFinite(snapshotTime) ||
    now - snapshotTime > 45000;
  const stateOf = (service: Service) => {
    if (stale) return "stale";
    if (
      !service.healthy ||
      !Number.isFinite(Date.parse(service.updatedAt)) ||
      now - Date.parse(service.updatedAt) > 90000 ||
      service.kernel?.healthy === false
    )
      return "error";
    return warnings(service).length ? "warning" : "healthy";
  };
  const counts = services.reduce(
    (out, service) => {
      out[stateOf(service)]++;
      return out;
    },
    { healthy: 0, warning: 0, error: 0, stale: 0 },
  );
  const ordered = [...services].sort(
    (a, b) => serviceOrder.indexOf(a.service) - serviceOrder.indexOf(b.service),
  );
  const hasSnapshot = services.length > 0;
  const toggle = (name: string) =>
    setExpanded((old) => {
      const next = new Set(old);
      if (next.has(name)) next.delete(name);
      else next.add(name);
      return next;
    });
  return (
    <Collapsible
      open={open}
      onOpenChange={setOpen}
      className="service-status"
      data-alert={stale || counts.warning + counts.error > 0}
    >
      <div className="service-status-header">
        <CollapsibleTrigger asChild>
          <Button
            variant="ghost"
            className="service-status-summary"
            aria-label={open ? "收起服务状态" : "展开服务状态"}
          >
            <Activity className="size-4 text-muted-foreground" />
            <span className="service-status-heading">服务状态</span>
            <ChevronDown className="service-status-chevron" data-open={open} />
          </Button>
        </CollapsibleTrigger>
        <div className="service-status-overview" aria-live="polite">
          {!hasSnapshot ? (
            loading ? (
              <span className="text-muted-foreground">正在读取</span>
            ) : (
              <Badge variant="outline" className="service-tone-stale">
                暂不可用
              </Badge>
            )
          ) : stale ? (
            <Badge variant="outline" className="service-tone-stale">
              <Clock3 />
              数据待更新
            </Badge>
          ) : (
            <>
              <Badge variant="outline" className="service-tone-healthy">
                <Check />
                {counts.healthy} 项运行
              </Badge>
              {counts.warning > 0 && (
                <Badge variant="outline" className="service-tone-warning">
                  <AlertTriangle />
                  {counts.warning} 项告警
                </Badge>
              )}
              {counts.error > 0 && (
                <Badge variant="outline" className="service-tone-error">
                  {counts.error} 项异常
                </Badge>
              )}
            </>
          )}
        </div>
        <span className="service-status-updated">
          {Number.isFinite(snapshotTime)
            ? `${time.format(new Date(snapshotTime))} 更新`
            : "等待首次更新"}
        </span>
        <Button
          variant="ghost"
          size="icon-sm"
          aria-label="刷新服务状态"
          onClick={onRefresh}
          disabled={loading}
          className="service-refresh"
        >
          <RefreshCw
            className={loading ? "service-refreshing size-3.5" : "size-3.5"}
          />
        </Button>
      </div>
      {error && (
        <p className="service-status-notice" role="status">
          <AlertTriangle className="size-3.5" />
          {error}
          {!hasSnapshot && "，请点击刷新重试"}
        </p>
      )}
      {!error && stale && hasSnapshot && (
        <p className="service-status-notice" role="status">
          <Clock3 className="size-3.5" />
          已超过 45 秒未取得新数据，以下为上次记录。
        </p>
      )}
      <CollapsibleContent className="service-status-body">
        <div className="service-status-context">
          <span>
            {hasSnapshot ? `${services.length} 项服务` : "服务运行概况"}
          </span>
          <Popover>
            <PopoverTrigger asChild>
              <Button
                variant="ghost"
                size="xs"
                className="text-muted-foreground"
              >
                <Info className="size-3.5" />
                指标说明
              </Button>
            </PopoverTrigger>
            <PopoverContent
              align="end"
              className="w-80 text-xs leading-relaxed"
            >
              <p className="mb-2 font-medium">任务与服务分别统计</p>
              <p>
                到期待处理：已经到执行时间的排队任务。定时等待：尚未到期的任务，包含恢复冷却。
              </p>
              <p className="mt-2">
                执行中：持有有效执行租约的任务；共享内核展示实际快速检测数。核验中会话另列在恢复服务详情。
              </p>
              <p className="mt-2">
                运行表示服务心跳正常。节点配置隔离、任务配置被拒绝或执行超时会单独告警。
              </p>
            </PopoverContent>
          </Popover>
        </div>
        {!hasSnapshot ? (
          <div className="service-status-empty">
            {loading ? (
              <>
                <Skeleton className="h-8 w-full" />
                <Skeleton className="h-8 w-full" />
                <Skeleton className="h-8 w-full" />
              </>
            ) : (
              "尚未取得服务状态，请刷新重试。"
            )}
          </div>
        ) : (
          <Table className="service-status-table" aria-label="服务运行状态">
            <TableHeader>
              <TableRow>
                <TableHead>服务</TableHead>
                <TableHead>运行状态</TableHead>
                <TableHead className="service-numeric">
                  <span className="hidden sm:inline">到期</span>待处理
                </TableHead>
                <TableHead className="service-numeric service-wide-cell">
                  定时等待
                </TableHead>
                <TableHead className="service-numeric">执行中</TableHead>
                <TableHead className="service-numeric service-wide-cell">
                  最长等待
                </TableHead>
                <TableHead className="w-10">
                  <span className="sr-only">详情</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {ordered.map((service) => {
                const state = stateOf(service);
                const name = serviceNames[service.service] || service.service;
                const notes = [
                  ...(service.error ? [service.error] : []),
                  ...warnings(service),
                ];
                if (state === "error" && !notes.length)
                  notes.push("服务心跳已过期");
                const showDetails = expanded.has(service.service);
                const running = service.kernel
                  ? service.kernel.fastRunning
                  : service.running;
                return (
                  <Fragment key={service.service}>
                    <TableRow
                      className="service-status-row"
                      data-health={state}
                    >
                      <TableCell className="service-identity-cell">
                        <span className="service-status-name">{name}</span>
                        {notes.length > 0 && (
                          <p className="service-row-warning">
                            {notes.join("；")}
                          </p>
                        )}
                      </TableCell>
                      <TableCell>
                        <Badge
                          variant="outline"
                          className={`service-tone-${state}`}
                        >
                          <span className="service-status-dot" />
                          {
                            {
                              healthy: "运行",
                              warning: "告警",
                              error: "异常",
                              stale: "待更新",
                            }[state]
                          }
                        </Badge>
                      </TableCell>
                      <TableCell className="service-numeric">
                        {hasWorkQueue(service)
                          ? number.format(service.queued)
                          : "—"}
                      </TableCell>
                      <TableCell className="service-numeric service-wide-cell">
                        {hasWorkQueue(service)
                          ? number.format(service.scheduled ?? 0)
                          : "—"}
                      </TableCell>
                      <TableCell className="service-numeric">
                        {hasWorkQueue(service) || service.kernel
                          ? number.format(running)
                          : "—"}
                      </TableCell>
                      <TableCell className="service-numeric service-wide-cell">
                        {waitLabel(service.oldestWaitSeconds)}
                      </TableCell>
                      <TableCell>
                        <Button
                          variant="ghost"
                          size="icon-xs"
                          aria-label={`${showDetails ? "收起" : "查看"}${name}详情`}
                          aria-expanded={showDetails}
                          aria-controls={`service-detail-${service.service}`}
                          onClick={() => toggle(service.service)}
                        >
                          <ChevronRight
                            className="service-detail-chevron"
                            data-open={showDetails}
                          />
                        </Button>
                      </TableCell>
                    </TableRow>
                    {showDetails && (
                      <TableRow className="service-detail-row">
                        <TableCell
                          colSpan={7}
                          id={`service-detail-${service.service}`}
                        >
                          <ServiceDetails service={service} />
                        </TableCell>
                      </TableRow>
                    )}
                  </Fragment>
                );
              })}
            </TableBody>
          </Table>
        )}
        <div className="service-status-footer">
          <span className="service-status-dot" />每 15 秒刷新
          <span className="service-footer-hint">
            告警与异常可在对应服务行查看
          </span>
        </div>
      </CollapsibleContent>
    </Collapsible>
  );
}
