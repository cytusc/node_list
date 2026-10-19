#!/bin/sh
# 容器入口：read_only 下 /app/webman/runtime 是 tmpfs（空的），不继承镜像里的
# 目录结构。webman（PHP）启动要写 runtime/logs（Monolog）、runtime/cache
# （PSR-16 文件缓存）、runtime/container（DI 容器缓存），必须先建好，否则首条
# 日志/缓存写入就抛异常、/api/health 永远 503。
# 但同一镜像也跑 Go 的 node-worker（来源导入、测活、IP 属性、住宅恢复、生命周期），
# 它不碰 PHP runtime，read_only 下没有 /app/webman/runtime 的 tmpfs 也完全正常——
# 不能因为 mkdir 失败（Read-only file system）就把 node-worker 进程拖死。
# 因此只在真正启动 PHP 时才建这些目录。exec 把信号透传给最终命令，保证
# php start.php / node-worker 都能正常收到 SIGTERM 优雅退出。
case "${1:-}" in
  php|start.php)
    mkdir -p /app/webman/runtime/logs /app/webman/runtime/cache /app/webman/runtime/container /app/webman/runtime/views
    ;;
esac
exec "$@"
