#!/bin/sh
# 容器入口：read_only 下 /app/webman/runtime 是 tmpfs（空的），不继承镜像里的
# 目录结构。webman 启动要写 runtime/logs（Monolog）、runtime/cache（PSR-16 文件
# 缓存）、runtime/container（DI 容器缓存），必须先建好，否则首条日志/缓存写入
# 就抛异常、worker 起不来、/api/health 永远 503，冒烟与线上都会挂。
# 这里只 mkdir，不碰镜像里其它只读路径；exec 把信号透传给最终命令，保证
# php start.php / node-worker 都能正常收到 SIGTERM 优雅退出。
set -e
mkdir -p /app/webman/runtime/logs /app/webman/runtime/cache /app/webman/runtime/container /app/webman/runtime/views
exec "$@"
