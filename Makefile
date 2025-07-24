GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get
GOMOD=$(GOCMD) mod
GOFMT=$(GOCMD) fmt
GOVET=$(GOCMD) vet
GOLINT=golint

BINARY_NAME=kioshun
PACKAGE_NAME=github.com/unkn0wn-root/kioshun

.PHONY: all
all: test build

.PHONY: build
build:
	$(GOBUILD) -v ./...

.PHONY: test
test:
	$(GOTEST) -v -race -coverprofile=coverage.out ./...

.PHONY: bench-deps
bench-deps:
	cd _benchmarks && $(GOMOD) tidy && $(GOMOD) download

.PHONY: bench-runner
bench-runner: bench-deps
	cd _benchmarks && timeout 600 go run benchmark_runner.go

.PHONY: bench
bench: bench-deps
	cd _benchmarks && $(GOTEST) -bench=. -benchmem -run=^$$ ./...

.PHONY: bench-full
bench-full: bench-deps
	cd _benchmarks && $(GOTEST) -bench=. -benchmem -benchtime=10s -run=^$$ ./...

.PHONY: bench-compare
bench-compare: bench-deps
	@echo "Running performance comparison..."
	cd _benchmarks && $(GOTEST) -bench=BenchmarkCacheShardComparison -benchmem -run=^$$ ./...
	cd _benchmarks && $(GOTEST) -bench=BenchmarkCacheEvictionPolicyComparison -benchmem -run=^$$ ./...

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
stress-test: bench-deps
	@echo "Running stress test..."
	cd _benchmarks && $(GOTEST) -bench=BenchmarkCacheScalability -benchmem -benchtime=30s -run=^$$ ./...

.PHONY: mem-analysis
mem-analysis: bench-deps
	@echo "Running memory usage analysis..."
	cd _benchmarks && $(GOTEST) -bench=BenchmarkCacheMemoryUsage -benchmem -run=^$$ ./...

.PHONY: install-tools
install-tools:
	$(GOGET) -u golang.org/x/lint/golint
	$(GOGET) -u golang.org/x/tools/cmd/goimports
	$(GOGET) -u github.com/kisielk/errcheck
