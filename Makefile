# Developer and release tasks for gta. `make help` lists the targets.

GO         ?= go
GORELEASER ?= goreleaser
BIN        ?= bin/gta
PKG        := github.com/cmyolo441-coder/go-terminal-agent
VERSION_PKG := $(PKG)/internal/version

# Version metadata, the same values goreleaser injects on a release. Between
# tags git describe gives a semantic pre-release such as 1.2.3-4-gabc1234-dirty;
# with no tag at all the version stays the development default.
VERSION ?= $(or $(shell git describe --tags --match 'v[0-9]*' --dirty 2>/dev/null | sed 's/^v//'),0.1.0-dev)
COMMIT  ?= $(or $(shell git rev-parse HEAD 2>/dev/null),none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(VERSION_PKG).Version=$(VERSION) \
	-X $(VERSION_PKG).Commit=$(COMMIT) \
	-X $(VERSION_PKG).Date=$(DATE)

# SBOMs need syft; skip them in a local snapshot when it is not installed.
SNAPSHOT_SKIP := $(if $(shell command -v syft 2>/dev/null),,--skip=sbom)

.PHONY: help build install test race vet fmt lint release-check release-snapshot homebrew-formula clean

help: ## list the targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## build ./bin/gta with version metadata
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/gta

install: ## go install gta with version metadata
	CGO_ENABLED=0 $(GO) install -trimpath -ldflags '$(LDFLAGS)' ./cmd/gta

test: ## run the tests with the race detector, as CI does
	$(GO) test -race -count=1 ./...

vet: ## go vet for this platform and for Windows
	$(GO) vet ./...
	GOOS=windows $(GO) vet ./...

fmt: ## format the Go sources
	gofmt -w .

lint: vet ## gofmt check, go vet, shellcheck and golangci-lint (when installed)
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	sh -n scripts/install.sh
	@if command -v shellcheck >/dev/null 2>&1; then shellcheck -s sh scripts/install.sh; else echo "shellcheck not installed; skipped"; fi
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; else echo "golangci-lint not installed; skipped"; fi

release-check: ## validate .goreleaser.yaml
	$(GORELEASER) check

release-snapshot: ## build every release archive into ./dist without publishing
	$(GORELEASER) release --snapshot --clean $(SNAPSHOT_SKIP)

# Fill packaging/homebrew/gta.rb from dist/checksums.txt (after a release or
# a snapshot), e.g. make homebrew-formula VERSION=1.2.3
homebrew-formula: ## write dist/gta.rb from the formula template and checksums
	@test -f dist/checksums.txt || { echo "dist/checksums.txt missing; run goreleaser first"; exit 1; }
	@set -e; v='$(VERSION)'; v="$${v#v}"; \
	echo "$$v" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$$' || { echo "VERSION=$$v is not a release version" >&2; exit 1; }; \
	sum() { s="$$(awk -v f="gta_$${v}_$$1.tar.gz" '$$2 == f { print $$1 }' dist/checksums.txt)"; \
		echo "$$s" | grep -Eq '^[0-9a-f]{64}$$' || { echo "no checksum for gta_$${v}_$$1.tar.gz in dist/checksums.txt" >&2; exit 1; }; \
		echo "$$s"; }; \
	da="$$(sum darwin_arm64)"; di="$$(sum darwin_amd64)"; la="$$(sum linux_arm64)"; li="$$(sum linux_amd64)"; \
	sed -e "s/@VERSION@/$$v/g" \
		-e "s/@SHA256_DARWIN_ARM64@/$$da/" -e "s/@SHA256_DARWIN_AMD64@/$$di/" \
		-e "s/@SHA256_LINUX_ARM64@/$$la/" -e "s/@SHA256_LINUX_AMD64@/$$li/" \
		packaging/homebrew/gta.rb > dist/gta.rb; \
	echo "wrote dist/gta.rb for $$v"

clean: ## remove build output
	rm -rf bin dist
