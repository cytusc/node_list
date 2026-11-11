import { test, expect } from "@playwright/test";

test.beforeEach(async ({ page }) => {
  await page.route("https://hm.baidu.com/**", (route) => route.abort());
});

test("login validates credentials before mounting protected pages and supports logout", async ({
  page,
}) => {
  await page.route("**/api/auth/session", (route) =>
    route.fulfill({ json: { authenticated: false, configured: true } }),
  );
  let attempts = 0;
  await page.route("**/api/auth/login", (route) => {
    expect(route.request().headers()["x-node-explorer"]).toBe("1");
    attempts++;
    return route.fulfill(
      attempts === 1
        ? { status: 401, json: { error: "账号或密码错误。" } }
        : { json: { authenticated: true } },
    );
  });
  await page.route("**/api/auth/logout", (route) =>
    route.fulfill({ json: { authenticated: false } }),
  );
  await page.route("**/api/services", (route) =>
    route.fulfill({ status: 503, json: { error: "测试监控暂不可用" } }),
  );
  let protectedRequests = 0;
  page.on("request", (request) => {
    if (request.url().includes("/api/services")) protectedRequests++;
  });
  await page.goto("/services");
  await expect(
    page.getByRole("heading", { name: "登录节点面板" }),
  ).toBeVisible();
  expect(protectedRequests).toBe(0);
  await page.getByLabel("账号", { exact: true }).fill("admin");
  await page.getByLabel("密码", { exact: true }).fill("incorrect-password");
  await page.getByRole("button", { name: "登录", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText("账号或密码错误");
  await expect(page.getByLabel("账号", { exact: true })).toHaveValue("admin");
  await page.getByLabel("密码", { exact: true }).fill("correct-password");
  await page.getByRole("button", { name: "登录", exact: true }).click();
  await expect(page.getByRole("button", { name: "退出登录" })).toBeVisible();
  await expect.poll(() => protectedRequests).toBeGreaterThan(0);
  await page.getByRole("button", { name: "退出登录" }).click();
  await expect(
    page.getByRole("heading", { name: "登录节点面板" }),
  ).toBeVisible();
});

test("missing server credentials keeps the login form closed", async ({
  page,
}) => {
  await page.route("**/api/auth/session", (route) =>
    route.fulfill({ json: { authenticated: false, configured: false } }),
  );
  await page.goto("/login");
  await expect(page.getByRole("alert")).toContainText("管理员尚未配置");
  await expect(
    page.getByRole("button", { name: "登录", exact: true }),
  ).toBeDisabled();
});

test("expired protected request returns to login", async ({ page }) => {
  await page.route("**/api/auth/session", (route) =>
    route.fulfill({ json: { authenticated: true, configured: true } }),
  );
  await page.route("**/api/results?**", (route) =>
    route.fulfill({ status: 401, json: { error: "请先登录。" } }),
  );
  await page.goto("/");
  await expect(page.getByRole("alert")).toContainText("登录已过期");
  await expect(
    page.getByRole("heading", { name: "登录节点面板" }),
  ).toBeVisible();
});

test("anonymous clients cannot bypass the UI through protected API routes", async ({
  request,
}) => {
  for (const path of ["/api/results", "/api/services"]) {
    const response = await request.get(path);
    expect(response.status()).toBe(401);
    expect(response.headers()["cache-control"]).toBe("no-store");
  }
  const link = await request.post("/api/nodes/link", {
    data: { identity: "test" },
    headers: { "X-Node-Explorer": "1" },
  });
  expect(link.status()).toBe(401);
  expect((await request.get("/api/health")).ok()).toBeTruthy();
});
