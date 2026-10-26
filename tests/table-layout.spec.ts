import { test, expect } from "./authenticated-fixture";
import { fulfillRoute } from "./results-fixture";

for (const width of [1440, 1366, 1024]) {
  test(`table contains long ASN text and keeps time and copy visible at ${width}px`, async ({
    page,
  }) => {
    await page.setViewportSize({ width, height: 1000 });
    const organization =
      "A very long ASN organization with multiple international network company names";
    await page.route("**/api/results?**", (route) =>
      fulfillRoute(route, {
        json: {
          speedTest: true,
          mediaCheck: true,
          nodes: [
            {
              identity: "layout-node",
              name: "Long layout test node",
              country: "US",
              type: "vless",
              server: "very-long-server-name.example.org",
              port: "443",
              network: "ws",
              speed: 12345678,
              addedAt: "2026-10-20T01:02:03Z",
              checkedAt: "2026-10-26T04:05:06Z",
              ippure: {
                status: "success",
                asnOrganization: organization,
                source: "原生 IP",
                attributes: ["数据中心", "企业网络"],
                score: 20,
                label: "纯净",
                humanPercent: 99.99,
                botPercent: 0.01,
              },
            },
          ],
        },
      }),
    );
    await page.goto("/");
    const row = page.locator(".desktop-table tbody tr").first();
    await expect(row).toBeVisible();
    const asn = row.locator('[data-column="asnOrganization"] span');
    await expect(asn).toHaveAttribute("title", organization);
    expect(await asn.evaluate((el) => el.scrollWidth > el.clientWidth)).toBe(
      true,
    );
    expect(await asn.evaluate((el) => getComputedStyle(el).textOverflow)).toBe(
      "ellipsis",
    );
    for (const value of await row.locator(".ippure-ratio-values span").all()) {
      expect(
        await value.evaluate((el) => {
          const bounds = el.getBoundingClientRect();
          const parent = el.parentElement!.getBoundingClientRect();
          return bounds.right <= parent.right + 1 && bounds.left >= parent.left;
        }),
      ).toBe(true);
    }
    const container = page.locator(
      '.desktop-table [data-slot="table-container"]',
    );
    const bounds = (await container.boundingBox())!;
    for (const column of ["time", "copy"]) {
      const cell = row.locator(`[data-column="${column}"]`);
      const box = (await cell.boundingBox())!;
      expect(box.x).toBeGreaterThanOrEqual(bounds.x);
      expect(box.x + box.width).toBeLessThanOrEqual(
        bounds.x + bounds.width + 1,
      );
    }
    for (const time of await row.locator("time").all()) {
      expect(
        await time.evaluate((el) => el.scrollWidth <= el.clientWidth),
      ).toBe(true);
    }
    await expect(
      row.locator('[data-column="copy"] button'),
    ).toHaveAccessibleName("复制 Long layout test node 节点链接");
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
    ).toBe(true);
    await container.evaluate((el) => {
      el.scrollLeft = el.scrollWidth;
    });
    const copy = (await row
      .locator('[data-column="copy"] button')
      .boundingBox())!;
    expect(copy.x + copy.width).toBeLessThanOrEqual(width);
    if (width === 1440)
      await page.screenshot({
        path: "/tmp/node-table-layout.png",
        fullPage: true,
      });
  });
}
