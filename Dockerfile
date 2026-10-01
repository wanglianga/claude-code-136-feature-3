# syntax=docker/dockerfile:1
# ---------- 构建阶段 ----------
FROM golang:1.19-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/passgate .

# ---------- 运行阶段 ----------
FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata && \
    cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime && \
    echo "Asia/Shanghai" > /etc/timezone
WORKDIR /app
COPY --from=builder /out/passgate /app/passgate
EXPOSE 8080
ENTRYPOINT ["/app/passgate"]
