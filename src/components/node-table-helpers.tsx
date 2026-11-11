import { Globe2 } from "lucide-react";
import { flagUrl } from "@/lib/flags";
import {
  countryName,
  lastCheckedAt,
  speedParts,
  timeLabel,
  type IPPureResult,
  type NodeRecord,
} from "@/lib/data";

export type IPPureMetricName =
  | "asnOrganization"
  | "source"
  | "attributes"
  | "score"
  | "ratio";

export const ippureMetrics: {
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

export const ipTypeProviders: Record<string, string> = {
  ippure: "IPPure",
  ipinfo: "IPinfo",
  "ip-api": "IP-API",
  ipregistry: "IPregistry",
  abuseipdb: "AbuseIPDB",
  ipapi: "ipapi.is",
  ip2location: "IP2Location",
};

export function trafficRatioMessage(status?: IPPureResult["trafficRatioStatus"]) {
  if (status === "query-failed") return "查询失败，稍后重试";
  if (status === "no-data") return "上游暂无统计";
  return "统计数据不足";
}

export function IPTypeDetails({ result }: { result?: IPPureResult }) {
  const classification = result?.classification;
  if (!classification) return null;
  const types = Object.keys(ipTypeProviders).map((provider) => ({
    provider,
    ...classification.types?.find((type) => type.provider === provider),
  }));
  return (
    <section aria-label="IP 类型数据来源">
      <h3>IP 类型属性</h3>
      <div className="overflow-x-auto">
        <table className="w-full text-left text-sm">
          <thead className="text-muted-foreground">
            <tr>
              <th scope="col" className="py-2 pr-3 font-medium">
                数据库
              </th>
              <th scope="col" className="py-2 pr-3 font-medium">
                使用类型
              </th>
              <th scope="col" className="py-2 font-medium">
                公司类型
              </th>
            </tr>
          </thead>
          <tbody>
            {types.map((type) => (
              <tr key={type.provider} className="border-t border-border">
                <th scope="row" className="py-2 pr-3 font-medium">
                  {ipTypeProviders[type.provider] || type.provider}
                </th>
                <td className="py-2 pr-3">
                  {type.usageType ||
                    (type.companyType ? "—" : "未取得有效类型")}
                </td>
                <td className="py-2">{type.companyType || "—"}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <p className="detail-note">
        各数据库独立判断使用类型和公司类型；来源不一致时保留各自结果。未取得有效类型表示尚未采集、请求失败或未返回类型，不参与汇总判断。
      </p>
    </section>
  );
}

export function IPPureMetric({
  result,
  metric,
}: {
  result?: IPPureResult;
  metric: IPPureMetricName;
}) {
  if (metric === "attributes" && result?.classification) {
    const sources = result.classification.types
      ?.map(
        (type) =>
          `${ipTypeProviders[type.provider] || type.provider}：使用类型 ${type.usageType || (type.companyType ? "—" : "未取得有效类型")}；公司类型 ${type.companyType || "—"}`,
      )
      .join("\n");
    return (
      <span title={sources}>
        {result.attributes?.join(" · ") || "类型未知"}
      </span>
    );
  }
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
    return (
      <span title={result.asnOrganization}>
        {result.asnOrganization || "待补采"}
      </span>
    );
  if (metric === "source")
    return <span title={result.source}>{result.source || "—"}</span>;
  if (metric === "attributes")
    return (
      <span title={result.attributes?.join(" · ")}>
        {result.attributes?.join(" · ") || "—"}
      </span>
    );
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
    return (
      <span className="text-muted-foreground">
        {trafficRatioMessage(result.trafficRatioStatus)}
      </span>
    );
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

export function IPPureSummary({ result }: { result?: IPPureResult }) {
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

export function Flag({ code }: { code?: string }) {
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

export function Features({ node }: { node: NodeRecord }) {
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

export function Speed({ value, max }: { value: number; max: number }) {
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

export function NodeTimes({ node }: { node: NodeRecord }) {
  const checkedAt = lastCheckedAt(node);
  const label = (date?: string) =>
    date && Number.isFinite(Date.parse(date)) ? timeLabel(date) : "暂无记录";
  return (
    <div className="node-times">
      <div>
        <span>添加时间</span>
        <time dateTime={node.addedAt}>{label(node.addedAt)}</time>
      </div>
      <div>
        <span>最后检测时间</span>
        <time dateTime={checkedAt}>{label(checkedAt)}</time>
      </div>
    </div>
  );
}
