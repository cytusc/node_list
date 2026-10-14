import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  ArrowDown,
  ArrowUp,
  ArrowUpDown,
  ArrowUpRight,
  Check,
  ChevronLeft,
  ChevronRight,
  CircleAlert,
  Clock3,
  Copy,
  Download as DownloadIcon,
  ExternalLink,
  Globe2,
  Info,
  Layers3,
  ListFilter,
  Loader2,
  Moon,
  Network,
  Radio,
  RefreshCw,
  Search,
  Server,
  ShieldCheck,
  SlidersHorizontal,
  Sun,
  X,
  Zap,
} from "lucide-react";
import {
  flexRender,
  getCoreRowModel,
  useReactTable,
  type ColumnDef,
  type SortingState,
  type VisibilityState,
  type PaginationState,
} from "@tanstack/react-table";
import { toast, Toaster } from "sonner";
import { copyText } from "@/lib/clipboard";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import {
  DropdownMenu,
  DropdownMenuCheckboxItem,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Skeleton } from "@/components/ui/skeleton";
import { FilterMenu } from "@/components/filter-menu";
import {
  countryName,
  detailLabels,
  mediaPlatform,
  mediaValue,
  plainName,
  platforms,
  speedParts,
  timeLabel,
  type Download,
  type NodeRecord,
  type IPPureResult,
  type Snapshot,
} from "@/lib/data";
import { flagUrl } from "@/lib/flags";

type IPPureMetricName =
  "asnOrganization" | "source" | "attributes" | "score" | "ratio";
const ippureMetrics: {
  key: IPPureMetricName;
  label: string;
  column: string;
}[] = [
  { key: "asnOrganization", label: "ASN 组织", column: "asnOrganization" },
  { key: "source", label: "IP 来源", column: "ipSource" },
  { key: "attributes", label: "IP 属性", column: "ipAttributes" },
  { key: "score", label: "IPPure 系数", column: "ippure" },
  { key: "ratio", label: "人机流量比", column: "trafficRatio" },
];
function IPPureMetric({
  result,
  metric,
}: {
  result?: IPPureResult;
  metric: IPPureMetricName;
}) {
  if (result?.status !== "success")
    return (
      <span className="text-muted-foreground" title={result?.error}>
        {result?.status === "unsupported"
          ? result.failureStage === "share-link"
            ? "连接配置不支持"
            : "仅 IPv6，暂无评分"
          : result?.status === "error"
            ? result.ip
              ? "风险查询失败"
              : result.githubReachable
                ? "节点可达，探测受限"
                : result.failureStage === "ippure-query"
                  ? "风险查询失败"
                  : "出口探测失败"
            : "待检测"}
      </span>
    );
  if (metric === "asnOrganization")
    return <span>{result.asnOrganization || "待补采"}</span>;
  if (metric === "source") return <span>{result.source || "—"}</span>;
  if (metric === "attributes")
    return <span>{result.attributes?.join(" · ") || "—"}</span>;
  if (metric === "score")
    return (
      <div className="ippure-metric">
        <span>
          {result.score}% {result.label}
        </span>
        <div
          className="ippure-list-scale"
          role="img"
          aria-label={`IPPure 系数 ${result.score}% ${result.label}，越低越纯净`}
        >
          <span style={{ left: `${result.score}%` }} />
        </div>
      </div>
    );
  if (result.humanPercent == null || result.botPercent == null)
    return <span className="text-muted-foreground">统计数据不足</span>;
  return (
    <div className="ippure-metric">
      <div className="ippure-ratio-values">
        <span>human {result.humanPercent}%</span>
        <span>bot {result.botPercent}%</span>
      </div>
      <div
        className="ippure-ratio-track"
        role="img"
        aria-label={`人机流量比 human ${result.humanPercent}% / bot ${result.botPercent}%`}
      >
        <span style={{ left: `${result.humanPercent}%` }} />
      </div>
    </div>
  );
}
function IPPureSummary({ result }: { result?: IPPureResult }) {
  return (
    <div className="ippure-summary">
      {ippureMetrics.map(({ key, label }) => (
        <div className="ippure-summary-row" key={key}>
          <span>{label}</span>
          <div>
            <IPPureMetric result={result} metric={key} />
          </div>
        </div>
      ))}
    </div>
  );
}

function Flag({ code }: { code?: string }) {
  const url = flagUrl(code);
  return url ? (
    <img
      src={url}
      alt={countryName(code)}
      className="node-flag"
      loading="lazy"
      width="25"
      height="18"
    />
  ) : (
    <Globe2 aria-label="未知地区" className="size-5 text-muted-foreground" />
  );
}
function Features({ node }: { node: NodeRecord }) {
  return (
    <div className="feature-tags">
      {node.reality ? (
        <span className="feature-tag reality">Reality</span>
      ) : (
        node.tls && <span className="feature-tag tls">TLS</span>
      )}
      {node.udp && <span className="feature-tag">UDP</span>}
      {!node.tls && !node.reality && !node.udp && (
        <span className="text-muted-foreground">—</span>
      )}
    </div>
  );
}
function Speed({ value, max }: { value: number; max: number }) {
  const s = speedParts(value);
  return (
    <div className="speed-cell">
      <span className="speed-number">
        {s.value} <small>{s.unit}</small>
      </span>
      {value > 0 && (
        <span className="speed-track" aria-hidden="true">
          <span
            style={{
              width: `${Math.max(2, Math.min(100, (value / (max || 1)) * 100))}%`,
            }}
          />
        </span>
      )}
    </div>
  );
}
async function copy(text: string, source: HTMLElement) {
  try {
    await copyText(text, source);
    toast.success("已复制到剪贴板");
  } catch {
    toast.error("复制失败，请手动选择并复制内容");
  }
}
const columnLabels: Record<string, string> = {
  country: "地区",
  name: "节点 / 服务器",
  type: "协议 / 传输",
  media: "服务解锁",
  speed: "下载速度",
  ...Object.fromEntries(
    ippureMetrics.map(({ column, label }) => [column, label]),
  ),
};
const columnWidths: Record<string, number> = {
  country: 130,
  name: 155,
  type: 130,
  asnOrganization: 160,
  ipSource: 100,
  ipAttributes: 110,
  ippure: 150,
  trafficRatio: 205,
  media: 160,
  speed: 125,
  actions: 44,
};
const emptyNodes: NodeRecord[] = [];

export default function App() {
  const [snapshot, setSnapshot] = useState<Snapshot | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [errorCode, setErrorCode] = useState("");
  const [downloads, setDownloads] = useState<Download[]>([]);
  const [downloading, setDownloading] = useState(false);
  const [query, setQuery] = useState("");
  const [countries, setCountries] = useState<string[]>([]);
  const [types, setTypes] = useState<string[]>([]);
  const [networks, setNetworks] = useState<string[]>([]);
  const [features, setFeatures] = useState<string[]>([]);
  const [media, setMedia] = useState<string[]>([]);
  const [minSpeed, setMinSpeed] = useState("");
  const [selected, setSelected] = useState<NodeRecord | null>(null);
  const [sorting, setSorting] = useState<SortingState>([
    { id: "speed", desc: true },
  ]);
  const [visibility, setVisibility] = useState<VisibilityState>({});
  const [dark, setDark] = useState(() => {
    try {
      return localStorage.getItem("node-theme") === "dark";
    } catch {
      return false;
    }
  });
  const activeRequest = useRef<AbortController | null>(null);
  const nodes = snapshot?.nodes || emptyNodes;
  const criteria = JSON.stringify({
    query,
    countries,
    types,
    networks,
    features,
    media,
    minSpeed,
    sorting,
  });
  const [pageState, setPageState] = useState({
    pageIndex: 0,
    pageSize: 50,
    criteria,
  });
  const pagination: PaginationState = {
    pageIndex: pageState.criteria === criteria ? pageState.pageIndex : 0,
    pageSize: pageState.pageSize,
  };
  const { pageIndex, pageSize } = pagination;
  const params = new URLSearchParams({
    view: "aggregate",
    page: String(pageIndex + 1),
    limit: String(pageSize),
  });
  if (query.trim()) params.set("q", query.trim());
  for (const [key, values] of [
    ["country", countries],
    ["type", types],
    ["network", networks],
    ["features", features],
    ["media", media],
  ] as const) {
    if (values.length) params.set(key, values.join(","));
  }
  if (minSpeed) params.set("minSpeed", minSpeed);
  if (sorting[0]) {
    params.set("sort", sorting[0].id);
    params.set("direction", sorting[0].desc ? "desc" : "asc");
  }
  const resultsUrl = `/api/results?${params}`;
  useEffect(() => {
    document.documentElement.classList.toggle("dark", dark);
    try {
      localStorage.setItem("node-theme", dark ? "dark" : "light");
    } catch {
      /* storage may be unavailable */
    }
  }, [dark]);

  const load = useCallback(
    async (announce = false) => {
      activeRequest.current?.abort();
      const controller = new AbortController();
      activeRequest.current = controller;
      setLoading(true);
      try {
        const response = await fetch(resultsUrl, {
          signal: controller.signal,
        });
        const body = await response.json();
        if (controller.signal.aborted) return;
        if (!response.ok) {
          setErrorCode(body.code || "");
          throw new Error(body.error || "读取失败，请稍后重试");
        }
        if (!Array.isArray(body.nodes))
          throw new Error("结果格式无效，请检查检测结果文件");
        setSnapshot(body);
        setPageState({
          pageIndex: body.page - 1,
          pageSize: body.limit,
          criteria,
        });
        setSelected(null);
        setError("");
        setErrorCode("");
        if (!body.speedTest && sorting[0]?.id === "speed")
          setSorting([{ id: "name", desc: false }]);
        if (!body.mediaCheck) setMedia([]);
        if (!body.speedTest) setMinSpeed("");
        if (announce) toast.success("已刷新聚合检测结果");
      } catch (e) {
        if (!controller.signal.aborted)
          setError(
            e instanceof Error ? e.message : "无法连接本地服务，请稍后重试",
          );
      } finally {
        if (!controller.signal.aborted) setLoading(false);
      }
    },
    [resultsUrl, criteria],
  );
  useEffect(() => {
    const controller = new AbortController();
    void fetch("/api/downloads", { signal: controller.signal })
      .then(async (r) => {
        if (r.ok) setDownloads(await r.json());
      })
      .catch(() => {});
    return () => controller.abort();
  }, []);
  useEffect(() => {
    void load();
    return () => activeRequest.current?.abort();
  }, [load]);

  const maxSpeed = snapshot?.summary.maxSpeed || 0;
  const summary = snapshot?.summary || { countries: 0, median: 0 };
  const totalNodes = snapshot?.totalNodes || 0;
  const total = snapshot?.total || 0;
  const regionOptions = snapshot?.facets.country || [];
  const typeOptions = snapshot?.facets.type || [];
  const networkOptions = snapshot?.facets.network || [];
  const featureOptions = snapshot?.facets.features || [];
  const mediaOptions = (snapshot?.facets.media || []).map((option) => ({
    ...option,
    label: platforms[option.value] || option.value,
  }));
  const reset = () => {
    setQuery("");
    setCountries([]);
    setTypes([]);
    setNetworks([]);
    setFeatures([]);
    setMedia([]);
    setMinSpeed("");
  };
  const activeCount =
    countries.length +
    types.length +
    networks.length +
    features.length +
    media.length +
    Number(!!minSpeed) +
    Number(!!query.trim());
  const columns = useMemo<ColumnDef<NodeRecord>[]>(
    () => [
      {
        accessorKey: "country",
        header: "地区",
        cell: ({ row }) => (
          <div className="region-cell">
            <Flag code={row.original.country} />
            <div>
              <span>{countryName(row.original.country)}</span>
              <small>{row.original.country || "未知"}</small>
            </div>
          </div>
        ),
      },
      {
        id: "name",
        accessorFn: (n) => plainName(n),
        header: "节点 / 服务器",
        enableHiding: false,
        cell: ({ row }) => (
          <div className="identity-cell">
            <button
              className="node-name"
              onClick={(e) => {
                e.stopPropagation();
                setSelected(row.original);
              }}
            >
              {plainName(row.original)}
            </button>
            <span className="node-address">
              {row.original.server}:{row.original.port}
            </span>
          </div>
        ),
      },
      {
        accessorKey: "type",
        header: "协议 / 传输",
        cell: ({ row }) => (
          <div className="protocol-cell">
            <Badge
              variant="outline"
              className={`protocol-badge protocol-${row.original.type}`}
            >
              {row.original.type.toUpperCase()}
            </Badge>
            <small>{row.original.network?.toUpperCase() || "—"}</small>
          </div>
        ),
      },
      ...ippureMetrics.map(({ key, label, column }): ColumnDef<NodeRecord> => ({
        id: column,
        accessorFn: (node) => {
          if (node.ippure?.status !== "success") return undefined;
          return key === "ratio"
            ? (node.ippure.humanPercent ?? undefined)
            : key === "attributes"
              ? node.ippure.attributes?.join(" · ")
              : node.ippure[key];
        },
        header: label,
        sortUndefined: "last",
        cell: ({ row }) => (
          <IPPureMetric result={row.original.ippure} metric={key} />
        ),
      })),
      ...(snapshot?.mediaCheck
        ? [
            {
              id: "media",
              header: "服务解锁",
              enableSorting: false,
              cell: ({ row }: { row: { original: NodeRecord } }) => (
                <div className="feature-tags">
                  {row.original.media
                    ?.filter((m) => mediaValue(m))
                    .map((m, i) => (
                      <Badge key={i} variant="secondary" title={mediaValue(m)}>
                        {platforms[mediaPlatform(m)] || mediaPlatform(m)}
                      </Badge>
                    ))}
                  {!row.original.media?.some(mediaValue) && (
                    <span className="text-muted-foreground">暂无标签</span>
                  )}
                </div>
              ),
            },
          ]
        : []),
      ...(snapshot?.speedTest
        ? [
            {
              accessorKey: "speed",
              header: "下载速度",
              cell: ({ row }: { row: { original: NodeRecord } }) => (
                <Speed value={row.original.speed} max={maxSpeed} />
              ),
            },
          ]
        : []),
      {
        id: "actions",
        header: "",
        enableSorting: false,
        enableHiding: false,
        cell: ({ row }) => (
          <Button
            variant="ghost"
            size="icon"
            className="detail-button"
            aria-label={`查看 ${plainName(row.original)} 详情`}
            onClick={(e) => {
              e.stopPropagation();
              setSelected(row.original);
            }}
          >
            <ArrowUpRight className="size-4" />
          </Button>
        ),
      },
    ],
    [maxSpeed, snapshot?.speedTest, snapshot?.mediaCheck],
  );
  const table = useReactTable({
    data: nodes,
    columns,
    state: { sorting, columnVisibility: visibility, pagination },
    manualPagination: true,
    manualSorting: true,
    rowCount: total,
    onPaginationChange: (updater) => {
      const next =
        typeof updater === "function" ? updater(pagination) : updater;
      setPageState({
        ...next,
        pageIndex: next.pageSize !== pageSize ? 0 : next.pageIndex,
        criteria,
      });
    },
    onSortingChange: setSorting,
    onColumnVisibilityChange: setVisibility,
    getCoreRowModel: getCoreRowModel(),
  });

  async function downloadFile(item: Download) {
    setDownloading(true);
    try {
      const res = await fetch(`/api/download/${item.id}`);
      if (!res.ok) {
        const body = await res.json();
        throw new Error(body.error || "下载失败");
      }
      const url = URL.createObjectURL(await res.blob());
      const a = document.createElement("a");
      a.href = url;
      a.download =
        item.id === "clash"
          ? "all.yaml"
          : item.id === "mihomo"
            ? "mihomo.yaml"
            : "base64.txt";
      a.click();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
      toast.success(`${item.label}已下载`);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "下载失败，请检查本地服务");
    } finally {
      setDownloading(false);
    }
  }

  return (
    <div className="app-shell">
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
          <div className="header-right">
            <span className="local-label">
              <span />
              本地节点库
            </span>
            <Button
              variant="ghost"
              size="icon"
              onClick={() => setDark((v) => !v)}
              aria-label={dark ? "切换浅色主题" : "切换深色主题"}
            >
              {dark ? <Sun className="size-4" /> : <Moon className="size-4" />}
            </Button>
            <a
              className="source-link"
              href="https://github.com/beck-8/subs-check"
              target="_blank"
              rel="noreferrer"
            >
              subs-check
              <ExternalLink className="size-3.5" />
            </a>
          </div>
        </div>
      </header>
      <main className="main-content">
        <section className="intro">
          <div className="intro-top">
            <div>
              <h1>
                节点浏览器
                <span className="title-count">
                  {snapshot ? totalNodes : "—"}
                </span>
              </h1>
              <p>找到适合你的连接。</p>
            </div>
            <div className="snapshot-time">
              <Clock3 className="size-3.5" />
              <span>
                最近检测{" "}
                <time>
                  {snapshot?.checkedAt ? timeLabel(snapshot.checkedAt) : "—"}
                </time>
                <small>北京时间</small>
              </span>
            </div>
          </div>
          <div className="summary-line">
            <span>
              <Globe2 />
              覆盖 <strong>{snapshot ? summary.countries : "—"}</strong> 个地区
            </span>
            <i />
            {snapshot?.speedTest ? (
              <>
                <span>
                  <Zap />
                  最高速度 <strong>{speedParts(maxSpeed).value}</strong>{" "}
                  {speedParts(maxSpeed).unit}
                </span>
                <i />
                <span className="median-stat">
                  中位速度 <strong>{speedParts(summary.median).value}</strong>{" "}
                  {speedParts(summary.median).unit}
                </span>
              </>
            ) : (
              <span>测速{snapshot ? "未开启" : "数据加载中"}</span>
            )}
            <span className="summary-note">
              <Info className="size-3.5" />
              {snapshot?.mediaCheck
                ? "已启用服务解锁检测"
                : snapshot
                  ? "未启用服务解锁检测"
                  : "正在读取检测结果"}
            </span>
          </div>
        </section>

        <div className="mb-4 flex flex-wrap items-center gap-3 text-sm text-muted-foreground">
          <span>
            已聚合 {snapshot?.batchCount ?? 0}{" "}
            轮检测，同一节点保留最近一次结果。
          </span>
          <span>历史节点不代表当前仍可用，检测时间可在详情中查看。</span>
          <span>订阅下载仍使用最新生成的文件。</span>
        </div>
        {snapshot?.sourceWarning && !error && (
          <div role="status" className="error-banner">
            <CircleAlert className="size-5 shrink-0" />
            结果文件暂时无法读取，当前展示数据库中保存的检测结果。
          </div>
        )}
        {error && (
          <div role="alert" className="error-banner">
            <CircleAlert className="size-5 shrink-0" />
            <div>
              <strong>
                {snapshot
                  ? "刷新失败，当前保留上次读取的结果"
                  : errorCode === "NO_RESULTS"
                    ? "尚无检测结果"
                    : "无法读取检测结果"}
              </strong>
              <p>{error}</p>
            </div>
            <Button
              variant="outline"
              size="sm"
              disabled={loading}
              onClick={() => void load()}
            >
              重试
            </Button>
          </div>
        )}

        <section className="results-panel" aria-label="节点筛选与结果">
          <div className="filter-section">
            <div className="filter-row">
              <span className="filter-caption">
                <ListFilter className="size-4" />
                筛选
              </span>
              <FilterMenu
                label="协议"
                icon={<Layers3 className="size-4" />}
                options={typeOptions}
                selected={types}
                onChange={setTypes}
              />
              <FilterMenu
                label="地区"
                icon={<Globe2 className="size-4" />}
                options={regionOptions}
                selected={countries}
                onChange={setCountries}
              />
              <FilterMenu
                label="传输方式"
                icon={<Network className="size-4" />}
                options={networkOptions}
                selected={networks}
                onChange={setNetworks}
              />
              <FilterMenu
                label="连接特性"
                icon={<ShieldCheck className="size-4" />}
                options={featureOptions}
                selected={features}
                onChange={setFeatures}
              />
              {snapshot?.mediaCheck && (
                <FilterMenu
                  label="服务解锁"
                  icon={<Radio className="size-4" />}
                  options={mediaOptions}
                  selected={media}
                  onChange={setMedia}
                />
              )}
              {snapshot?.speedTest && (
                <label className="speed-filter">
                  <Zap className="size-3.5" />
                  <input
                    type="number"
                    min="0"
                    step="0.1"
                    aria-label="最低速度，MB/s"
                    placeholder="最低速度"
                    value={minSpeed}
                    onChange={(e) => setMinSpeed(e.target.value)}
                  />
                  <span>MB/s</span>
                </label>
              )}
            </div>
            <div className="search-row">
              <div className="search-input">
                <Search className="size-4" />
                <Input
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                  placeholder="搜索节点名称、服务器、SNI…"
                  aria-label="搜索节点"
                />
                {query && (
                  <button aria-label="清除搜索" onClick={() => setQuery("")}>
                    <X className="size-3.5" />
                  </button>
                )}
              </div>
              <div className="sort-control">
                <ArrowUpDown className="size-3.5" />
                <select
                  aria-label="排序方式"
                  value={`${sorting[0]?.id || "name"}-${sorting[0]?.desc ? "desc" : "asc"}`}
                  onChange={(e) => {
                    const [id, direction] = e.target.value.split("-");
                    setSorting([{ id, desc: direction === "desc" }]);
                  }}
                >
                  {snapshot?.speedTest && (
                    <>
                      <option value="speed-desc">速度最高</option>
                      <option value="speed-asc">速度最低</option>
                    </>
                  )}
                  <option value="name-asc">名称 A–Z</option>
                  <option value="name-desc">名称 Z–A</option>
                  <option value="country-asc">地区 A–Z</option>
                  <option value="country-desc">地区 Z–A</option>
                  <option value="type-asc">协议 A–Z</option>
                  <option value="type-desc">协议 Z–A</option>
                </select>
              </div>
              {activeCount > 0 && (
                <Button variant="ghost" size="sm" onClick={reset}>
                  <X className="size-3.5" />
                  清空筛选
                </Button>
              )}
            </div>
            {activeCount > 0 && (
              <div className="active-filters">
                {[
                  { values: countries, set: setCountries, label: countryName },
                  {
                    values: types,
                    set: setTypes,
                    label: (v: string) => v.toUpperCase(),
                  },
                  {
                    values: networks,
                    set: setNetworks,
                    label: (v: string) => v.toUpperCase(),
                  },
                  {
                    values: features,
                    set: setFeatures,
                    label: (v: string) => v.toUpperCase(),
                  },
                  {
                    values: media,
                    set: setMedia,
                    label: (v: string) => platforms[v] || v,
                  },
                ].flatMap((group, i) =>
                  group.values.map((v) => (
                    <button
                      key={`${i}-${v}`}
                      onClick={() =>
                        group.set(group.values.filter((x) => x !== v))
                      }
                      aria-label={`移除筛选 ${group.label(v)}`}
                    >
                      {group.label(v)}
                      <X className="size-3" />
                    </button>
                  )),
                )}
                {minSpeed && (
                  <button onClick={() => setMinSpeed("")}>
                    ≥ {minSpeed} MB/s
                    <X className="size-3" />
                  </button>
                )}
              </div>
            )}
          </div>
          <div className="table-toolbar">
            <div className="result-count">
              <span className="result-dot" />
              <strong>{total}</strong>
              <span>个节点</span>
              {activeCount > 0 && (
                <span className="total-count">/ 共 {totalNodes} 个</span>
              )}
            </div>
            <div className="table-actions">
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="ghost" size="sm" className="column-button">
                    <SlidersHorizontal className="size-3.5" />
                    <span>显示列</span>
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  <DropdownMenuLabel>表格显示列</DropdownMenuLabel>
                  <DropdownMenuSeparator />
                  {table
                    .getAllLeafColumns()
                    .filter((c) => c.getCanHide())
                    .map((c) => (
                      <DropdownMenuCheckboxItem
                        key={c.id}
                        checked={c.getIsVisible()}
                        onCheckedChange={(value) => c.toggleVisibility(!!value)}
                      >
                        {columnLabels[c.id] || c.id}
                      </DropdownMenuCheckboxItem>
                    ))}
                </DropdownMenuContent>
              </DropdownMenu>
              <Button
                variant="outline"
                size="sm"
                onClick={() => void load(true)}
                disabled={loading}
                aria-label="刷新结果"
              >
                <RefreshCw
                  className={`size-3.5 ${loading ? "animate-spin" : ""}`}
                />
                <span>刷新</span>
              </Button>
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button size="sm" disabled={downloading}>
                    <DownloadIcon className="size-3.5" />
                    <span>{downloading ? "下载中" : "下载订阅"}</span>
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end" className="w-72">
                  <DropdownMenuLabel>下载完整订阅</DropdownMenuLabel>
                  <p className="px-2 pb-2 text-xs leading-relaxed text-muted-foreground">
                    包含文件中的全部节点，不受当前筛选影响。
                  </p>
                  <DropdownMenuSeparator />
                  {downloads.length ? (
                    downloads.map((d) => (
                      <DropdownMenuItem
                        key={d.id}
                        disabled={!d.available}
                        onClick={() => void downloadFile(d)}
                      >
                        <DownloadIcon className="size-4" />
                        <div>
                          <div>
                            {d.label}
                            {!d.available && " · 尚未生成"}
                          </div>
                          {d.updatedAt && (
                            <small className="text-muted-foreground">
                              文件更新于 {timeLabel(d.updatedAt)}
                            </small>
                          )}
                        </div>
                      </DropdownMenuItem>
                    ))
                  ) : (
                    <DropdownMenuItem disabled>
                      订阅文件信息暂不可用
                    </DropdownMenuItem>
                  )}
                </DropdownMenuContent>
              </DropdownMenu>
            </div>
          </div>

          {loading ? (
            <div
              className="loading-skeleton"
              aria-label="正在加载节点"
              role="status"
            >
              {Array.from({ length: 8 }, (_, i) => (
                <div key={i}>
                  <Skeleton className="size-8 rounded-md" />
                  <Skeleton className="h-5 w-1/3" />
                  <Skeleton className="ml-auto h-5 w-24" />
                </div>
              ))}
            </div>
          ) : nodes.length ? (
            <>
              <div className="desktop-table">
                <Table
                  style={{
                    minWidth: table
                      .getVisibleLeafColumns()
                      .reduce(
                        (sum, column) => sum + (columnWidths[column.id] || 150),
                        0,
                      ),
                  }}
                >
                  <TableHeader>
                    {table.getHeaderGroups().map((g) => (
                      <TableRow key={g.id}>
                        {g.headers.map((h) => (
                          <TableHead
                            key={h.id}
                            data-column={h.column.id}
                            style={{ width: columnWidths[h.column.id] || 150 }}
                          >
                            {h.column.getCanSort() ? (
                              <button
                                className="sort-header"
                                onClick={() =>
                                  h.column.toggleSorting(
                                    h.column.getIsSorted() === "asc",
                                  )
                                }
                                aria-label={`按${columnLabels[h.column.id]}排序`}
                              >
                                {flexRender(
                                  h.column.columnDef.header,
                                  h.getContext(),
                                )}
                                {h.column.getIsSorted() === "desc" ? (
                                  <ArrowDown className="size-3.5" />
                                ) : h.column.getIsSorted() === "asc" ? (
                                  <ArrowUp className="size-3.5" />
                                ) : (
                                  <ArrowUpDown className="size-3 opacity-30" />
                                )}
                              </button>
                            ) : (
                              flexRender(
                                h.column.columnDef.header,
                                h.getContext(),
                              )
                            )}
                          </TableHead>
                        ))}
                      </TableRow>
                    ))}
                  </TableHeader>
                  <TableBody>
                    {table.getRowModel().rows.map((row) => (
                      <TableRow
                        key={row.id}
                        className="node-row"
                        onClick={() => setSelected(row.original)}
                      >
                        {row.getVisibleCells().map((c) => (
                          <TableCell key={c.id} data-column={c.column.id}>
                            {flexRender(
                              c.column.columnDef.cell,
                              c.getContext(),
                            )}
                          </TableCell>
                        ))}
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>
              <div className="mobile-nodes">
                {table.getRowModel().rows.map((row) => (
                  <button
                    key={row.id}
                    className="mobile-node"
                    onClick={() => setSelected(row.original)}
                  >
                    <div className="mobile-node-top">
                      <Flag code={row.original.country} />
                      <span>{plainName(row.original)}</span>
                      <ArrowUpRight className="ml-auto size-4 text-muted-foreground" />
                    </div>
                    <span className="mobile-address">
                      {row.original.server}:{row.original.port}
                    </span>
                    <IPPureSummary result={row.original.ippure} />
                    <div className="mobile-node-bottom">
                      <div>
                        <Badge variant="outline" className="protocol-badge">
                          {row.original.type.toUpperCase()}
                        </Badge>
                        <span className="text-xs text-muted-foreground">
                          {countryName(row.original.country)}
                        </span>
                      </div>
                      {snapshot?.speedTest && (
                        <Speed value={row.original.speed} max={maxSpeed} />
                      )}
                    </div>
                  </button>
                ))}
              </div>
            </>
          ) : (
            <div className="empty-state">
              <span>
                <Search className="size-7" />
              </span>
              <h2>
                {error && !snapshot
                  ? "等待检测结果"
                  : totalNodes
                    ? "没有匹配的节点"
                    : snapshot?.checkedAt
                      ? "暂无保留节点"
                      : "尚无检测结果"}
              </h2>
              <p>
                {error && !snapshot
                  ? "连接恢复后，点击刷新重新读取。"
                  : totalNodes
                    ? "尝试减少筛选条件，或换一个关键词。"
                    : "请在 subs-check 完成检测后刷新此页面。"}
              </p>
              {activeCount > 0 && (
                <Button variant="outline" onClick={reset}>
                  清空筛选
                </Button>
              )}
            </div>
          )}
          <div className="pagination-bar">
            <span>
              {total
                ? `显示 ${((snapshot?.page || 1) - 1) * pageSize + 1}–${Math.min((snapshot?.page || 1) * pageSize, total)}，共 ${total} 个`
                : "显示 0 个节点"}
            </span>
            <div>
              <label className="page-size">
                每页
                <select
                  aria-label="每页节点数"
                  value={pageSize}
                  onChange={(e) => table.setPageSize(Number(e.target.value))}
                >
                  {[20, 50, 100].map((n) => (
                    <option key={n} value={n}>
                      {n}
                    </option>
                  ))}
                </select>
                个
              </label>
              <span className="page-indicator">
                {pageIndex + 1} / {Math.max(1, table.getPageCount())}
              </span>
              <Button
                variant="outline"
                size="icon"
                className="size-8"
                disabled={loading || !table.getCanPreviousPage()}
                onClick={() => table.previousPage()}
                aria-label="上一页"
              >
                <ChevronLeft className="size-4" />
              </Button>
              <Button
                variant="outline"
                size="icon"
                className="size-8"
                disabled={loading || !table.getCanNextPage()}
                onClick={() => table.nextPage()}
                aria-label="下一页"
              >
                <ChevronRight className="size-4" />
              </Button>
            </div>
          </div>
        </section>
        <footer className="page-footer">
          <span>
            <Server className="size-3.5" />由 subs-check 提供检测结果
          </span>
          <span>速度为所选批次检测值，实际表现可能变化。</span>
        </footer>
      </main>
      <Sheet
        open={!!selected}
        onOpenChange={(open) => {
          if (!open) setSelected(null);
        }}
      >
        <SheetContent className="node-sheet w-full sm:max-w-lg">
          {selected && (
            <>
              <SheetHeader>
                <div className="detail-region">
                  <Flag code={selected.country} />
                  {countryName(selected.country)}
                  {selected.claimedCountry &&
                    selected.country &&
                    selected.claimedCountry.toUpperCase() !==
                      selected.country.toUpperCase() && (
                      <Badge
                        variant="outline"
                        className="border-amber-500 text-amber-600"
                      >
                        来源标注 {countryName(selected.claimedCountry)}
                      </Badge>
                    )}
                </div>
                <SheetTitle className="break-all text-xl">
                  {plainName(selected)}
                </SheetTitle>
                <SheetDescription>
                  最近检测 · {timeLabel(selected.checkedAt)}
                </SheetDescription>
              </SheetHeader>
              <div className="detail-content">
                <div className="detail-overview">
                  <Badge variant="outline" className="protocol-badge">
                    {selected.type.toUpperCase()}
                  </Badge>
                  <Features node={selected} />
                  {(selected.speedTest ?? snapshot?.speedTest) && (
                    <Speed value={selected.speed} max={maxSpeed} />
                  )}
                </div>
                <h3>连接信息</h3>
                <dl>
                  {[
                    ["完整名称", selected.name],
                    ["服务器", selected.server],
                    ["端口", selected.port],
                    ["传输方式", selected.network],
                    ["SNI", selected.sni],
                    ["出口 IP", selected.ippure?.ip],
                    ["上游检测 IP", selected.ip],
                    ["来源标签", selected.subTag],
                    ["IP 风险", selected.ipRisk],
                  ]
                    .filter(([, v]) => v)
                    .map(([label, value]) => (
                      <div className="detail-field" key={label}>
                        <dt>{label}</dt>
                        <dd>
                          {value}
                          <Button
                            variant="ghost"
                            size="icon"
                            className="size-6 shrink-0"
                            aria-label={`复制${label}`}
                            onClick={(event) =>
                              void copy(value!, event.currentTarget)
                            }
                          >
                            <Copy className="size-3" />
                          </Button>
                        </dd>
                      </div>
                    ))}
                </dl>
                <p className="detail-note">
                  <Info className="size-3.5" />
                  TLS、UDP 等标签表示节点配置特性。
                </p>
                {!!selected.details?.length && (
                  <>
                    <h3>协议参数</h3>
                    <dl>
                      {selected.details.map((d, i) => (
                        <div className="detail-field" key={`${d.k}-${i}`}>
                          <dt>{detailLabels[d.k] || d.k}</dt>
                          <dd>{d.v}</dd>
                        </div>
                      ))}
                    </dl>
                  </>
                )}
                <h3>IPPure 检测</h3>
                {selected.ippure?.ip && (
                  <dl>
                    <div className="detail-field">
                      <dt>检测出口 IP</dt>
                      <dd>{selected.ippure.ip}</dd>
                    </div>
                    {selected.ippure.exitIPSource && (
                      <div className="detail-field">
                        <dt>出口查询服务</dt>
                        <dd>{selected.ippure.exitIPSource}</dd>
                      </div>
                    )}
                  </dl>
                )}
                {selected.ippure?.status === "success" ? (
                  <>
                    <dl>
                      {[
                        ["ASN", selected.ippure.asn || "待补采"],
                        [
                          "ASN 组织",
                          selected.ippure.asnOrganization || "待补采",
                        ],
                        [
                          "出口国家",
                          selected.ippure.country
                            ? `${countryName(selected.ippure.country)}（${selected.ippure.country.toUpperCase()}）`
                            : "待补采",
                        ],
                        [
                          "人机流量比",
                          selected.ippure.humanPercent == null
                            ? "统计数据不足"
                            : `human ${selected.ippure.humanPercent}% / bot ${selected.ippure.botPercent}%`,
                        ],
                        ["IP 来源", selected.ippure.source],
                        ["IP 属性", selected.ippure.attributes?.join(" · ")],
                        [
                          "IPPure 系数",
                          `${selected.ippure.score}% ${selected.ippure.label}`,
                        ],
                        ["检测时间", timeLabel(selected.ippure.checkedAt)],
                      ].map(([label, value]) => (
                        <div className="detail-field" key={label}>
                          <dt>{label}</dt>
                          <dd>{value}</dd>
                        </div>
                      ))}
                    </dl>
                    <div
                      className="ippure-scale"
                      aria-label={`IPPure 系数 ${selected.ippure.score}%`}
                    >
                      <div className="ippure-track">
                        <span style={{ left: `${selected.ippure.score}%` }} />
                      </div>
                      <div className="ippure-ticks">
                        {[0, 15, 25, 40, 50, 70, 100].map((n) => (
                          <span key={n} style={{ left: `${n}%` }}>
                            {n}
                          </span>
                        ))}
                      </div>
                    </div>
                    <p className="detail-note">
                      系数越低越纯净；人机流量比为 IPPure 提供的 ASN
                      流量统计。按该节点访问出口查询服务得到的 IP 查询，IPPure
                      请求可经其他可用节点转发。
                    </p>
                  </>
                ) : (
                  <p className="detail-note">
                    {selected.ippure?.status === "unsupported"
                      ? selected.ippure.error
                      : selected.ippure?.status === "error"
                        ? `${selected.ippure.error}（已尝试 ${selected.ippure.attempts} 次，最多 3 次）`
                        : snapshot?.ippureCollector?.error ||
                          (snapshot?.ippureCollector?.enabled === false
                            ? "IPPure 检测服务未启用，请配置独立代理检测服务。"
                            : "尚无 IPPure 结果；检测服务会依次检测当前及历史节点，刷新页面可查看进度。同一连接复用最近的出口检测结果。")}
                  </p>
                )}
                <h3>服务解锁</h3>
                {(selected.mediaCheck ?? snapshot?.mediaCheck) ? (
                  selected.media?.length ? (
                    <dl>
                      {selected.media.map((m, i) => (
                        <div className="detail-field" key={i}>
                          <dt>
                            {platforms[mediaPlatform(m)] || mediaPlatform(m)}
                          </dt>
                          <dd>{mediaValue(m) || "未返回解锁标签"}</dd>
                        </div>
                      ))}
                    </dl>
                  ) : (
                    <p className="detail-note">该节点没有服务解锁记录。</p>
                  )
                ) : (
                  <p className="detail-note">未启用服务解锁检测。</p>
                )}
              </div>
              <div className="detail-footer">
                <Button
                  className="w-full"
                  onClick={(event) =>
                    void copy(
                      `${selected.server}:${selected.port}`,
                      event.currentTarget,
                    )
                  }
                >
                  <Copy className="size-4" />
                  复制服务器地址
                </Button>
              </div>
            </>
          )}
        </SheetContent>
      </Sheet>
      <Toaster
        position="bottom-right"
        theme={dark ? "dark" : "light"}
        richColors
        closeButton
      />
    </div>
  );
}
