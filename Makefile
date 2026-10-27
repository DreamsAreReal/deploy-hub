GO ?= go
OUT_DIR ?= $(or $(TMPDIR),/tmp)/deploy-hub-build
TOOLS_DIR ?= $(OUT_DIR)/toolchain
GOOS ?= linux
GOARCH ?= amd64
APP ?= deploy-hub
REPO ?= DreamsAreReal/deploy-hub
COMMIT ?= dev
ENGINE = -X github.com/DreamsAreReal/deploy-hub/internal/gate/mutation.EngineVersion=$(shell $(GO) run ./cmd/deploy-hub engine-version)

.PHONY: build test lint toolchain gate precheck

build:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build -trimpath -ldflags="-s -w -X github.com/DreamsAreReal/deploy-hub/internal/cli.Commit=$(COMMIT) $(ENGINE)" -o $(OUT_DIR)/deploy-hub ./cmd/deploy-hub

test:
	$(GO) test -race -count=1 ./...

lint:
	golangci-lint run --config policy/golangci.yml ./...

toolchain:
	$(GO) build -trimpath -ldflags="$(ENGINE)" -o $(TOOLS_DIR)/deploy-hub ./cmd/deploy-hub
	$(TOOLS_DIR)/deploy-hub tools --manifest policy/tools.yaml --dir $(TOOLS_DIR)

gate:
	$(GO) run ./cmd/deploy-hub gate --app $(APP) --repo $(REPO) --commit $$(git rev-parse HEAD) --policy $(CURDIR)/policy/gate.yaml --src $(CURDIR) --out $(OUT_DIR)/gate

precheck:
	$(GO) run ./cmd/deploy-hub precheck --app $(APP) --repo $(REPO) --commit $$(git rev-parse HEAD) --policy $(CURDIR)/policy/gate.yaml --src $(CURDIR) --out $(OUT_DIR)/precheck
