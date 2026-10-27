import { apiFetch } from "@/lib/api";
import { useEffect, useState } from "react";
import { ArrowLeft, Moon, RefreshCw, Sun } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import type { MonitorData, Service } from "@/lib/monitor";

const labels: Record<Service["status"], string> = {
  healthy: "运行正常",
  degraded: "需要关注",
  stale: "心跳超时",
  unknown: "暂无心跳",
};
const time = (stamp: string | null) =>
  stamp ? new Date(stamp).toLocaleString("zh-CN", { hour12: false }) : "—";

export default function Services() {
  const [data, setData] = useState<MonitorData | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [refresh, setRefresh] = useState(0);
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
      /* optional preference */
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
      const request = new AbortController();
      controller = request;
      const timeout = window.setTimeout(() => request.abort(), 12000);
      try {
        const response = await apiFetch("/api/services", {
          signal: request.signal,
          cache: "no-store",
        });
        if (!response.ok) throw new Error("unavailable");
        const result: MonitorData = await response.json();
        if (!Array.isArray(result.services) || !result.liveness)
          throw new Error("invalid response");
        if (active) {
          setData(result);
          setError("");
        }
      } catch {
        if (active) {
          setData(null);
          setError("服务监控暂不可用，请稍后重试。");
        }
      } finally {
        window.clearTimeout(timeout);
        busy = false;
        if (active) setLoading(false);
      }
    }
    void load();
    const interval = window.setInterval(() => void load(), 15000);
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
  }, [refresh]);
  return (
    <main className="mx-auto max-w-6xl space-y-6 p-4 sm:p-8">
      <header className="flex flex-wrap items-center justify-between gap-4">
        <div className="space-y-2">
          <a
            href="/"
            className="inline-flex items-center gap-1 text-sm text-muted-foreground"
          >
            <ArrowLeft className="size-4" />
            节点列表
          </a>
          <h1 className="text-2xl font-semibold">服务监控</h1>
          <p className="text-sm text-muted-foreground">
            当前服务心跳，每 15 秒刷新。
          </p>
        </div>
        <div className="flex gap-2">
          <Button
            variant="outline"
            size="icon"
            aria-label={dark ? "切换浅色主题" : "切换深色主题"}
            onClick={() => setDark(!dark)}
          >
            {dark ? <Sun /> : <Moon />}
          </Button>
          <Button
            variant="outline"
            disabled={loading}
            onClick={() => setRefresh((v) => v + 1)}
          >
            <RefreshCw className={loading ? "animate-spin" : ""} />
            刷新状态
          </Button>
        </div>
      </header>
      {error ? (
        <div role="alert" className="space-y-3 rounded-xl border p-5">
          <p>{error}</p>
          <Button onClick={() => setRefresh((v) => v + 1)}>重新连接</Button>
        </div>
      ) : !data ? (
        <p role="status">正在读取服务状态…</p>
      ) : (
        <>
          <p className="text-xs text-muted-foreground">
            查询时间：{time(data.generatedAt)}
          </p>
          <section
            aria-label="服务状态"
            className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3"
          >
            {data.services.map((service) => (
              <Card key={service.id}>
                <CardHeader>
                  <CardTitle className="flex flex-wrap items-center justify-between gap-2 text-base">
                    {service.name}
                    <Badge variant="outline">{labels[service.status]}</Badge>
                  </CardTitle>
                  <CardDescription>{service.description}</CardDescription>
                </CardHeader>
                <CardContent className="space-y-2 text-sm">
                  <p>{service.message}</p>
                  <p className="text-xs text-muted-foreground">
                    心跳时间：{time(service.updatedAt)}
                  </p>
                </CardContent>
              </Card>
            ))}
          </section>
          <Card>
            <CardHeader>
              <CardTitle>节点测活</CardTitle>
              <CardDescription>最近一次服务心跳报告的执行状态</CardDescription>
            </CardHeader>
            <CardContent className="grid gap-4 sm:grid-cols-3">
              {(
                [
                  ["Xray 待测", data.liveness.xrayQueued],
                  ["Mihomo 待测", data.liveness.mihomoQueued],
                  ["执行中", data.liveness.inFlight],
                ] as const
              ).map(([label, value]) => (
                <div key={label}>
                  <p className="text-sm text-muted-foreground">{label}</p>
                  <p className="text-2xl tabular-nums">
                    {value == null ? "—" : value.toLocaleString("zh-CN")}
                  </p>
                </div>
              ))}
              <p className="text-xs text-muted-foreground sm:col-span-3">
                最近完成：{time(data.liveness.lastCompletedAt)}
              </p>
              <div className="space-y-2 text-sm sm:col-span-3">
                <p className="font-medium">首页节点与隐藏原因（当前数量）</p>
                <p className="text-muted-foreground">
                  {(
                    [
                      ["已展示", "published"],
                      ["尚未标记可用", "unavailable"],
                      ["测活过期或未完成", "expired"],
                      ["测活未验证或配置已变化", "unverified"],
                      ["出口待验证", "unverifiedExit"],
                    ] as const
                  )
                    .map(
                      ([label, key]) =>
                        `${label}：${data.liveness.visibility?.[key]?.toLocaleString("zh-CN") ?? "—"}`,
                    )
                    .join(" · ")}
                </p>
                <p className="font-medium">本次测活服务启动以来</p>
                <p className="text-muted-foreground">
                  {(
                    [
                      ["测活成功", "reachable"],
                      ["测活失败", "unreachable"],
                      ["配置错误", "config_error"],
                      ["探测环境异常", "infrastructure"],
                    ] as const
                  )
                    .map(
                      ([label, key]) =>
                        `${label}：${data.liveness.results?.[key]?.toLocaleString("zh-CN") ?? "—"}`,
                    )
                    .join(" · ")}
                </p>
              </div>
            </CardContent>
          </Card>
        </>
      )}
    </main>
  );
}
