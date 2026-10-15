#!/bin/sh
# Older Compose deployments invoke Node for patrol and its health check. Keep
# those two invocations working when only the image is upgraded to Go.
case "${1:-}" in
  server/github-patrol-service.js|./server/github-patrol-service.js|/app/server/github-patrol-service.js)
    shift
    exec github-patrol "$@"
    ;;
  *)
    printf '%s\n' 'This image uses Go. The legacy node command only supports server/github-patrol-service.js (including --health).' >&2
    exit 127
    ;;
esac
