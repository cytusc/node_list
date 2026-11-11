import { Info, Copy } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { CopyNodeButton } from "@/components/copy-node-button";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import {
  Flag,
  Features,
  Speed,
  IPTypeDetails,
  trafficRatioMessage,
} from "@/components/node-table-helpers";
import {
  countryName,
  detailLabels,
  lastCheckedAt,
  mediaPlatform,
  mediaValue,
  plainName,
  platforms,
  protocolLabel,
  timeLabel,
  type NodeRecord,
} from "@/lib/data";

interface NodeDetailSheetProps {
  selected: NodeRecord | null;
  onClose: () => void;
  maxSpeed: number;
  speedTest?: boolean;
  mediaCheck?: boolean;
  onCopy: (text: string, trigger: HTMLElement) => void;
}

export function NodeDetailSheet({
  selected,
  onClose,
  maxSpeed,
  speedTest,
  mediaCheck,
  onCopy,
}: NodeDetailSheetProps) {
  return (
    <Sheet
      open={!!selected}
      onOpenChange={(open) => {
        if (!open) onClose();
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
                最近检测 · {timeLabel(lastCheckedAt(selected))}
              </SheetDescription>
            </SheetHeader>
            <div className="detail-content">
              <div className="detail-overview">
                <Badge variant="outline" className="protocol-badge">
                  {protocolLabel(selected.type)}
                </Badge>
                <Features node={selected} />
                {(selected.speedTest ?? speedTest) && (
                  <Speed value={selected.speed} max={maxSpeed} />
                )}
              </div>
              <CopyNodeButton node={selected} showLabel />
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
                            onCopy(value!, event.currentTarget)
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
              <h3>节点检查</h3>
              <dl>
                {[
                  [
                    "连通性",
                    selected.connectivity
                      ? selected.connectivity.reachable
                        ? "可用"
                        : selected.connectivity.error || "不可用"
                      : "待检查",
                  ],
                  ["测活时间", timeLabel(selected.connectivity?.checkedAt)],
                  ["出口检查时间", timeLabel(selected.exit?.checkedAt)],
                  [
                    "IP 属性时间",
                    timeLabel(selected.ippure?.classification?.checkedAt),
                  ],
                ].map(([label, value]) => (
                  <div className="detail-field" key={label}>
                    <dt>{label}</dt>
                    <dd>{value}</dd>
                  </div>
                ))}
              </dl>
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
                          ? trafficRatioMessage(
                              selected.ippure.trafficRatioStatus,
                            )
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
                      ? selected.ippure.error
                      : "暂无 IP 属性数据。"}
                </p>
              )}
              <IPTypeDetails result={selected.ippure} />
              <h3>服务解锁</h3>
              {(selected.mediaCheck ?? mediaCheck) ? (
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
          </>
        )}
      </SheetContent>
    </Sheet>
  );
}
