export type MediaTag = {
  platform?: string;
  tag?: string;
  Platform?: string;
  Tag?: string;
};
export type IPClassification = {
  version: number;
  ip?: string;
  checkedAt?: string;
  state:
    | "residential"
    | "business"
    | "hosting"
    | "datacenter"
    | "disputed"
    | "unknown";
  types?: {
    provider: string;
    usageType?: string;
    companyType?: string;
  }[];
};
export type IPPureResult = {
  classification?: IPClassification;
  status: "success" | "error" | "unsupported";
  checkedAt: string;
  attempts: number;
  error?: string;
  ip?: string;
  exitIPSource?: string;
  country?: string;
  addressFamily?: 4 | 6;
  failureStage?: string;
  githubReachable?: boolean;
  asn?: string;
  asnOrganization?: string;
  humanPercent?: number | null;
  botPercent?: number | null;
  trafficRatioStatus?: "available" | "no-data" | "query-failed";
  source?: string;
  attributes?: string[];
  score?: number;
  label?: string;
};
type FastCount = { completed: number; success: number; failed: number; abnormal: number };
export type ServiceStatus = {
  fast?: { minute: FastCount; hour: FastCount };
  kernel?: { healthy: boolean; loaded: number; quarantined: number; generation: string; reloads: number; reloadMillis: number; fastRunning: number };
  service: string;
  updatedAt: string;
  healthy: boolean;
  error?: string;
  queued: number;
  scheduled?: number;
  running: number;
  oldestWaitSeconds: number;
  activeAssessments?: number;
  checkedLastHour?: number;
  drainEtaSeconds?: number | null;
  restored: number;
  checked?: number;
  skipped?: number;
};
export type NodeRecord = {
  connectivity?: { checkedAt: string; reachable: boolean; error?: string };
  exit?: {
    ip?: string;
    checkedAt: string;
    version: number;
    verified: boolean;
    error?: string;
  };
  lifecycle?: { status: string; reason?: string; retiredAt?: string };
  ippure?: IPPureResult;
  checkedAt?: string;
  snapshotId?: number;
  speedTest?: boolean;
  mediaCheck?: boolean;
  name: string;
  baseName?: string;
  type: string;
  server: string;
  port: string;
  sni?: string;
  tls: boolean;
  reality?: boolean;
  udp: boolean;
  network: string;
  speed: number;
  country?: string;
  claimedCountry?: string;
  ip?: string;
  ipRisk?: string;
  subTag?: string;
  media?: MediaTag[];
  details?: { k: string; v: string }[];
};
export type HistoryItem = {
  id: number;
  checkedAt?: string;
  importedAt: string;
  nodeCount: number;
};
type FacetOption = { value: string; label: string; count: number };
export type Snapshot = {
  total: number;
  totalNodes: number;
  page: number;
  limit: number;
  summary: { countries: number; maxSpeed: number; median: number };
  facets: {
    country: FacetOption[];
    type: FacetOption[];
    network: FacetOption[];
    features: FacetOption[];
    ipSource: FacetOption[];
    ipAttributes: FacetOption[];
    media: FacetOption[];
  };
  ippureCollector?: {
    enabled: boolean;
    running?: boolean;
    error?: string | null;
  };
  id?: number;
  batchCount?: number;
  sourceWarning?: string | null;
  checkedAt?: string;
  speedTest: boolean;
  mediaCheck: boolean;
  nodes: NodeRecord[];
};
export const platforms: Record<string, string> = {
  openai: "OpenAI",
  netflix: "Netflix",
  disney: "Disney+",
  youtube: "YouTube",
  gemini: "Gemini",
  claude: "Claude",
  spotify: "Spotify",
  tiktok: "TikTok",
};
const regions = new Intl.DisplayNames(["zh-CN"], { type: "region" });
export function countryName(code?: string) {
  if (!code || code === "unknown") return "未知地区";
  try {
    return regions.of(code.toUpperCase()) || code;
  } catch {
    return code;
  }
}
export function plainName(n: NodeRecord) {
  return (
    (n.baseName || n.name).replace(/[\u{1F1E6}-\u{1F1FF}]/gu, "").trim() ||
    n.name
  );
}
export function speedParts(speed: number) {
  return speed > 0
    ? {
        value: speed >= 1024 ? (speed / 1024).toFixed(2) : String(speed),
        unit: speed >= 1024 ? "MB/s" : "KB/s",
      }
    : { value: "—", unit: "" };
}
export function timeLabel(date?: string) {
  return date && Number.isFinite(Date.parse(date))
    ? new Intl.DateTimeFormat("zh-CN", {
        year: "numeric",
        month: "2-digit",
        day: "2-digit",
        hour: "2-digit",
        minute: "2-digit",
        hour12: false,
        timeZone: "Asia/Shanghai",
      }).format(new Date(date))
    : "暂无检测时间";
}
export function mediaPlatform(m: MediaTag) {
  return m.platform || m.Platform || "";
}
export function mediaValue(m: MediaTag) {
  return m.tag || m.Tag || "";
}
export const detailLabels: Record<string, string> = {
  "source-id": "来源 ID",
  latency: "延迟",
  status: "状态",
  "ip-type": "IP 类型",
  isp: "运营商",
  asn: "ASN",
  features: "解锁能力",
  cipher: "加密方式",
  flow: "流控",
  "client-fingerprint": "客户端指纹",
  alpn: "ALPN",
  "skip-cert-verify": "跳过证书验证",
  "ws-path": "WebSocket 路径",
  "ws-host": "WebSocket Host",
  "grpc-service-name": "gRPC 服务名",
  "h2-path": "HTTP/2 路径",
  "http-path": "HTTP 路径",
  plugin: "插件",
  obfs: "混淆",
  protocol: "协议参数",
  up: "上行带宽",
  down: "下行带宽",
  ports: "端口跳跃",
  "congestion-controller": "拥塞控制",
  "udp-relay-mode": "UDP 中继模式",
  version: "版本",
};
