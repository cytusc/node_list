import { fulfillRoute } from "./results-fixture";
import { test, expect } from "@playwright/test";

test("shows business usage independently of residential votes and company type", async ({
  page,
}) => {
  await page.route("**/api/results?**", (route) =>
    fulfillRoute(route, {
      json: {
        speedTest: false,
        mediaCheck: false,
        nodes: [
          {
            name: "Business network",
            type: "http",
            server: "business.example",
            port: "443",
            network: "tcp",
            tls: true,
            udp: false,
            speed: 0,
            ippure: {
              status: "success",
              attempts: 1,
              checkedAt: "2026-10-15T00:00:00Z",
              source: "原生IP",
              attributes: ["商业宽带"],
              score: 10,
              label: "极度纯净",
              classification: {
                version: 4,
                state: "unknown",
                types: [
                  { provider: "ippure", usageType: "商业宽带" },
                  {
                    provider: "ipinfo",
                    usageType: "商业宽带",
                    companyType: "住宅IP",
                  },
                ],
              },
            },
          },
        ],
      },
    }),
  );
  await page.route("**/api/history", (route) =>
    fulfillRoute(route, { json: { items: [], total: 0 } }),
  );
  await page.goto("/");
  const cell = page.locator('.node-row [data-column="ipAttributes"]');
  await expect(cell).toHaveText("商业宽带");
  await expect(cell.locator("span[title]")).toHaveAttribute(
    "title",
    /IPinfo：使用类型 商业宽带；公司类型 住宅IP/,
  );
  await page.getByRole("button", { name: "IP 属性筛选", exact: true }).click();
  await page.getByRole("option", { name: /^商业宽带\s+1$/ }).click();
  await page.keyboard.press("Escape");
  await expect(page.locator(".node-row")).toHaveCount(1);
  await page.locator(".node-name").click();
  const sources = page
    .getByRole("dialog")
    .getByRole("region", { name: "IP 类型数据来源" });
  await expect(
    sources.getByRole("columnheader", { name: "使用类型" }),
  ).toBeVisible();
  const ipinfo = sources.getByRole("row").filter({ hasText: "IPinfo" });
  await expect(ipinfo.getByRole("cell").nth(0)).toHaveText("商业宽带");
  await expect(ipinfo.getByRole("cell").nth(1)).toHaveText("住宅IP");
  await expect(page.getByRole("dialog")).not.toContainText("性质未知");
  await expect(page.getByRole("dialog")).not.toContainText("性质争议");
});

test("filters IP source and attributes and clears selections", async ({
  page,
}) => {
  const nodes = [
    { name: "Residential", source: "原生IP", attributes: ["住宅IP", "IPv4"] },
    { name: "Datacenter", source: "广播IP", attributes: ["机房IP", "IPv4"] },
    { name: "Broadcast residential", source: "广播IP", attributes: ["住宅IP"] },
  ].map(({ name, source, attributes }, index) => ({
    name,
    type: "http",
    server: `node-${index}.example`,
    port: "443",
    network: "tcp",
    tls: true,
    udp: false,
    speed: 0,
    ippure: {
      status: "success",
      source,
      attributes,
      attempts: 1,
      checkedAt: "2026-10-14T00:00:00Z",
    },
  }));
  await page.route("**/api/results?**", (route) =>
    fulfillRoute(route, {
      json: { speedTest: false, mediaCheck: false, nodes },
    }),
  );
  await page.route("**/api/downloads", (route) =>
    fulfillRoute(route, { json: [] }),
  );
  await page.goto("/");
  await expect(page.locator(".node-row")).toHaveCount(3);
  await expect(page.getByRole("button", { name: "连接特性筛选" })).toHaveCount(
    0,
  );
  await page.getByRole("button", { name: "IP 来源筛选", exact: true }).click();
  await page.getByRole("option", { name: "广播IP" }).click();
  await page.keyboard.press("Escape");
  await expect(page.locator(".node-row")).toHaveCount(2);
  await page.getByRole("button", { name: "IP 属性筛选", exact: true }).click();
  await page.getByRole("option", { name: "住宅IP" }).click();
  await page.keyboard.press("Escape");
  await expect(page.locator(".node-row")).toHaveCount(1);
  await expect(page.locator(".node-row")).toContainText(
    "Broadcast residential",
  );
  await page
    .getByRole("button", { name: "移除筛选 广播IP", exact: true })
    .click();
  await expect(page.locator(".node-row")).toHaveCount(2);
  await page.getByRole("button", { name: "清空筛选", exact: true }).click();
  await expect(page.locator(".node-row")).toHaveCount(3);
  await expect(page.locator(".active-filters")).toHaveCount(0);
});

test("shows stored IPPure fields in desktop details and mobile cards", async ({
  page,
}) => {
  await page.route("**/api/results?**", (route) =>
    fulfillRoute(route, {
      json: {
        speedTest: false,
        mediaCheck: false,
        batchCount: 1,
        checkedAt: "2026-10-11T00:00:00Z",
        nodes: [
          {
            name: "IPPure node",
            type: "http",
            server: "test.example.org",
            port: "443",
            network: "tcp",
            tls: true,
            udp: false,
            speed: 0,
            ip: "8.8.8.8",
            ippure: {
              status: "success",
              attempts: 1,
              checkedAt: "2026-10-11T00:00:00Z",
              ip: "82.38.64.237",
              asn: "AS212194",
              asnOrganization: "YUUSEI LTD",
              humanPercent: 15.96,
              botPercent: 84.04,
              source: "广播IP",
              attributes: ["机房IP"],
              score: 32,
              label: "中性",
            },
          },
        ],
      },
    }),
  );
  await page.route("**/api/history", (route) =>
    fulfillRoute(route, { json: { items: [], total: 0 } }),
  );
  await page.route("**/api/downloads", (route) =>
    fulfillRoute(route, { json: [] }),
  );
  await page.goto("/");
  await expect(page.locator(".node-row")).toContainText("32% 中性");
  for (const label of [
    "ASN 组织",
    "IP 来源",
    "IP 属性",
    "IPPure 系数",
    "人机流量比",
  ])
    await expect(page.getByRole("columnheader", { name: label })).toHaveCount(
      1,
    );
  const row = page.locator(".node-row");
  await expect(row.locator('[data-column="asnOrganization"]')).toHaveText(
    "YUUSEI LTD",
  );
  await expect(row.locator('[data-column="ipSource"]')).toHaveText("广播IP");
  await expect(row.locator('[data-column="ipAttributes"]')).toHaveText(
    "机房IP",
  );
  await expect(row.locator('[data-column="ippure"]')).toContainText("32% 中性");
  await expect(row.locator('[data-column="ippure"]')).not.toContainText(
    "YUUSEI LTD",
  );
  await expect(row.locator('[data-column="trafficRatio"]')).toContainText(
    "human 15.96%",
  );
  for (const text of [
    "广播IP",
    "机房IP",
    "YUUSEI LTD",
    "human 15.96%",
    "bot 84.04%",
  ])
    await expect(row).toContainText(text);
  await expect(
    row.getByRole("img", { name: "人机流量比 human 15.96% / bot 84.04%" }),
  ).toBeVisible();
  await page.getByRole("button", { name: "显示列" }).click();
  await page
    .getByRole("menuitemcheckbox", { name: "IP 来源", exact: true })
    .click();
  await page.keyboard.press("Escape");
  await expect(row.locator('[data-column="ipSource"]')).toHaveCount(0);
  await expect(row.locator('[data-column="ipAttributes"]')).toHaveText(
    "机房IP",
  );
  await page.getByRole("button", { name: "显示列" }).click();
  await page
    .getByRole("menuitemcheckbox", { name: "IP 来源", exact: true })
    .click();
  await page.keyboard.press("Escape");
  await page.locator(".node-name").click();
  const dialog = page.getByRole("dialog");
  for (const text of [
    "human 15.96% / bot 84.04%",
    "广播IP",
    "机房IP",
    "32% 中性",
    "82.38.64.237",
    "YUUSEI LTD",
    "AS212194",
  ])
    await expect(dialog).toContainText(text);
  await expect(dialog.getByLabel("IPPure 系数 32%")).toBeVisible();
  await expect(
    dialog
      .locator(".detail-field")
      .filter({ has: page.locator("dt", { hasText: /^出口 IP$/ }) }),
  ).toContainText("82.38.64.237");
  await expect(
    dialog
      .locator(".detail-field")
      .filter({ has: page.locator("dt", { hasText: /^上游检测 IP$/ }) }),
  ).toContainText("8.8.8.8");
  await page.keyboard.press("Escape");
  await page.setViewportSize({ width: 390, height: 844 });
  const card = page.locator(".mobile-node");
  for (const text of [
    "32% 中性",
    "广播IP",
    "机房IP",
    "YUUSEI LTD",
    "human 15.96%",
    "bot 84.04%",
  ])
    await expect(card).toContainText(text);
  await expect(
    card.getByRole("img", { name: "人机流量比 human 15.96% / bot 84.04%" }),
  ).toBeVisible();
});

test("retains an IPv6 exit and distinguishes unsupported scoring from a dead node", async ({
  page,
}) => {
  await page.route("**/api/results?**", (route) =>
    fulfillRoute(route, {
      json: {
        speedTest: false,
        mediaCheck: false,
        batchCount: 1,
        nodes: [
          {
            name: "IPv6 node",
            type: "http",
            server: "node.example",
            port: "443",
            network: "tcp",
            tls: true,
            udp: false,
            speed: 0,
            ippure: {
              status: "unsupported",
              attempts: 1,
              checkedAt: "2026-10-12T00:00:00Z",
              ip: "2001:db8::23",
              exitIPSource: "AWS CheckIP",
              addressFamily: 6,
              error: "已获取 IPv6 出口，IPPure 风险评分接口暂不支持 IPv6",
            },
          },
        ],
      },
    }),
  );
  await page.route("**/api/downloads", (route) =>
    fulfillRoute(route, { json: [] }),
  );
  await page.goto("/");
  const row = page.locator(".node-row");
  await expect(row.locator('[data-column="ippure"]')).toHaveText(
    "仅 IPv6，暂无评分",
  );
  await expect(row).not.toContainText("检测失败");
  await page.locator(".node-name").click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toContainText("2001:db8::23");
  await expect(dialog).toContainText("AWS CheckIP");
  await expect(dialog).toContainText("风险评分接口暂不支持 IPv6");
  await expect(dialog).not.toContainText("最多 3 次");
});
