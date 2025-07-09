GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get
GOMOD=$(GOCMD) mod
GOFMT=$(GOCMD) fmt
GOVET=$(GOCMD) vet
GOLINT=golint

BINARY_NAME=cago
PACKAGE_NAME=github.com/david0/cago

.PHONY: all
all: test build

.PHONY: build
build:
	$(GOBUILD) -v ./...

.PHONY: test
test:
	$(GOTEST) -v -race -coverprofile=coverage.out ./...

.PHONY: bench
bench:
	$(GOTEST) -bench=. -benchmem -run=^$$ ./...

.PHONY: bench-full
bench-full:
	$(GOTEST) -bench=. -benchmem -benchtime=10s -run=^$$ ./...

.PHONY: bench-compare
bench-compare:
	@echo "Running performance comparison..."
	$(GOTEST) -bench=BenchmarkCacheShardComparison -benchmem -run=^$$ ./...
	$(GOTEST) -bench=BenchmarkCacheEvictionPolicyComparison -benchmem -run=^$$ ./...

.PHONY: lint
lint:
	$(GOVET) ./...
	$(GOLINT) ./...

.PHONY: fmt
fmt:
	$(GOFMT) ./...

.PHONY: clean
clean:
	$(GOCLEAN)
	rm -f $(BINARY_NAME)

.PHONY: tidy
tidy:
	$(GOMOD) tidy

.PHONY: deps
deps:
	$(GOMOD) download

.PHONY: check
check: fmt lint test

.PHONY: stress-test
stress-test:
	@echo "Running stress test..."
	$(GOTEST) -bench=BenchmarkCacheScalability -benchmem -benchtime=30s -run=^$$ ./...

.PHONY: mem-analysis
mem-analysis:
	@echo "Running memory usage analysis..."
	$(GOTEST) -bench=BenchmarkCacheMemoryUsage -benchmem -run=^$$ ./...

.PHONY: install-tools
install-tools:
	$(GOGET) -u golang.org/x/lint/golint
	$(GOGET) -u golang.org/x/tools/cmd/goimports
	$(GOGET) -u github.com/kisielk/errcheck
