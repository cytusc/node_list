# Frontend assets are platform-independent. Compile them on the builder host to
# avoid running the Vite/Rolldown toolchain under QEMU.
FROM --platform=$BUILDPLATFORM node:22-alpine AS frontend
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY index.html tsconfig.json vite.config.ts ./
COPY public ./public
COPY src ./src
RUN npm run build

# The collectors stay Go. Cross-compile them on the builder host for the same
# reason: CGO is disabled, so the target architecture needs no emulated toolchain.
# The HTTP API is PHP (webman) now, so only node-worker is built here.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS backend
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend ./
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
      go build -trimpath -ldflags "-s -w" -o /out/node-worker ./cmd/node-worker

# PHP dependencies come from the lock file only, so editing a source file does
# not invalidate this layer.
FROM composer:2 AS vendor
WORKDIR /app
COPY webman/composer.json webman/composer.lock ./
RUN composer install --no-dev --no-scripts --no-interaction --no-progress \
 && composer dump-autoload --optimize --no-dev

# Runtime: PHP with the extensions webman/Workerman needs:
#   pcntl     process control (Workerman requires it)
#   sockets   event transport
#   pdo_pgsql PostgreSQL driver
#   mbstring  multibyte string helpers used by the service layer
#   tzdata    时区数据库（默认 Asia/Shanghai，见 config/app.php default_timezone）
# The PHP API process and the Go node-worker share this image; compose passes a
# different command per service. The API process binds no privileged port and
# runs as uid 1000, matching the data volumes.
FROM php:8.4-cli-alpine
RUN apk add --no-cache $PHPIZE_DEPS linux-headers oniguruma-dev postgresql-dev tzdata \
 && docker-php-ext-install -j"$(nproc)" pcntl sockets pdo_pgsql mbstring \
 && docker-php-ext-enable opcache \
 && apk del $PHPIZE_DEPS linux-headers oniguruma-dev postgresql-dev \
 && adduser -D -u 1000 -h /home/node node \
 && mkdir -p /app/webman/runtime /app/webman/public \
 && chown -R node:node /app
WORKDIR /app/webman
# 时区与 config/app.php 的 default_timezone 默认值一致：面板按北京时间渲染。
# 注意这不是 PG 会话时区——库里是 timestamptz，查询两侧都不经过它。
# API_HOST 必须是 0.0.0.0：容器里监听回环的话，compose 的端口映射
# （DNAT 到容器网卡）打不进来，站点直接连不上。本地开发默认 127.0.0.1 更安全。
ENV API_HOST=0.0.0.0 API_PORT=3001 TZ=Asia/Shanghai SUBS_CHECK_DIR=/data/sub
COPY --from=vendor --chown=node:node /app/vendor ./vendor
COPY --chown=node:node webman/start.php webman/config webman/app webman/support ./
# Frontend build output is the static root (config/static.php).
COPY --from=frontend --chown=node:node /app/dist ./public
COPY --from=backend --chown=node:node /out/node-worker /usr/local/bin/node-worker
USER node
EXPOSE 3001
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:3001/api/health || exit 1
CMD ["php", "start.php", "start"]
