.PHONY: setup fmt lint test build static ci

# Install dependencies: Go modules, plus the IaC DSL for .railway/railway.ts.
setup:
	go mod download
	@command -v bun >/dev/null 2>&1 && bun install || echo "bun not found; skipping IaC deps (only needed for 'railway config apply')"

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './.git/*')

# gofmt check + go vet, no external tools.
lint:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "gofmt needed on:"; echo "$$unformatted"; exit 1; fi
	go vet ./...

test:
	go test ./...

build:
	go build ./...

# Mirror the container build: pure-Go, no cgo.
static:
	CGO_ENABLED=0 GOOS=linux go build ./...

ci: lint test static
