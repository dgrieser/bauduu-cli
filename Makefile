APP := bauduu
BINDIR ?= /usr/local/bin

# Build identity, reported by `bauduu --version`. Overridable, e.g.
# `make build VERSION=v0.1.0`; outside a git checkout COMMIT is empty.
VERSION ?= dev
COMMIT ?= $(shell h=$$(git rev-parse --short=7 HEAD 2>/dev/null) && printf %s "$$h" && git status --porcelain | grep -q . && printf %s -dirty)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

# Release targets: linux amd64/arm64/armv7 and darwin arm64.
PLATFORMS := linux/amd64 linux/arm64 linux/arm/7 darwin/arm64

.DEFAULT_GOAL := build

.PHONY: help build cross test race vet fmt tidy clean install snapshot release

help: ## Show available targets
	@awk 'BEGIN {FS = ":.*## "}; /^[a-zA-Z0-9_.-]+:.*## / {printf "%-10s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build the binary into ./bin
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o bin/$(APP) .

cross: ## Build static binaries for all release platforms into ./bin (no goreleaser needed)
	mkdir -p bin
	@set -e; for p in $(PLATFORMS); do \
		os=$${p%%/*}; rest=$${p#*/}; arch=$${rest%%/*}; arm=; out=bin/$(APP)_$${os}_$${arch}; \
		case $$rest in */*) arm=$${rest#*/}; out=$${out}v$$arm;; esac; \
		echo "build $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch GOARM=$$arm go build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o $$out .; \
	done

test: ## Run the test suite
	go test ./...

race: ## Run the tests with the race detector
	go test -race ./...

vet: ## Run go vet
	go vet ./...

fmt: ## Format Go sources
	gofmt -w .

tidy: ## Tidy go.mod/go.sum
	go mod tidy

clean: ## Remove build output
	rm -rf bin dist

install: build ## Install the binary to $(BINDIR)
	install -d $(BINDIR)
	install -m 0755 bin/$(APP) $(BINDIR)/$(APP)

snapshot: ## Build all release targets with goreleaser, without publishing
	goreleaser build --snapshot --clean

release: ## Full goreleaser release (needs a tag and GITHUB_TOKEN)
	goreleaser release --clean
