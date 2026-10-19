import { defineConfig } from "@playwright/test";
import { existsSync } from "node:fs";

const port = Number(process.env.PLAYWRIGHT_PORT || 5173);

export default defineConfig({
  testDir: "./tests",
  workers: 1,
  use: {
    baseURL: `http://127.0.0.1:${port}`,
    viewport: { width: 1440, height: 1080 },
    launchOptions: {
      executablePath:
        process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE ||
        (existsSync("/snap/bin/chromium") ? "/snap/bin/chromium" : undefined),
      args: ["--no-sandbox", "--disable-dev-shm-usage"],
    },
  },
  webServer: {
    command: `npx concurrently -k "npm:dev:api" "vite --host 127.0.0.1 --port ${port} --strictPort"`,
    url: `http://127.0.0.1:${port}`,
    reuseExistingServer: !process.env.CI,
  },
});
