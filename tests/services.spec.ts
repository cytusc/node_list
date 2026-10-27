import { test, expect } from "./authenticated-fixture";
import type { MonitorData } from "../src/lib/monitor";

function fixture(): MonitorData {
  const now = new Date().toISOString();
  return {
    generatedAt: now,
    services: [
      "api",
      "source-ingest",
      "node-liveness",
      "exit-provision",
      "exit-attributes",
    ].map((id, i) => ({
      id,
      name: ["节点 API", "数据采集", "节点测活", "出口检测", "IP 属性"][i],
      description: "后台服务",
      status: "healthy",
      updatedAt: now,
      message: "心跳正常",
    })),
    liveness: {
      xrayQueued: 10,
      mihomoQueued: 3,
      inFlight: 2,
      lastCompletedAt: now,
    },
  };
}

test("monitor reads current services without history or kernel placeholders", async ({
  page,
}) => {
  let requests = 0;
  await page.route("**/api/services", (route) => {
    requests++;
    return route.fulfill({ json: fixture() });
  });
  await page.goto("/services");
  await expect(
    page.getByRole("heading", { name: "服务监控", exact: true }),
  ).toBeVisible();
  await expect(page.getByText("运行正常", { exact: true })).toHaveCount(5);
  await expect(page.getByText("Xray 待测", { exact: true })).toBeVisible();
  await expect(
    page.getByRole("button", { name: "7 天", exact: true }),
  ).toHaveCount(0);
  const beforeRefresh = requests;
  await page.getByRole("button", { name: "刷新状态" }).click();
  await expect.poll(() => requests).toBeGreaterThan(beforeRefresh);
  await page.getByRole("button", { name: "切换深色主题" }).click();
  await expect(page.locator("html")).toHaveClass("dark");
});

test("mobile monitor shows heartbeat expiry and unknown counts", async ({
  page,
}) => {
  await page.setViewportSize({ width: 390, height: 844 });
  const data = fixture();
  data.services[2].status = "stale";
  data.liveness.inFlight = null;
  await page.route("**/api/services", (route) => route.fulfill({ json: data }));
  await page.goto("/services");
  await expect(page.getByText("心跳超时", { exact: true })).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBeTruthy();
});

test("failed refresh clears prior readings and retry reads updated values", async ({
  page,
}) => {
  let failed = false;
  const data = fixture();
  await page.route("**/api/services", (route) =>
    failed
      ? route.fulfill({ status: 503, json: { error: "unavailable" } })
      : route.fulfill({ json: data }),
  );
  await page.goto("/services");
  await expect(page.getByText("运行正常", { exact: true })).toHaveCount(5);
  failed = true;
  await page.getByRole("button", { name: "刷新状态" }).click();
  await expect(page.getByRole("alert")).toContainText("服务监控暂不可用");
  await expect(page.getByText("运行正常", { exact: true })).toHaveCount(0);
  failed = false;
  data.services[0].status = "degraded";
  await page.getByRole("button", { name: "重新连接" }).click();
  await expect(page.getByText("需要关注", { exact: true })).toBeVisible();
});
