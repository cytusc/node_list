#!/usr/bin/env bash
# 把前端构建产物（仓库根 dist/）同步进 webman/public/。
#
# webman 的静态根是 public/，而前端仍然是原来那套 Vite 构建（产物在 dist/）。
# 这两个目录之间需要一次同步，否则会出现「接口通了、打开首页 404」。
# 部署（Dockerfile）另有一条路：构建阶段直接把 dist 拷进镜像的 public/，
# 本地开发跑 `npm run dev` 时需要的是这个脚本。
#
# 用法：
#   npm run build && bash webman/tools/sync-frontend.sh
#
# 说明：
#   - public/static 是框架约定的目录，默认保留不删（里面有占位，也方便放 favicon）。
#   - 同步是「以 dist 为准」的覆盖：dist 里没有的旧产物会被清掉，
#     否则改名后的旧 chunk 会一直留在服务器上被浏览器缓存命中。
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PUBLIC="$(cd "$HERE/.." && pwd)/public"
ROOT="$(cd "$HERE/../.." && pwd)"
DIST="$ROOT/dist"

if [[ ! -f "$DIST/index.html" ]]; then
  printf '错误：%s 不存在，请先在仓库根执行 npm run build\n' "$DIST/index.html" >&2
  exit 1
fi

mkdir -p "$PUBLIC/static"

if command -v rsync >/dev/null 2>&1; then
  rsync -a --delete --exclude 'static/' "$DIST"/ "$PUBLIC"/
else
  # 没有 rsync 时退化为「先清空除 static 外的一切，再整份复制」。
  find "$PUBLIC" -mindepth 1 -maxdepth 1 ! -name 'static' -exec rm -rf {} +
  cp -a "$DIST"/. "$PUBLIC"/
fi

printf '已同步 %s -> %s\n' "$DIST" "$PUBLIC"
ls -1 "$PUBLIC" | head -10
