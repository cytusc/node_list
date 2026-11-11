<?php
/**
 * 静态文件服务。
 *
 * 前端构建产物（Vite 的 dist/）同步到 public/ 下，webman 的静态文件中间件
 * 会先于路由匹配处理。未命中的路径回落到 config/route.php 的 fallback。
 *
 * 同步用 tools/sync-frontend.sh（`npm run build` 之后执行）；部署细节见
 * webman/README.md。注意 /（目录请求）不会自动映射到 index.html，
 * 那一步由 StaticController 显式处理。
 */

return [
    'enable' => true,
    'middleware' => [app\middleware\StaticCache::class],
];
