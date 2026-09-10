# syntax=docker/dockerfile:1.7

# oracle-keeper: Oracle Cloud Always Free 保活守护进程。
#
# 构建上下文两种布局都支持：
#   - monorepo 根目录（本地 docker-compose 的 context: ".."，go.work 让
#     go-common 以本地模块参与解析，与 reference-service 的构建方式一致）；
#   - 独立仓库 checkout（GitHub Actions 的 context: "."，go.mod 里的
#     已发布 go-common 版本经 proxy 解析）。

ARG GO_VERSION=1.26.6
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS builder

ARG TARGETPLATFORM
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
# Go module proxy. Default is Go's standard public proxy; override with
# --build-arg GOPROXY=https://goproxy.cn,direct when proxy.golang.org is
# unreachable (compose build.args wires this through).
ARG GOPROXY=https://proxy.golang.org,direct

WORKDIR /src
ENV GOTOOLCHAIN=local
ENV GOPROXY=$GOPROXY

# Bind the build context read-only so standalone modules and monorepos with
# local replace directives use the same build. Module and compiler caches stay
# outside that mount and remain reusable across builds. The builder runs on
# BUILDPLATFORM while the Go compiler targets TARGETPLATFORM without QEMU.
RUN --mount=type=bind,source=.,target=/workspace \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eu; \
    if [ "${TARGETOS}" != "linux" ]; then \
      echo "unsupported target OS: ${TARGETOS}; the runtime image is Linux" >&2; \
      exit 1; \
    fi; \
    case "${TARGETARCH}/${TARGETVARIANT}" in \
      amd64/) ;; \
      amd64/v1|amd64/v2|amd64/v3|amd64/v4) export GOAMD64="${TARGETVARIANT}" ;; \
      arm/|arm/v7) export GOARM=7 ;; \
      arm64/|arm64/v8|ppc64le/|s390x/) ;; \
      *) echo "unsupported target platform: ${TARGETPLATFORM}" >&2; exit 1; \
    esac; \
    mkdir -p /out; \
    cd /workspace; \
    if [ -d oracle-keeper ]; then cd oracle-keeper; fi; \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
      go build \
        -mod=readonly \
        -trimpath \
        -buildvcs=false \
        -ldflags="-s -w -buildid=" \
        -o /out/oracle-keeper \
        ./cmd/keeper; \
    cp config.example.yaml /out/config.yaml

FROM alpine:3.24
# ca-certificates: HTTPS 下载源; tzdata: cron 时区 (ORACLE_KEEPER_SCHEDULE_TIMEZONE)
RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app
COPY --from=builder /out/oracle-keeper /app/oracle-keeper
# configx 要求配置文件存在；全注释的示例文件 = 全部走默认值 + 环境变量覆盖
COPY --from=builder /out/config.yaml /app/config.yaml

# 守护进程需要写宿主机挂载的两个临时目录（root 属主）并主动出网，故以
# root uid 运行但剥掉全部 capabilities（见 docker-compose）。无端口、无其他权限。
ENTRYPOINT ["/app/oracle-keeper"]
