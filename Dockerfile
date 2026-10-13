# ---------------------------------------------------------------------------
# memos-md — one-shot image build for this fork.
#
# Self-contained: it builds the React SPA and the Go binary, so a plain
# `docker build .` works without running pnpm or go locally.
#
#   docker build -t memos-md .
#   docker run -d --name memos-md -p 5230:5230 -v "$PWD/data:/var/opt/memos" memos-md
#
# or: docker compose -f docker-compose.memofiles.yml up -d --build
#
# Upstream's scripts/Dockerfile assumes the SPA was already built into
# server/frontend/dist (its release workflow runs `pnpm release` first) and
# ignores web/ in the build context. This stage builds the SPA here instead, so
# cloning the repository is enough.
# ---------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM node:24-alpine AS frontend
WORKDIR /src/web
RUN corepack enable
# pnpm-workspace.yaml declares the patched dependency and allowBuilds entries the
# lockfile was written with, and patches/ holds the patch itself. Installing with
# only package.json and the lockfile fails with ERR_PNPM_LOCKFILE_CONFIG_MISMATCH.
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml ./
COPY web/patches/ ./patches/
RUN --mount=type=cache,target=/root/.local/share/pnpm/store \
    pnpm install --frozen-lockfile
# The SPA imports the generated proto types from web/src/types/proto (tracked in
# git), so the frontend sources are all it needs; the output path is the one the
# `pnpm release` script uses, which go:embed then bakes into the binary.
COPY web/ ./
RUN node ./node_modules/vite/bin/vite.js build --mode release --outDir=../server/frontend/dist --emptyOutDir

FROM --platform=$BUILDPLATFORM golang:1.27.0-alpine AS backend
WORKDIR /backend-build
RUN apk add --no-cache git ca-certificates
# Module proxy. proxy.golang.org is unreachable from some networks, so the
# module path goes through goproxy.cn first and falls back to the official proxy
# and then to direct fetches. Override for your own mirror:
#   docker build --build-arg GOPROXY=https://your.proxy .
ARG GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
COPY --from=frontend /src/server/frontend/dist ./server/frontend/dist

ARG TARGETOS TARGETARCH VERSION=dev COMMIT=unknown
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build \
      -trimpath \
      -ldflags="-s -w -X github.com/usememos/memos/internal/version.Version=${VERSION} -X github.com/usememos/memos/internal/version.Commit=${COMMIT} -extldflags '-static'" \
      -tags netgo,osusergo \
      -o memos \
      ./cmd/memos

FROM alpine:3.21 AS monolithic

RUN apk add --no-cache tzdata ca-certificates su-exec && \
    addgroup -g 10001 -S nonroot && \
    adduser -u 10001 -S -G nonroot -h /var/opt/memos nonroot && \
    mkdir -p /var/opt/memos /usr/local/memos && \
    chown -R nonroot:nonroot /var/opt/memos

COPY --from=backend /backend-build/memos /usr/local/memos/memos
COPY --from=backend --chmod=755 /backend-build/scripts/entrypoint.sh /usr/local/memos/entrypoint.sh

USER root
WORKDIR /var/opt/memos

VOLUME /var/opt/memos

ENV TZ="UTC" \
    MEMOS_PORT="5230" \
    MEMOS_MEMO_FILES="true"

EXPOSE 5230

ENTRYPOINT ["/usr/local/memos/entrypoint.sh", "/usr/local/memos/memos"]
