import { test as base } from "@playwright/test";
export { expect } from "@playwright/test";

// Existing dashboard tests exercise an already authenticated user.
export const test = base.extend({
  page: async ({ page }, use) => {
    await page.route("**/api/auth/session", (route) =>
      route.fulfill({ json: { authenticated: true, configured: true } }),
    );
    await use(page);
  },
});
