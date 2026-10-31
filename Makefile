.PHONY: build test vet dev dev-memory migrate seed fmt lint

GO ?= go

build:
	$(GO) build ./...

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

lint: vet

# 零依赖本地启动（无需 Docker / PostgreSQL）
dev-memory:
	$(GO) run ./cmd/server -store memory -seed

# 生产形态：先起基础设施，再迁移，最后启动
dev:
	docker compose up -d
	$(GO) run ./cmd/server -store postgres -migrate -seed

migrate:
	$(GO) run ./cmd/server -store postgres -migrate -no-dispatch -addr :0 2>/dev/null || true
	@echo "迁移由服务启动时执行（见 cmd/server/store.go）"

seed:
	$(GO) run ./cmd/server -store postgres -seed
