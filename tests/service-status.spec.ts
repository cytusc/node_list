import { test, expect, type Page } from "@playwright/test";
import { fulfillRoute } from "./results-fixture";

function snapshot() {
  const updatedAt = new Date().toISOString();
  const services = [
    "api",
    "connectivity",
    "ip-service",
    "mihomo-manager",
    "node-cleanup",
    "node-lifecycle",
    "normal-recovery",
    "residential-recovery",
    "source-ingest",
  ].map((service) => ({
    service,
    updatedAt,
    healthy: true,
    queued: 0,
    scheduled: 0,
    running: 0,
    oldestWaitSeconds: 0,
    restored: 0,
    restoredNodes: 0,
    checked: 0,
    checkedLastHour: 0,
    skipped: 0,
    blocked: 0,
    expired: 0,
    cooldown: 0,
    activeAssessments: 0,
    kernel: undefined as
      | undefined
      | {
          loaded: number;
          quarantined: number;
          healthy: boolean;
          generation: string;
          reloads: number;
          reloadMillis: number;
          fastRunning: number;
        },
    fast: undefined as
      | undefined
      | {
          minute: {
            completed: number;
            success: number;
            failed: number;
            abnormal: number;
          };
          hour: {
            completed: number;
            success: number;
            failed: number;
            abnormal: number;
          };
        },
  }));
  Object.assign(
    services.find((s) => s.service === "mihomo-manager")!,
    {
      kernel: {
        loaded: 15920,
        quarantined: 1878,
        healthy: true,
        generation: "instance:2",
        reloads: 2,
        reloadMillis: 47165,
        fastRunning: 3,
      },
      fast: {
        minute: { completed: 66, success: 26, failed: 40, abnormal: 0 },
        hour: { completed: 39199, success: 6235, failed: 32895, abnormal: 69 },
      },
    },
  );
  Object.assign(
    services.find((s) => s.service === "connectivity")!,
    { running: 4, scheduled: 2360 },
  );
  Object.assign(
    services.find((s) => s.service === "ip-service")!,
    { running: 1, scheduled: 5103 },
  );
  Object.assign(
    services.find((s) => s.service === "residential-recovery")!,
    {
      activeAssessments: 7,
      scheduled: 35,
      cooldown: 35,
      checked: 2592,
      checkedLastHour: 34,
      skipped: 622,
    },
  );
  Object.assign(
    services.find((s) => s.service === "normal-recovery")!,
    { scheduled: 4833, checked: 178897, checkedLastHour: 31027 },
  );
  Object.assign(
    services.find((s) => s.service === "node-lifecycle")!,
    { restored: 1630, restoredNodes: 1120 },
  );
  return { observedAt: updatedAt, services };
}
async function results(page: Page) {
  await page.route("**/api/results?**", (route) =>
    fulfillRoute(route, {
      json: { speedTest: true, mediaCheck: false, nodes: [] },
    }),
  );
}

test("dashboard summarizes health, queues and per-service warnings", async ({
  page,
}) => {
  await results(page);
  await page.route("**/api/services", (route) =>
    route.fulfill({ json: snapshot() }),
  );
  await page.goto("/");
  const panel = page.locator(".service-status");
  await expect(panel).toContainText("8 项运行");
  await expect(panel).toContainText("1 项告警");
  await page.getByRole("button", { name: "展开服务状态", exact: true }).focus();
  await page.keyboard.press("Enter");
  const grid = panel.locator(".service-status-grid");
  await expect(grid).toBeVisible();
  await expect(grid).toContainText("8");
  await expect(grid).toContainText("1 项需关注");
  await expect(grid).toContainText("8");
  await expect(grid).toContainText("执行中任务");
  await expect(grid).toContainText("待处理队列");
  await expect(grid).toContainText("定时等待 12,331");
  const alertList = panel.locator(".service-alert-list");
  await expect(alertList).toHaveCount(1);
  const alertCard = alertList.locator(".service-alert-card");
  await expect(alertCard).toHaveCount(1);
  await expect(alertCard).toContainText("共享测活内核");
  await expect(alertCard).toContainText("1,878 个节点配置被隔离");
  await alertCard.click();
  const detail = page.locator("#service-detail-mihomo-manager");
  await expect(detail).toContainText("快速检测中3");
  await expect(alertCard).toHaveAttribute("data-health", "warning");
  await page
    .getByRole("button", { name: "查看住宅 IP 恢复详情", exact: true })
    .click();
  const recovery = page.locator("#service-detail-residential-recovery");
  await expect(recovery).toContainText("核验中会话7");
  await expect(recovery).toContainText("其中冷却等待35");
  await expect(recovery).toContainText("早期未留存记录不计入");
  await page
    .getByRole("button", { name: "查看节点状态详情", exact: true })
    .click();
  await expect(page.locator("#service-detail-node-lifecycle")).toContainText(
    "累计恢复次数1,630",
  );
  await expect(page.locator("#service-detail-node-lifecycle")).toContainText(
    "涉及不同节点1,120",
  );
  await page.getByRole("button", { name: "刷新服务状态", exact: true }).click();
  await expect(recovery).toBeVisible();
  await expect(
    page.getByRole("button", { name: "收起服务状态", exact: true }),
  ).toHaveAttribute("aria-expanded", "true");
});

test("failed refresh and old snapshots cannot retain green status", async ({
  page,
}) => {
  await results(page);
  let mode = "fresh";
  await page.route("**/api/services", (route) => {
    if (mode === "failed")
      return route.fulfill({ status: 503, json: { error: "read failed" } });
    const body = snapshot();
    if (mode === "old")
      body.observedAt = new Date(Date.now() - 60000).toISOString();
    return route.fulfill({ json: body });
  });
  await page.goto("/");
  const panel = page.locator(".service-status");
  await expect(panel).toContainText("8 项运行");
  await page.getByRole("button", { name: "展开服务状态", exact: true }).click();
  mode = "failed";
  await page.getByRole("button", { name: "刷新服务状态", exact: true }).click();
  await expect(panel).toContainText("状态更新失败");
  await expect(panel).toContainText("数据待更新");
  await expect(panel).not.toContainText("8 项运行");
  mode = "old";
  await page.getByRole("button", { name: "刷新服务状态", exact: true }).click();
  await expect(panel).toContainText("已超过 45 秒未取得新数据");
  mode = "fresh";
  await page.getByRole("button", { name: "刷新服务状态", exact: true }).click();
  await expect(panel).toContainText("8 项运行");
  await expect(panel).not.toContainText("数据待更新");
});

test("fresh snapshots still expose stale service heartbeats", async ({
  page,
}) => {
  await results(page);
  const body = snapshot();
  body.services.find((s) => s.service === "connectivity")!.updatedAt = new Date(
    Date.now() - 120000,
  ).toISOString();
  await page.route("**/api/services", (route) => route.fulfill({ json: body }));
  await page.goto("/");
  const panel = page.locator(".service-status");
  await expect(panel).toContainText("1 项异常");
  await page.getByRole("button", { name: "展开服务状态", exact: true }).click();
  const card = panel
    .locator(".service-alert-card")
    .filter({ hasText: "连通性检测" });
  await expect(card).toHaveAttribute("data-health", "error");
  await expect(card).toContainText("服务心跳已过期");
});

test("initial read failure offers a visible retry", async ({ page }) => {
  await results(page);
  await page.route("**/api/services", (route) =>
    route.fulfill({ status: 503, json: {} }),
  );
  await page.goto("/");
  await expect(page.locator(".service-status")).toContainText("暂不可用");
  await expect(
    page.getByRole("button", { name: "刷新服务状态", exact: true }),
  ).toBeEnabled();
});

test("mobile and dark service details remain readable", async ({ page }) => {
  await results(page);
  await page.route("**/api/services", (route) =>
    route.fulfill({ json: snapshot() }),
  );
  await page.goto("/");
  await page.getByRole("button", { name: "展开服务状态", exact: true }).click();
  await page
    .getByRole("button", { name: "查看共享测活内核详情", exact: true })
    .click();
  await page
    .locator(".service-status")
    .screenshot({ path: "test-results/service-status-desktop.png" });
  await page.evaluate(() => document.documentElement.classList.add("dark"));
  await page
    .locator(".service-status")
    .screenshot({ path: "test-results/service-status-dark.png" });
  await page.setViewportSize({ width: 390, height: 844 });
  const overflow = await page
    .locator(".service-status")
    .evaluate((el) => el.scrollWidth > el.clientWidth);
  expect(overflow).toBe(false);
  await expect(
    page.getByRole("button", { name: "收起共享测活内核详情", exact: true }),
  ).toBeVisible();
  await page
    .locator(".service-status")
    .screenshot({ path: "test-results/service-status-mobile.png" });
  await page.getByRole("button", { name: "指标说明", exact: true }).click();
  await expect(
    page.getByText("任务与服务分别统计", { exact: true }),
  ).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(
    page.getByText("任务与服务分别统计", { exact: true }),
  ).not.toBeVisible();
});
