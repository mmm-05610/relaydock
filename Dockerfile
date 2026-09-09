# 前端控制台：Vite + React 构建产物为纯静态文件
FROM node:22-alpine AS webbuild
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --registry=https://registry.npmmirror.com
COPY web/ .
RUN npm run build

# Go 网关单二进制：~10MB，无运行时依赖
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY gateway/go.mod gateway/go.sum ./
RUN go mod download
# 前端 dist 内嵌进二进制（单文件完整形态）
COPY --from=webbuild /src/web/dist /src/internal/console/dist
ARG VERSION=dev
COPY gateway/ .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X gateway/internal/gateway.Version=${VERSION}" -o /out/gateway ./cmd/gateway

FROM alpine:3.20
# ca-certificates：上游均为 HTTPS；tzdata：用量统计按本地时区
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/gateway /usr/local/bin/gateway
WORKDIR /app
EXPOSE 8080
ENTRYPOINT ["gateway"]
