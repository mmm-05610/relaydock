# Go 网关单二进制：~10MB，无运行时依赖
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY gateway/go.mod gateway/go.sum ./
RUN go mod download
COPY gateway/ .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

FROM alpine:3.20
# ca-certificates：上游均为 HTTPS；tzdata：用量统计按本地时区
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/gateway /usr/local/bin/gateway
# config.yaml 由部署时挂载（渠道种子数据）；面板静态文件挂载到 /navpage
ENV STATIC_DIR=/navpage
WORKDIR /app
EXPOSE 8080
ENTRYPOINT ["gateway"]
