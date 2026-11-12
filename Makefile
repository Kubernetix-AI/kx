GO      ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//')
COMMIT  ?= $(shell git rev-parse HEAD 2>/dev/null)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG     := github.com/kubernetix-ai/kx/internal/version
LDFLAGS := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)
TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64
FUZZTIME ?= 30s

.PHONY: build test lint vuln fmt cross fuzz snapshot clean

build: ## Build ./kx for this computer
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o kx .

test: ## Run every test with the race detector
	$(GO) test -race ./...

lint: ## gofmt, go vet and golangci-lint
	@test -z "$$(gofmt -l .)" || { echo "These files need gofmt:"; gofmt -l .; exit 1; }
	$(GO) vet ./...
	golangci-lint run ./...

vuln: ## Check dependencies for known vulnerabilities
	govulncheck ./...

fmt: ## Format the code
	gofmt -w .

cross: ## Build all six release targets into dist/
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; ext=; [ "$$os" = windows ] && ext=.exe; \
		echo "$$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/kx-$$os-$$arch$$ext . || exit 1; \
	done

fuzz: ## Run each fuzz test for FUZZTIME (default 30s)
	$(GO) test ./internal/api -run '^$$' -fuzz '^FuzzCheckToken$$' -fuzztime $(FUZZTIME)
	$(GO) test ./internal/api -run '^$$' -fuzz '^FuzzParseErrorBody$$' -fuzztime $(FUZZTIME)
	$(GO) test ./internal/config -run '^$$' -fuzz '^FuzzParseUser$$' -fuzztime $(FUZZTIME)
	$(GO) test ./internal/config -run '^$$' -fuzz '^FuzzParseProject$$' -fuzztime $(FUZZTIME)
	$(GO) test ./internal/docker -run '^$$' -fuzz '^FuzzParseDigest$$' -fuzztime $(FUZZTIME)
	$(GO) test ./internal/docker -run '^$$' -fuzz '^FuzzParseCachedCredential$$' -fuzztime $(FUZZTIME)
	$(GO) test ./internal/deploy -run '^$$' -fuzz '^FuzzExposedPort$$' -fuzztime $(FUZZTIME)
	$(GO) test ./internal/deploy -run '^$$' -fuzz '^FuzzChangedBesidesKxJSON$$' -fuzztime $(FUZZTIME)
	$(GO) test ./internal/docker -run '^$$' -fuzz '^FuzzParseBuildxDriver$$' -fuzztime $(FUZZTIME)
	$(GO) test ./internal/docker -run '^$$' -fuzz '^FuzzParseDockerContext$$' -fuzztime $(FUZZTIME)
	$(GO) test ./internal/safetext -run '^$$' -fuzz '^FuzzNeutralize$$' -fuzztime $(FUZZTIME)
	$(GO) test ./internal/safetext -run '^$$' -fuzz '^FuzzEscapeJSON$$' -fuzztime $(FUZZTIME)

snapshot: ## Build release archives locally without publishing (needs goreleaser)
	goreleaser release --snapshot --clean

clean:
	rm -rf dist kx kx.exe
