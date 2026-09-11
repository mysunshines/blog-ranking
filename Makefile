.PHONY: all build run test clean deps update proto docker docker-run lint fmt help api

SERVICE_SHORT_NAME=ranking
SERVICE_NAME=$(SERVICE_SHORT_NAME)-service
BINARY_NAME=$(SERVICE_NAME)
SRC_DIR=cmd/server
BIN_DIR=bin
# 容器端口映射（docker-run 使用）。与 docker-compose.yml 宿主机映射保持一致：
# HTTP 8086（探活）/ gRPC 9106（业务入口）/ Metrics 9097 不对外映射，由 Prometheus 内网抓取。
PORTS=8086:8086 9106:9106

GIT_VERSION      := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
VERSION_LDFLAGS := -X main.Version=$(GIT_VERSION)

MODULE := github.com/mysunshines/blog-$(SERVICE_SHORT_NAME)
PROTO_DIR := proto
PROTO_OUT := $(PROTO_DIR)/pb
PROTOC_OPTS := --go_out=. --go_opt=module=$(MODULE) --go-grpc_out=. --go-grpc_opt=module=$(MODULE)

all: build

deps:
	go mod tidy
	go mod download

update:
	go get -u ./...

# 生成 gRPC 代码（仅纯 gRPC，无 google.api.http 注解，不生成 grpc-gateway）。
# 除本服务主 proto 外，还需生成共享 decorator 契约（decorator.v0.Decorator），
# 供 GetRanking 回调业务服务装饰榜单成员时使用。
proto:
	@if [ -d $(PROTO_DIR) ]; then \
		mkdir -p $(PROTO_OUT) && \
		protoc -I $(PROTO_DIR) $(PROTOC_OPTS) $(PROTO_DIR)/$(SERVICE_SHORT_NAME).proto && \
		protoc -I $(PROTO_DIR) $(PROTOC_OPTS) $(PROTO_DIR)/$(SERVICE_SHORT_NAME)_ingest.proto && \
		protoc -I $(PROTO_DIR) $(PROTOC_OPTS) $(PROTO_DIR)/decorator/v0/decorator.proto; \
	else \
		echo "==> No proto directory"; \
	fi

# 生成对外 API 文档（来源：proto，网关按 /api/v1/<svc>/<snake_method> 反射代理）
# 生成文件：服务根目录 api.md（含 url / method / headers / request / response / curl 示例）
# Go 版生成器（Python 版 gen_api_doc.py 保留作为备选）
API_GEN_DIR := $(dir $(lastword $(MAKEFILE_LIST)))/../infra/scripts/gen_api_doc
API_GEN := $(API_GEN_DIR)/genapidoc
api:
	@cd "$(API_GEN_DIR)" && go build -o genapidoc .
	@"$(API_GEN)" --proto $(PROTO_DIR)/$(SERVICE_SHORT_NAME).proto --out api.md

lint:
	golangci-lint run

fmt:
	go fmt ./...
	@which goimports > /dev/null 2>&1 || GO111MODULE=on go install golang.org/x/tools/cmd/goimports@latest
	goimports -local common -w .

build: deps proto fmt
	mkdir -p $(BIN_DIR)
	go build -ldflags "$(VERSION_LDFLAGS)" -o $(BIN_DIR)/$(BINARY_NAME) $(SRC_DIR)/main.go

run:
	go run $(SRC_DIR)/main.go

test:
	go test -v ./...

clean:
	rm -f $(BIN_DIR)/*

docker-build:
	docker build --build-arg GIT_VERSION=$(GIT_VERSION) -t $(SERVICE_NAME):latest .

docker-run:
	docker run -p $(PORTS) $(SERVICE_NAME):latest
