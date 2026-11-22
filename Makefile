VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test bench lint alloc-gate run clean

build:
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/salottod ./cmd/salottod
	@if [ -d ./cmd/salotto ]; then go build -ldflags "$(LDFLAGS)" -o bin/salotto ./cmd/salotto; fi

test:
	go test ./... -race -count=1

bench:
	go test ./internal/ring -bench . -benchmem -run '^$$'
	@if grep -q 'func Benchmark' internal/facade/*_test.go 2>/dev/null; then \
		go test ./internal/facade -bench . -benchmem -run '^$$'; \
	else \
		echo "no facade benchmarks"; \
	fi

lint:
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then \
		echo "gofmt needed:"; \
		printf '%s\n' "$$out"; \
		exit 1; \
	fi
	go vet ./...

alloc-gate:
	@out="$$(go test ./internal/ring -bench . -benchmem -run '^$$')" || { printf '%s\n' "$$out"; exit 1; }; \
	printf '%s\n' "$$out"; \
	printf '%s\n' "$$out" | awk '/^Benchmark/ { if ($$0 !~ /(^|[[:space:]])0 B\/op([[:space:]]|$$)/ || $$0 !~ /(^|[[:space:]])0 allocs\/op([[:space:]]|$$)/) { print "alloc-gate: " $$0; bad=1 } } END { exit bad }'

run: build
	./bin/salottod -config config.example.toml

clean:
	rm -rf bin
