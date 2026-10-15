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

# The backend is Go. Cross-compile it on the builder host for the same reason:
# CGO is disabled, so the target architecture needs no emulated toolchain.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS backend
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend ./
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
      go build -trimpath -ldflags "-s -w" -o /out/node-explorer ./cmd/node-explorer \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
      go build -trimpath -ldflags "-s -w" -o /out/node-worker ./cmd/node-worker

# Runtime: alpine plus a CA bundle, nothing else. The Go binaries are static,
# so no Node runtime or production dependencies are shipped. The service binds
# no privileged port and runs as uid 1000, matching the data volumes.
FROM alpine:3.21
RUN apk add --no-cache ca-certificates \
 && adduser -D -u 1000 -h /home/node node \
 && mkdir -p /app \
 && chown -R node:node /app
WORKDIR /app
ENV HOST=0.0.0.0 API_PORT=3001 SUBS_CHECK_DIR=/data/sub
COPY --from=frontend --chown=node:node /app/dist ./dist
COPY --from=backend --chown=node:node /out/node-explorer /usr/local/bin/node-explorer
COPY --from=backend --chown=node:node /out/node-worker /usr/local/bin/node-worker
USER node
EXPOSE 3001
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:3001/api/health || exit 1
CMD ["/usr/local/bin/node-explorer"]
