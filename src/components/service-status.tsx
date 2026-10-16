import { ChevronDown } from "lucide-react";
import type { ServiceStatus as Service } from "@/lib/data";

const serviceNames: Record<string, string> = {
  api: "查询与投影",
  "source-ingest": "来源导入",
  "ip-service": "IP 属性",
  connectivity: "连通性检测",
  "residential-recovery": "住宅 IP 恢复",
  "node-lifecycle": "节点状态",
};

const number = new Intl.NumberFormat("zh-CN");

function waitLabel(seconds: number) {
  if (seconds <= 60) return "—";
  return `${number.format(Math.floor(seconds / 60))} 分钟`;
}

export function ServiceStatusPanel({ services }: { services: Service[] }) {
  if (!services.length) return null;
  const healthy = services.filter((service) => service.healthy).length;
  const hasErrors = healthy < services.length;

  return (
    <details className="source-warning service-status" data-alert={hasErrors}>
      <summary className="service-status-summary">
        <span className="service-status-heading">服务状态</span>
        <span className="service-status-signals" aria-hidden="true">
          {services.map((service) => (
            <span key={service.service} data-healthy={service.healthy} />
          ))}
        </span>
        <span className="service-status-overview">
          <strong>
            {healthy}/{services.length}
          </strong>{" "}
          正常
          {hasErrors && (
            <span className="service-status-alert">
              {services.length - healthy} 项异常
            </span>
          )}
        </span>
        <ChevronDown className="service-status-chevron" aria-hidden="true" />
      </summary>
      <div className="service-status-body">
        <div className="service-status-columns" aria-hidden="true">
          <span>服务</span>
          <span>待处理</span>
          <span>执行中</span>
          <span>最长等待</span>
        </div>
        <ul className="service-status-list">
          {services.map((service) => (
            <li
              className="service-status-row"
              key={service.service}
              data-healthy={service.healthy}
            >
              <div className="service-status-identity">
                <span className="service-status-dot" aria-hidden="true" />
                <span className="service-status-name">
                  {serviceNames[service.service] || service.service}
                </span>
                <span className="service-status-health">
                  {service.healthy ? "正常" : "异常"}
                </span>
              </div>
              <dl className="service-status-metrics">
                <div>
                  <dt>待处理</dt>
                  <dd data-active={service.queued > 0}>
                    {number.format(service.queued)}
                  </dd>
                </div>
                <div>
                  <dt>执行中</dt>
                  <dd data-active={service.running > 0}>
                    {number.format(service.running)}
                  </dd>
                </div>
                <div>
                  <dt>最长等待</dt>
                  <dd data-active={service.oldestWaitSeconds > 60}>
                    {waitLabel(service.oldestWaitSeconds)}
                  </dd>
                </div>
              </dl>
              {(service.restored > 0 || service.error) && (
                <div className="service-status-note">
                  {service.restored > 0 && (
                    <span className="service-status-restored">
                      已恢复 {number.format(service.restored)}
                    </span>
                  )}
                  {service.error && (
                    <span className="service-status-error">
                      {service.error}
                    </span>
                  )}
                </div>
              )}
            </li>
          ))}
        </ul>
      </div>
    </details>
  );
}
