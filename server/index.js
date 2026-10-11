import path from "node:path";
import { fileURLToPath } from "node:url";
import { createApp } from "./app.js";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const subDir = path.resolve(
  process.env.SUBS_CHECK_DIR ||
    (process.platform === "win32" ? "D:\\code\\sub" : "/mnt/d/code/sub"),
);
const app = createApp({
  subDir,
  resultsFile: process.env.RESULTS_FILE,
  distDir: path.join(root, "dist"),
});
const port = Number(process.env.API_PORT || 3001);
app.listen(port, process.env.HOST || "127.0.0.1", () =>
  console.log(
    `Node Explorer: http://localhost:${port}\nResults directory: ${subDir}`,
  ),
);
