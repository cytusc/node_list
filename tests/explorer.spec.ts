import { test, expect } from "@playwright/test";

const snapshot = {
  checkedAt: "2026-10-11T08:06:01+08:00",
  speedTest: true,
  mediaCheck: false,
  nodes: Array.from({ length: 63 }, (_, i) => ({
    name: `Node ${i + 1}`,
    baseName: `Node ${i + 1}`,
    server: `node-${i + 1}.example.org`,
    port: "443",
    type: i % 2 ? "vless" : "http",
    country: i % 2 ? "JP" : "US",
    network: "tcp",
    tls: true,
    udp: i % 2 === 1,
    speed: (i + 1) * 1024,
    details: [{ k: "alpn", v: "h2" }],
  })),
};

test("loads real local results with no browser errors", async ({
  page,
  request,
}) => {
  const errors: string[] = [];
  page.on("pageerror", (e) => errors.push(e.message));
  const response = await request.get("/api/results?view=aggregate");
  test.skip(!response.ok(), "Local subs-check installation is not available");
  const data = await response.json();
  await page.goto("/");
  await expect(page.locator(".title-count")).toHaveText(
    String(data.nodes.length),
  );
  await expect(page.locator(".node-row")).toHaveCount(
    Math.min(50, data.nodes.length),
  );
  await page.screenshot({
    animations: "disabled",
    path: "test-results/desktop.png",
  });
  await page.getByRole("button", { name: "切换深色主题" }).click();
  await expect(page.locator("html")).toHaveClass("dark");
  await page.screenshot({
    animations: "disabled",
    path: "test-results/dark.png",
  });
  expect(errors).toEqual([]);
});

test.describe("interactions and data states", () => {
  test.beforeEach(async ({ page }) => {
    await page.route("**/api/results?view=aggregate", (route) =>
      route.fulfill({ json: snapshot }),
    );
    await page.route("**/api/history", (route) =>
      route.fulfill({ json: { items: [], total: 0 } }),
    );
    await page.route("**/api/downloads", (route) =>
      route.fulfill({
        json: [
          {
            id: "clash",
            label: "Clash 节点",
            available: true,
            updatedAt: snapshot.checkedAt,
          },
        ],
      }),
    );
    await page.goto("/");
    await expect(page.locator(".title-count")).toHaveText("63");
  });

  test("shows aggregated nodes and refreshes without a batch selector", async ({
    page,
  }) => {
    await page.route("**/api/results?view=aggregate", (route) =>
      route.fulfill({
        json: {
          ...snapshot,
          batchCount: 2,
          nodes: [
            {
              ...snapshot.nodes[0],
              checkedAt: "2026-10-10T00:00:00Z",
              name: "Archived node",
              baseName: "Archived node",
            },
            snapshot.nodes[1],
          ],
        },
      }),
    );
    await page.getByRole("button", { name: "刷新结果" }).click();
    await expect(page.locator(".title-count")).toHaveText("2");
    await expect(page.getByLabel("检测批次")).toHaveCount(0);
    await expect(page.getByText(/已聚合 2 轮检测/)).toBeVisible();
    await expect(
      page.getByText("订阅下载仍使用最新生成的文件。"),
    ).toBeVisible();
    await page
      .getByRole("textbox", { name: "搜索节点", exact: true })
      .fill("Archived node");
    await page.locator(".node-name").first().click();
    await expect(page.getByRole("dialog")).toContainText("2026/10/10 08:00");
    await page.keyboard.press("Escape");
    await page.getByRole("button", { name: "刷新结果" }).click();
    await expect(page.locator(".title-count")).toHaveText("2");
  });

  test("sorts, paginates, searches and distinguishes no matches", async ({
    page,
  }) => {
    await expect(page.locator(".node-row").first()).toContainText("Node 63");
    await page.getByRole("button", { name: "下一页", exact: true }).click();
    await expect(page.locator(".node-row")).toHaveCount(13);
    await page
      .getByRole("textbox", { name: "搜索节点", exact: true })
      .fill("node-5.example.org");
    await expect(page.locator(".node-row")).toHaveCount(1);
    await expect(page.locator(".pagination-bar")).toContainText(
      "显示 1–1，共 1 个",
    );
    await page
      .getByRole("textbox", { name: "搜索节点", exact: true })
      .fill("nothing-matches");
    await expect(
      page.getByRole("heading", { name: "没有匹配的节点" }),
    ).toBeVisible();
    await page
      .getByRole("button", { name: "清空筛选", exact: true })
      .first()
      .click();
    await page.getByLabel("排序方式").selectOption("speed-asc");
    await expect(page.locator(".node-row").first()).toContainText("Node 1");
  });

  test("combines filters and opens accessible details", async ({ page }) => {
    await page.getByRole("button", { name: "协议筛选", exact: true }).click();
    await page.getByRole("option", { name: /VLESS/ }).click();
    await page.keyboard.press("Escape");
    await expect(page.locator(".result-count")).toContainText("31");
    await page.getByLabel("最低速度，MB/s").fill("60");
    await expect(page.locator(".node-row")).toHaveCount(2);
    await page.locator(".node-name").first().click();
    await expect(page.getByRole("dialog")).toBeVisible();
    await expect(page.getByRole("dialog")).toContainText("Node 62");
    await expect(page.getByRole("dialog")).toContainText("h2");
    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog")).not.toBeVisible();
  });

  for (const mode of ["modern", "missing", "denied", "failed"] as const) {
    test(`copies detail fields and server addresses (${mode})`, async ({
      page,
    }) => {
      await page.addInitScript((mode) => {
        const writes: string[] = [];
        Object.assign(window, { clipboardWrites: writes });
        Object.defineProperty(navigator, "clipboard", {
          configurable: true,
          value:
            mode === "missing"
              ? undefined
              : {
                  writeText: async (text: string) => {
                    if (mode !== "modern") {
                      throw new DOMException(
                        "Permission denied",
                        "NotAllowedError",
                      );
                    }
                    writes.push(text);
                  },
                },
        });
        const execCommand = document.execCommand.bind(document);
        document.execCommand = (command) => {
          if (command !== "copy") return execCommand(command);
          if (mode === "failed") return false;
          const input = document.activeElement;
          if (
            !(input instanceof HTMLTextAreaElement) ||
            !input.closest('[role="dialog"]')
          ) {
            throw new Error("Copy input must be focused inside the dialog");
          }
          const copied = execCommand(command);
          if (copied)
            writes.push(
              input.value.slice(input.selectionStart, input.selectionEnd),
            );
          return copied;
        };
      }, mode);
      await page.reload();
      await page.locator(".node-name").first().click();
      const dialog = page.getByRole("dialog");
      const transport = dialog.getByRole("button", {
        name: "复制传输方式",
        exact: true,
      });
      await transport.click();
      if (mode === "failed") {
        await expect(
          page.getByText("复制失败，请手动选择并复制内容"),
        ).toBeVisible();
        await expect(transport).toBeFocused();
        await expect(dialog.locator("textarea")).toHaveCount(0);
        return;
      }
      await expect(page.getByText("已复制到剪贴板").first()).toBeVisible();
      await expect(transport).toBeFocused();
      await dialog
        .getByRole("button", { name: "复制完整名称", exact: true })
        .click();
      await dialog
        .getByRole("button", { name: "复制服务器地址", exact: true })
        .click();
      await expect
        .poll(() =>
          page.evaluate(
            () =>
              (window as Window & { clipboardWrites?: string[] })
                .clipboardWrites,
          ),
        )
        .toEqual(["tcp", "Node 63", "node-63.example.org:443"]);
      await expect(dialog.locator("textarea")).toHaveCount(0);
    });
  }

  test("shows column preferences and downloads the complete subscription", async ({
    page,
  }) => {
    await page.getByRole("button", { name: "显示列" }).click();
    await page
      .getByRole("menuitemcheckbox", { name: "IP 来源", exact: true })
      .click();
    await page.keyboard.press("Escape");
    await expect(page.locator('th[data-column="ipSource"]')).toHaveCount(0);
    await page.route("**/api/download/clash", (route) =>
      route.fulfill({ contentType: "application/yaml", body: "proxies: []\n" }),
    );
    await page.getByRole("button", { name: "下载订阅", exact: true }).click();
    await expect(
      page.getByText("包含文件中的全部节点，不受当前筛选影响。"),
    ).toBeVisible();
    const pending = page.waitForEvent("download");
    await page.getByRole("menuitem", { name: /Clash 节点/ }).click();
    expect((await pending).suggestedFilename()).toBe("all.yaml");
  });

  test("retains previous results after refresh fails", async ({ page }) => {
    await page.route("**/api/results?view=aggregate", (route) =>
      route.fulfill({
        status: 503,
        json: { error: "读取失败", code: "READ_FAILED" },
      }),
    );
    await page.getByRole("button", { name: "刷新结果" }).click();
    await expect(page.getByRole("alert")).toContainText(
      "刷新失败，当前保留上次读取的结果",
    );
    await expect(page.locator(".node-row")).toHaveCount(50);
  });

  test("handles zero nodes and disabled speed tests", async ({ page }) => {
    await page.route("**/api/results?view=aggregate", (route) =>
      route.fulfill({ json: { ...snapshot, speedTest: false, nodes: [] } }),
    );
    await page.getByRole("button", { name: "刷新结果" }).click();
    await expect(
      page.getByRole("heading", { name: "暂无保留节点" }),
    ).toBeVisible();
    await expect(page.getByLabel("最低速度，MB/s")).toHaveCount(0);
    await expect(page.locator(".summary-line")).toContainText("测速未开启");
  });

  test("shows service tags only when enabled", async ({ page }) => {
    await expect(
      page.getByRole("button", { name: "服务解锁筛选" }),
    ).toHaveCount(0);
    await page.route("**/api/results?view=aggregate", (route) =>
      route.fulfill({
        json: {
          ...snapshot,
          mediaCheck: true,
          nodes: [
            {
              ...snapshot.nodes[0],
              media: [{ platform: "openai", tag: "GPT⁺-US" }],
            },
            snapshot.nodes[1],
          ],
        },
      }),
    );
    await page.getByRole("button", { name: "刷新结果" }).click();
    await page
      .getByRole("button", { name: "服务解锁筛选", exact: true })
      .click();
    await page.getByRole("option", { name: /OpenAI/ }).click();
    await page.keyboard.press("Escape");
    await expect(page.locator(".node-row")).toHaveCount(1);
    await page.locator(".node-name").first().click();
    await expect(page.getByRole("dialog")).toContainText("GPT⁺-US");
  });

  test("mobile layout fits the viewport and opens details", async ({
    page,
  }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await expect(page.locator(".desktop-table")).not.toBeVisible();
    await expect(page.locator(".mobile-node").first()).toBeVisible();
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= window.innerWidth,
      ),
    ).toBe(true);
    await page.screenshot({
      animations: "disabled",
      path: "test-results/mobile.png",
    });
    await page.locator(".mobile-node").first().click();
    await expect(page.getByRole("dialog")).toBeVisible();
    await page.screenshot({
      animations: "disabled",
      path: "test-results/mobile-details.png",
    });
  });
});
