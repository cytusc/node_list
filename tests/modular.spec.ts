import { test, expect } from "./authenticated-fixture";
import { fulfillRoute } from "./results-fixture";

test("refreshes results without closing node details", async ({
  page,
}) => {
  let reads = 0;
  const serviceRequests: string[] = [];
  page.on("request", (request) => {
    if (new URL(request.url()).pathname === "/api/services") {
      serviceRequests.push(request.url());
    }
  });
  const sorts: string[] = [];
  await page.route("**/api/results?**", (route) => {
    reads++;
    sorts.push(new URL(route.request().url()).searchParams.get("sort") || "");
    return fulfillRoute(route, {
      json: {
        speedTest: true,
        mediaCheck: false,
        nodes: [
          {
            name: "Residential node",
            baseName: reads > 1 ? "Updated node" : "Residential node",
            type: "http",
            server: "example.test",
            port: "443",
            speed: 100,
            connectivity: {
              reachable: true,
              checkedAt: new Date().toISOString(),
            },
            exit: {
              ip: "8.8.4.4",
              version: 1,
              verified: true,
              checkedAt: new Date().toISOString(),
            },
          },
        ],
      },
    });
  });
  await page.goto("/");
  await expect(page.getByLabel("排序方式")).toHaveValue("preferred-desc");
  expect(sorts[0]).toBe("preferred");
  await page.locator(".node-name").click();
  await expect(page.getByRole("dialog")).toContainText("节点检查");
  await expect(page.getByRole("dialog")).toContainText("可用");
  const readsBeforePoll = reads;
  await expect
    .poll(() => reads, { timeout: 10000 })
    .toBeGreaterThan(readsBeforePoll);
  await expect(page.getByRole("dialog")).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.locator(".node-row")).toContainText("Updated node");
  await page.getByLabel("排序方式").selectOption("name-asc");
  await expect.poll(() => sorts.at(-1)).toBe("name");
  await expect(page.locator(".service-status")).toHaveCount(0);
  expect(serviceRequests).toEqual([]);
});
