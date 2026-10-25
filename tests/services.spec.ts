import { test, expect } from "@playwright/test";
import type { MonitorData } from "../src/lib/monitor";

// The global analytics script is outside this feature; never let its network
// latency hold the browser load event or send fixture traffic to the provider.
test.beforeEach(async ({ page }) => {
  await page.route("https://hm.baidu.com/**", (route) =>
    route.fulfill({ contentType: "application/javascript", body: "" }),
  );
});

function fixture(hours = 24): MonitorData {
  const now = new Date();
  const end = new Date(now);
  end.setUTCMinutes(0, 0, 0);
  return {
    generatedAt: now.toISOString(),
    sampledAt: now.toISOString(),
    sampleStale: false,
    services: [
      "api",
      "source-ingest",
      "mihomo-manager",
      "exit-provision",
      "exit-attributes",
      "recovery",
      "dead-verify",
    ].map((id, i) => ({
      id,
      name: [
        "节点 API",
        "数据采集",
        "Mihomo manager",
        "出口检测",
        "IP 属性",
        "节点恢复",
        "数据维护",
      ][i],
      description: "后台服务",
      status: "healthy",
      updatedAt: now.toISOString(),
      message: "心跳正常",
    })),
    current: {
      queues: [
        {
          kind: "exit",
          due: 6600,
          scheduled: 1700,
          running: 12,
          expired: 0,
          oldest_seconds: 6900,
        },
        {
          kind: "attributes",
          due: 200,
          scheduled: 400,
          running: 2,
          expired: 0,
          oldest_seconds: 300,
        },
      ],
      coverage: {
        total: 42000,
        connectivity: 40000,
        exit: 17000,
        attributes: 12000,
      },
      recovery: { active: 2, stalled: 0 },
    },
    kernel: {
      healthy: true,
      loaded: 42000,
      quarantined: 70,
      reloadMillis: 7500,
      reloadDrainMillis: 13000,
      reloadPauseMillis: 21000,
      exitCapacity: 12,
    },
    sweep: {
      running: true,
      candidates: 42000,
      processed: 21000,
      observations: 0,
      skipped: 0,
      rejected: 0,
      pending: 0,
      phase: "probing",
      outcome: null,
      kind: "full",
      started_at: now.toISOString(),
      finished_at: null,
      next_run_at: null,
    },
    hours,
    hourly: Array.from({ length: hours }, (_, i) => ({
      hour: new Date(+end - (hours - 1 - i) * 3600000).toISOString(),
      samples: i === 5 ? 0 : 60,
      queues:
        i === 5
          ? null
          : {
              exit: {
                due: Math.max(500, 8000 - i * 40),
                scheduled: 1500 + i * 5,
                running: 8 + (i % 5),
              },
              attributes: { due: 400 - i * 3, scheduled: 400, running: 2 },
            },
      coverage:
        i === 5
          ? null
          : {
              total: 42000,
              connectivity: Math.min(42000, 38000 + i * 20),
              exit: 15000 + i * 75,
              attributes: 10000 + i * 80,
            },
    })),
  };
}

test("service monitor shows progress, real hourly buckets, theme and range selection", async ({
  page,
}) => {
  const errors: string[] = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await page.route("**/api/services?**", (route) =>
    route.fulfill({
      json: fixture(
        Number(new URL(route.request().url()).searchParams.get("hours")),
      ),
    }),
  );
  await page.goto("/services", { waitUntil: "domcontentloaded" });
  await expect(
    page.getByRole("heading", { name: "服务监控", exact: true }),
  ).toBeVisible();
  await expect(page.getByText("运行正常", { exact: true })).toHaveCount(7);
  await expect(
    page.getByRole("progressbar", { name: "当前 / 最近一轮扫描" }),
  ).toHaveAttribute("aria-valuenow", "50");
  await expect(
    page.getByRole("progressbar", { name: "出口执行槽占用" }),
  ).toHaveAttribute("aria-valuenow", "100");
  await expect(
    page.getByRole("img", { name: "队列积压与计划，每小时均值曲线" }),
  ).toBeVisible();
  await page.getByRole("button", { name: "任务范围" }).click();
  await page.getByRole("menuitemradio", { name: "出口检测" }).click();
  await expect(
    page.getByText("出口检测 · 到期任务与未来计划分开统计"),
  ).toBeVisible();
  const sevenDays = page.waitForResponse((r) =>
    r.url().includes("/api/services?hours=168"),
  );
  await page.getByRole("button", { name: "7 天", exact: true }).click();
  await sevenDays;
  await expect(
    page.getByRole("button", { name: "7 天", exact: true }),
  ).toHaveAttribute("aria-pressed", "true");
  await page.getByRole("button", { name: "切换深色主题" }).click();
  await expect(page.locator("html")).toHaveClass("dark");
  await page.screenshot({
    path: test.info().outputPath("desktop-dark.png"),
    fullPage: true,
  });
  await page.getByRole("button", { name: "切换浅色主题" }).click();
  await page.screenshot({
    path: test.info().outputPath("desktop.png"),
    fullPage: true,
  });
  expect(errors).toEqual([]);
});

test("mobile layout fits and stale telemetry remains visibly stale", async ({
  page,
}) => {
  await page.setViewportSize({ width: 390, height: 844 });
  const data = fixture();
  data.sampleStale = true;
  data.sampledAt = new Date(Date.now() - 600000).toISOString();
  data.services[2].status = "stale";
  await page.route("**/api/services?**", (route) =>
    route.fulfill({ json: data }),
  );
  await page.goto("/services", { waitUntil: "domcontentloaded" });
  await expect(page.getByText(/采样已过期/)).toBeVisible();
  await expect(page.getByText("心跳超时", { exact: true })).toBeVisible();
  await expect(page.getByRole("heading", { name: "维护与等待" })).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth,
    ),
  ).toBeTruthy();
  await page.screenshot({
    path: test.info().outputPath("mobile.png"),
    fullPage: true,
  });
});

test("missing samples are not zero and failed refresh keeps prior readings", async ({
  page,
}) => {
  let failed = false;
  const data = fixture();
  data.current = null;
  data.sampledAt = null;
  data.sampleStale = true;
  data.hourly = data.hourly.map((p) => ({
    ...p,
    samples: 0,
    queues: null,
    coverage: null,
  }));
  await page.route("**/api/services?**", (route) =>
    failed
      ? route.fulfill({ status: 503, json: { error: "unavailable" } })
      : route.fulfill({ json: data }),
  );
  await page.goto("/services", { waitUntil: "domcontentloaded" });
  await expect(page.getByText("尚无历史采样", { exact: true })).toHaveCount(3);
  await expect(page.getByText(/正在等待首个后台样本/)).toBeVisible();
  failed = true;
  await page.getByRole("button", { name: "刷新状态" }).click();
  await expect(page.getByRole("alert")).toContainText("保留上次数据");
  await expect(page.getByText("运行正常", { exact: true })).toHaveCount(7);
});

test("initial API error can be retried without displaying fabricated health", async ({
  page,
}) => {
  let failed = true;
  await page.route("**/api/services?**", (route) =>
    failed
      ? route.fulfill({ status: 503, json: { error: "unavailable" } })
      : route.fulfill({ json: fixture() }),
  );
  await page.goto("/services", { waitUntil: "domcontentloaded" });
  await expect(
    page.getByText("服务监控暂不可用", { exact: true }),
  ).toBeVisible();
  await expect(page.getByText("运行正常", { exact: true })).toHaveCount(0);
  failed = false;
  await page.getByRole("button", { name: "重新连接" }).click();
  await expect(page.getByText("运行正常", { exact: true })).toHaveCount(7);
});
