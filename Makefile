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
