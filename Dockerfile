# 构建阶段
FROM golang:1.27-alpine AS build

WORKDIR /src

# 先拷依赖清单，利用 Docker 层缓存：依赖没变就不用重新下载
COPY go.mod go.sum ./
RUN go mod download

# 再拷源码并构建。CGO_ENABLED=0 因为 modernc.org/sqlite 是纯 Go 实现，不需要 gcc
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/webhook-gw ./cmd/webhook-gw

# 运行阶段
FROM alpine:3.20

WORKDIR /app
COPY --from=build /out/webhook-gw /app/webhook-gw

# 非 root 运行，最小权限
RUN adduser -D -u 10001 appuser
USER appuser

# 数据库文件放 /data，挂载卷后重启不丢
VOLUME ["/data"]
EXPOSE 8080

ENTRYPOINT ["/app/webhook-gw"]
CMD ["-db", "/data/data.db", "-port", "8080"]
