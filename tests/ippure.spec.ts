import { test, expect } from "@playwright/test";
import { extractIPPure } from "../server/ippure.js";

const html = `<div class="iptable-container">
<div><span class="info-key">IP</span><div class="info-value"><span>82.38.64.237</span><button></button></div></div>
<div><span class="info-key">ASN</span><div class="info-value">AS212194 - YUUSEI LTD</div><div class="botclass-value"><span>human 15.96%</span><span>bot 84.04%</span></div></div>
<div><span class="info-key">IP来源</span><span class="info-value"><span class="ip-tag">广播IP</span></span></div>
<div><span class="info-key">IP属性</span><span class="info-value ip-attr"><span class="ip-tag">机房IP</span></span></div>
<div class="colormap-indicator-value">32% 中性</div></div>`;

test("extracts IPPure DOM values, handles zero and missing ASN statistics, rejects partial results", async ({
  page,
}) => {
  await page.setContent(html);
  const result = await page.evaluate(extractIPPure);
  expect(result).toEqual({
    ip: "82.38.64.237",
    asn: "AS212194",
    asnOrganization: "YUUSEI LTD",
    source: "广播IP",
    attributes: ["机房IP"],
    humanPercent: 15.96,
    botPercent: 84.04,
    score: 32,
    label: "中性",
  });
  await page.locator(".colormap-indicator-value").evaluate((el) => {
    el.textContent = "0% 极度纯净";
  });
  expect((await page.evaluate(extractIPPure)).score).toBe(0);
  await page.locator(".botclass-value").evaluate((el) => el.remove());
  expect((await page.evaluate(extractIPPure)).humanPercent).toBeNull();
  await page.locator(".ip-attr").evaluate((el) => {
    el.textContent = "";
  });
  await expect(page.evaluate(extractIPPure)).rejects.toThrow(
    "IPPure 返回信息不完整",
  );
});

test("shows stored IPPure fields in desktop details and mobile cards", async ({
  page,
}) => {
  await page.route("**/api/results?view=aggregate", (route) =>
    route.fulfill({
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
    route.fulfill({ json: { items: [], total: 0 } }),
  );
  await page.route("**/api/downloads", (route) => route.fulfill({ json: [] }));
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
