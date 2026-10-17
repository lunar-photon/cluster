.PHONY: all build test install clean dist

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/lunar-photon/cluster/internal/cli.Version=$(VERSION)
PREFIX  ?= $(HOME)/.local

all: build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/cluster ./cmd/cluster

test:
	go vet ./...
	go test -race ./...

install: build
	install -Dm755 bin/cluster $(PREFIX)/bin/cluster

# Static binaries for Linux and macOS, both architectures.
dist:
	@for os in linux darwin; do for arch in amd64 arm64; do \
		echo "dist/cluster-$$os-$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' \
			-o dist/cluster-$$os-$$arch ./cmd/cluster || exit 1; \
	done; done

clean:
	rm -rf bin dist
