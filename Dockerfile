# 多阶段构建：编译阶段有完整工具链，运行阶段只有一个静态二进制。
# 最终镜像约 20MB（Python 版约 250MB），且不含 shell / pip / 包管理器 ——
# 没有 shell 的镜像意味着拿到 RCE 也没法就地执行命令，安全面小一圈。
FROM golang:1.27 AS build

WORKDIR /src
# 先只拷 go.mod/go.sum，让依赖层可缓存：改代码不必重下依赖
COPY go.mod go.sum ./
RUN go env -w GOPROXY=https://goproxy.cn,direct && go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/adapter ./cmd/adapter

FROM gcr.io/distroless/static:nonroot

COPY --from=build /out/adapter /adapter

# 账本目录：必须以 nonroot（uid 65532）可写。
# 踩坑记录：目录不可写时服务一切正常、音频照常返回，但 /v1/usage 永远是 0，
# 只有 docker logs 里那行 "billing log write failed" 能看出来。
WORKDIR /app
VOLUME ["/app/logs"]

ENV ADAPTER_HOST=0.0.0.0 \
    ADAPTER_PORT=8123 \
    ADAPTER_BILLING_LOG_DIR=/app/logs/billing

EXPOSE 8123
USER nonroot:nonroot

ENTRYPOINT ["/adapter"]
