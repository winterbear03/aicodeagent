# 第一阶段：编译 Go 程序
FROM golang:1.26-alpine AS builder

ENV GOPROXY=https://goproxy.cn,direct
ENV CGO_ENABLED=0
ENV GOOS=linux

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN go build -ldflags="-s -w" -o server agent.go

# 第二阶段：运行环境
FROM alpine:latest

# 无需额外安装，alpine 基础镜像已包含必要的 CA 证书
WORKDIR /root/

COPY --from=builder /app/server .
COPY --from=builder /app/frontend ./frontend

EXPOSE 8080

CMD ["./server"]