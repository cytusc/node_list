import { apiFetch } from "@/lib/api";
import { useDebouncedValue } from "@/lib/use-debounced-value";
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
  Copy,
  ExternalLink,
  Globe2,
  Layers3,
  ListFilter,
  Loader2,
  Moon,
  Network,
  Radio,
  RefreshCw,
  Search,
  ShieldCheck,
  SlidersHorizontal,
  Sun,
  Tag,
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
import { CopyNodeButton } from "@/components/copy-node-button";
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
  DropdownMenu,
  DropdownMenuCheckboxItem,
  DropdownMenuContent,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Skeleton } from "@/components/ui/skeleton";
import { FilterMenu } from "@/components/filter-menu";
import { NodeDetailSheet } from "@/components/node-detail-sheet";
import {
  Flag,
  Features,
  Speed,
  NodeTimes,
  IPPureSummary,
  IPPureMetric,
  ippureMetrics,
  type IPPureMetricName,
} from "@/components/node-table-helpers";
import {
  countryName,
  mediaPlatform,
  mediaValue,
  plainName,
  protocolLabel,
  lastCheckedAt,
  platforms,
  speedParts,
  timeLabel,
  type NodeRecord,
  type IPPureResult,
  type NodeResults,
} from "@/lib/data";

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
  time: "时间",
  copy: "复制",
  ...Object.fromEntries(
    ippureMetrics.map(({ column, label }) => [column, label]),
  ),
};
const columnWidths: Record<string, number> = {
  country: 100,
  name: 155,
  type: 98,
  asnOrganization: 128,
  ipSource: 80,
  ipAttributes: 95,
  ippure: 100,
  trafficRatio: 120,
  media: 120,
  speed: 100,
  time: 150,
  copy: 48,
};
const emptyNodes: NodeRecord[] = [];

export default function App() {
  const [results, setResults] = useState<NodeResults | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [query, setQuery] = useState("");
  const [countries, setCountries] = useState<string[]>([]);
  const [types, setTypes] = useState<string[]>([]);
  const [networks, setNetworks] = useState<string[]>([]);
  const [sources, setSources] = useState<string[]>([]);
  const [ipSources, setIpSources] = useState<string[]>([]);
  const [ipAttributes, setIpAttributes] = useState<string[]>([]);
  const [media, setMedia] = useState<string[]>([]);
  const [minSpeed, setMinSpeed] = useState("");
  const debouncedQuery = useDebouncedValue(query);
  const debouncedMinSpeed = useDebouncedValue(minSpeed);
  const [selected, setSelected] = useState<NodeRecord | null>(null);
  const [sorting, setSorting] = useState<SortingState>([
    { id: "preferred", desc: true },
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
  const requestPending = useRef(false);
  const pollFailures = useRef(0);
  const nextPollAt = useRef(0);
  const nodes = results?.nodes || emptyNodes;
  const criteria = JSON.stringify({
    query: debouncedQuery,
    countries,
    types,
    networks,
    sources,
    ipSources,
    ipAttributes,
    media,
    minSpeed: debouncedMinSpeed,
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
    page: String(pageIndex + 1),
    limit: String(pageSize),
  });
  if (debouncedQuery.trim()) params.set("q", debouncedQuery.trim());
  for (const [key, values] of [
    ["country", countries],
    ["type", types],
    ["network", networks],
    ["source", sources],
    ["ipSource", ipSources],
    ["ipAttributes", ipAttributes],
    ["media", media],
  ] as const) {
    if (values.length) params.set(key, values.join(","));
  }
  if (debouncedMinSpeed) params.set("minSpeed", debouncedMinSpeed);
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
    async (announce = false, background = false) => {
      if (
        background &&
        (document.hidden ||
          requestPending.current ||
          Date.now() < nextPollAt.current)
      )
        return;
      requestPending.current = true;
      activeRequest.current?.abort();
      const controller = new AbortController();
      activeRequest.current = controller;
      if (!background) setLoading(true);
      try {
        const response = await apiFetch(resultsUrl, {
          cache: "no-store",
          signal: controller.signal,
        });
        const body = await response.json();
        if (controller.signal.aborted) return;
        if (!response.ok) {
          throw new Error(body.error || "读取失败，请稍后重试");
        }
        if (
          !body ||
          !Array.isArray(body.nodes) ||
          !body.nodes.every(
            (node: unknown) =>
              node !== null && typeof node === "object" && !Array.isArray(node),
          ) ||
          !body.summary ||
          !body.facets ||
          ![
            "country",
            "type",
            "network",
            "source",
            "ipSource",
            "ipAttributes",
            "media",
          ].every((key) => Array.isArray(body.facets[key])) ||
          !Number.isSafeInteger(body.page) ||
          body.page < 1 ||
          !Number.isSafeInteger(body.limit) ||
          body.limit < 1
        )
          throw new Error("接口返回的节点数据格式无效");
        pollFailures.current = 0;
        nextPollAt.current = 0;
        setResults((previous) =>
          JSON.stringify(previous) === JSON.stringify(body) ? previous : body,
        );
        setPageState((previous) => {
          const next = {
            pageIndex: body.page - 1,
            pageSize: body.limit,
            criteria,
          };
          return previous.pageIndex === next.pageIndex &&
            previous.pageSize === next.pageSize &&
            previous.criteria === criteria
            ? previous
            : next;
        });
        if (!background) setSelected(null);
        setError("");
        if (!body.speedTest && sorting[0]?.id === "speed")
          setSorting([{ id: "name", desc: false }]);
        if (!body.mediaCheck)
          setMedia((previous) => (previous.length ? [] : previous));
        if (!body.speedTest) setMinSpeed("");
        if (announce) toast.success("已刷新可用节点");
      } catch (e) {
        if (!controller.signal.aborted) {
          pollFailures.current = Math.min(pollFailures.current + 1, 4);
          nextPollAt.current =
            Date.now() + Math.min(5000 * 2 ** pollFailures.current, 60000);
          if (!background) {
            setResults(null);
            setSelected(null);
          }
          setError(e instanceof Error ? e.message : "无法连接服务，请稍后重试");
        }
      } finally {
        if (activeRequest.current === controller) {
          requestPending.current = false;
          setLoading(false);
        }
      }
    },
    [resultsUrl, criteria],
  );
  useEffect(() => {
    void load();
    const timer = window.setInterval(() => void load(false, true), 5000);
    return () => {
      window.clearInterval(timer);
      activeRequest.current?.abort();
    };
  }, [load]);

  const maxSpeed = results?.summary.maxSpeed || 0;
  const summary = results?.summary || { countries: 0, median: 0 };
  const totalNodes = results?.totalNodes || 0;
  const total = results?.total || 0;
  const regionOptions = results?.facets.country || [];
  const typeOptions = results?.facets.type || [];
  const networkOptions = results?.facets.network || [];
  const sourceOptions = results?.facets.source || [];
  const ipSourceOptions = results?.facets.ipSource || [];
  const ipAttributeOptions = results?.facets.ipAttributes || [];
  const mediaOptions = (results?.facets.media || []).map((option) => ({
    ...option,
    label: platforms[option.value] || option.value,
  }));
  const reset = () => {
    setQuery("");
    setCountries([]);
    setTypes([]);
    setNetworks([]);
    setSources([]);
    setIpSources([]);
    setIpAttributes([]);
    setMedia([]);
    setMinSpeed("");
  };
  const activeCount =
    countries.length +
    types.length +
    networks.length +
    sources.length +
    ipSources.length +
    ipAttributes.length +
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
              {protocolLabel(row.original.type)}
            </Badge>
            <small>{row.original.network?.toUpperCase() || "—"}</small>
          </div>
        ),
      },
      ...ippureMetrics.map(({ key, label, column }): ColumnDef<NodeRecord> => ({
        id: column,
        accessorFn: (node) => {
          if (key === "attributes" && node.ippure?.classification)
            return node.ippure.attributes?.join(" · ");
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
      ...(results?.mediaCheck
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
      ...(results?.speedTest
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
        id: "time",
        header: "时间",
        enableSorting: false,
        cell: ({ row }) => <NodeTimes node={row.original} />,
      },
      {
        id: "copy",
        header: "复制",
        enableSorting: false,
        enableHiding: false,
        cell: ({ row }) => <CopyNodeButton node={row.original} />,
      },
    ],
    [maxSpeed, results?.speedTest, results?.mediaCheck],
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

  return (
    <div className="app-shell">
      <header className="site-header">
        <div className="header-inner">
          <a
            href="/"
            className="brand"
            aria-label="Node Explorer 节点浏览器首页"
          >
            <span className="brand-mark">
              <Network className="size-5" />
            </span>
            <span>
              Node<span className="brand-light"> Explorer</span>
            </span>
          </a>
          <div className="header-right">
            <Button variant="ghost" size="sm" asChild>
              <a href="/services">
                <Radio className="size-4" />
                服务监控
              </a>
            </Button>
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
                  {results ? totalNodes : "—"}
                </span>
              </h1>
              <p>实时展示数据库中的可用节点。</p>
            </div>
          </div>
          <div className="summary-line">
            <span>
              <Globe2 />
              覆盖 <strong>{results ? summary.countries : "—"}</strong> 个地区
            </span>
            {results?.speedTest ? (
              <>
                <i />
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
            ) : null}
          </div>
        </section>

        {error && (
          <div role="alert" className="error-banner">
            <CircleAlert className="size-5 shrink-0" />
            <div>
              <strong>{results ? "节点刷新失败" : "无法读取可用节点"}</strong>
              <p>{error}</p>
              {results && <p>当前展示上次成功查询的数据，可能已发生变化。</p>}
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
                label="来源"
                icon={<Tag className="size-4" />}
                options={sourceOptions}
                selected={sources}
                onChange={setSources}
              />
              <FilterMenu
                label="IP 来源"
                icon={<Globe2 className="size-4" />}
                options={ipSourceOptions}
                selected={ipSources}
                onChange={setIpSources}
              />
              <FilterMenu
                label="IP 属性"
                icon={<ShieldCheck className="size-4" />}
                options={ipAttributeOptions}
                selected={ipAttributes}
                onChange={setIpAttributes}
              />
              {results?.mediaCheck && (
                <FilterMenu
                  label="服务解锁"
                  icon={<Radio className="size-4" />}
                  options={mediaOptions}
                  selected={media}
                  onChange={setMedia}
                />
              )}
              {results?.speedTest && (
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
                  <option value="preferred-desc">住宅 IP 优先</option>
                  {results?.speedTest && (
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
                  <option value="source-asc">来源 A–Z</option>
                  <option value="source-desc">来源 Z–A</option>
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
                    values: sources,
                    set: setSources,
                    label: (v: string) => v,
                  },
                  {
                    values: ipSources,
                    set: setIpSources,
                    label: (v: string) => v,
                  },
                  {
                    values: ipAttributes,
                    set: setIpAttributes,
                    label: (v: string) => v,
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
                            style={{
                              width:
                                h.column.id === "name"
                                  ? undefined
                                  : columnWidths[h.column.id] || 150,
                            }}
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
                    {table.getColumn("time")?.getIsVisible() && (
                      <NodeTimes node={row.original} />
                    )}
                    <div className="mobile-node-bottom">
                      <div>
                        <Badge variant="outline" className="protocol-badge">
                          {protocolLabel(row.original.type)}
                        </Badge>
                        <span className="text-xs text-muted-foreground">
                          {countryName(row.original.country)}
                        </span>
                      </div>
                      {results?.speedTest && (
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
                {error && !results
                  ? "暂时无法加载节点"
                  : totalNodes
                    ? "没有匹配的节点"
                    : "暂无可用节点"}
              </h2>
              <p>
                {error && !results
                  ? "连接恢复后，点击刷新重新读取。"
                  : totalNodes
                    ? "尝试减少筛选条件，或换一个关键词。"
                    : "数据库中暂无可用节点，请稍后刷新。"}
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
                ? `显示 ${((results?.page || 1) - 1) * pageSize + 1}–${Math.min((results?.page || 1) * pageSize, total)}，共 ${total} 个`
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
      </main>
      <NodeDetailSheet
        selected={selected}
        onClose={() => setSelected(null)}
        maxSpeed={maxSpeed}
        speedTest={results?.speedTest}
        mediaCheck={results?.mediaCheck}
        onCopy={(text, trigger) => void copy(text, trigger)}
      />
      <Toaster
        position="bottom-right"
        theme={dark ? "dark" : "light"}
        richColors
        closeButton
      />
    </div>
  );
}
