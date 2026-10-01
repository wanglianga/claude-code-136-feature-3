# syntax=docker/dockerfile:1

# ---- 构建阶段 ----
FROM golang:1.21-alpine AS builder
WORKDIR /src

# 先拉依赖以利用层缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# 静态构建（CGO_ENABLED=0），Swagger UI 与 OpenAPI 已通过 go:embed 内嵌
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/passgate .

# ---- 运行阶段 ----
FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata wget && \
    adduser -D -u 10001 appuser
WORKDIR /app
COPY --from=builder /out/passgate /app/passgate
USER appuser
EXPOSE 8080
ENV TZ=Asia/Shanghai \
    REDIS_ADDR=redis:6379 \
    PORT=8080
ENTRYPOINT ["/app/passgate"]
