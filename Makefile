.PHONY: build run clean test test-race cover lint vet licenses docker dev-up dev-setup dev-down dev-logs macos macos-run

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

build:
	go build $(LDFLAGS) -o bin/redash-wire ./cmd/redash-wire

# Uses the normal config resolution order (./config.yaml, then ~/.redash-wire/config.yaml),
# so a wizard-created config works without passing -config.
run: build
	./bin/redash-wire

test:
	go test ./...

test-race:
	go test -race -count=1 ./...

cover:
	go test -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

lint:
	golangci-lint run ./...

vet:
	go vet ./...

# Regenerate the bundled third-party license file (also run by goreleaser on release).
licenses:
	sh scripts/gen-third-party-licenses.sh

docker:
	docker build -t redash-wire:$(VERSION) --build-arg VERSION=$(VERSION) .

# Builds RedashWire.app with a universal redash-wire embedded in it. Needs Xcode,
# not just the Command Line Tools.
macos:
	./scripts/build-app.sh

macos-run: macos
	open build/RedashWire.app

clean:
	rm -rf bin/ build/ coverage.out coverage.html

dev-up:
	docker compose up -d

dev-setup: dev-up
	./dev/setup.sh

dev-down:
	docker compose down -v

dev-logs:
	docker compose logs -f
