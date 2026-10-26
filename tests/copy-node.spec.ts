import { test, expect } from "./authenticated-fixture";
import { fulfillRoute } from "./results-fixture";

test("copies the exact stored link and never substitutes a missing link with configuration", async ({
  page,
  context,
}) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  const original =
    "vless://original-id@example.org:443?security=tls&type=ws&path=%2Fnode#%E8%8A%82%E7%82%B9";
  await page.route("**/api/results?**", (route) =>
    fulfillRoute(route, {
      json: {
        speedTest: false,
        mediaCheck: false,
        nodes: [
          {
            identity: "stored-node",
            name: "Stored node",
            type: "vless",
            server: "example.org",
            port: "443",
            network: "ws",
            tls: true,
            udp: false,
            speed: 0,
          },
        ],
      },
    }),
  );
  let available = true;
  await page.route("**/api/nodes/link", (route) => {
    expect(route.request().postDataJSON()).toEqual({ identity: "stored-node" });
    return route.fulfill(
      available
        ? { json: { format: "link", content: original } }
        : { status: 422, json: { error: "此节点未保存原始链接。" } },
    );
  });
  await page.goto("/");
  await page.getByRole("button", { name: "复制 Stored node 节点链接" }).click();
  await expect(page.getByText("节点链接已复制", { exact: true })).toBeVisible();
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(
    original,
  );
  available = false;
  await page.getByRole("button", { name: "复制 Stored node 节点链接" }).click();
  await expect(
    page.getByText("此节点未保存原始链接。", { exact: true }),
  ).toBeVisible();
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(
    original,
  );
});
