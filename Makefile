GO ?= go

.PHONY: build test race lint fmt vet dash-check sim run tidy

build:
	$(GO) build ./...
	$(GO) build -o bin/penstock$(BINEXT) ./cmd/penstock
	$(GO) build -o bin/llmsim$(BINEXT) ./cmd/llmsim

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

lint:
	golangci-lint run

# The repository bans the em-dash character everywhere. The byte escape
# below avoids embedding one in this file.
dash-check:
	@! grep -rIn --exclude-dir=.git --exclude-dir=bin "$$(printf '\342\200\224')" . || (echo "em-dash found" && exit 1)

sim:
	$(GO) run ./cmd/llmsim

run:
	$(GO) run ./cmd/penstock --config config.yaml

tidy:
	$(GO) mod tidy

.PHONY: bench bench-all bench-clean

# Benchmarks. bench/run.sh builds the binaries, starts two llmsim
# instances and the gateway on loopback, waits for all three, runs the
# scenario, and writes raw k6 JSON plus a hardware stanza into
# bench/results. Nothing has to be started by hand.
#
# Read bench/README.md before quoting any number these produce. What is
# being measured, and what is deliberately not, is the point.
BENCH_SCENARIO ?= gateway_overhead

bench:
	bash bench/run.sh $(BENCH_SCENARIO)

# Includes the soak, so this takes SOAK_DURATION (30m by default) on top
# of the rest. Shorten it with: SOAK_DURATION=5m make bench-all
bench-all:
	bash bench/run.sh gateway_overhead
	bash bench/run.sh streaming_ttft
	bash bench/run.sh cache_hit
	bash bench/run.sh soak

# Clears results but keeps the directory and its note. Committed runs
# are evidence, so this is deliberately a separate target you have to
# ask for rather than something a build does on its way past.
bench-clean:
	@find bench/results -type f ! -name '.gitkeep' -delete
	@echo "bench/results cleared"
