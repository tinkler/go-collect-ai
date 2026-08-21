# Dockerfile for collect-ai-server (Go + Gin + pgx)
# 多阶段 build: 编译 Go, 然后放到 alpine 跑

# ============= Stage 1: build =============
FROM golang:1.26.3-alpine AS builder
WORKDIR /src
# 装 CGO 依赖 (pgx 需要)
RUN apk add --no-cache gcc musl-dev
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 静态链接 (CGO_ENABLED=0) 避免 alpine 装 libc6-compat
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/collect-ai-server ./cmd/server

# ============= Stage 2: run =============
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata wget
WORKDIR /app
COPY --from=builder /out/collect-ai-server /app/collect-ai-server
# (PG 表自动 migrate, 不需要 SQL 文件目录)
# 健康检查
HEALTHCHECK --interval=10s --timeout=3s --retries=5 \
  CMD wget -qO- http://localhost:8089/api/v1/health || exit 1
EXPOSE 8089
ENV PORT=8089
ENV UPLOAD_DIR=/app/uploads
ENV PG_HOST=pg
ENV PG_PORT=5432
ENV PG_USER=postgres
ENV PG_PASSWORD=postgres
ENV PG_DATABASE=collectai
ENV AGENT_URL=http://cube-agent:8088
VOLUME ["/app/uploads"]
ENTRYPOINT ["/app/collect-ai-server"]
