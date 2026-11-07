BINARY  ?= bin/agent-task-runner
IMAGE   ?= <your-registry>/agent-task-runner
TAG     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GO      ?= go

.PHONY: build test vet lint image e2e clean

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-X main.version=$(VERSION)" -o $(BINARY) ./cmd/agent-task-runner

test:
	$(GO) test ./... -race -count=1

vet:
	$(GO) vet ./...

# The plan intentionally has no separate linter; vet is the lint gate.
lint: vet

image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(TAG) -t $(IMAGE):latest .

e2e:
	./scripts/e2e.sh

clean:
	rm -rf bin
