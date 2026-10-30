# Static Linux builds of gdiskmap. CGO is disabled so the binaries have no
# dynamic dependencies.

GO      ?= go
LDFLAGS := -s -w
ARCHES  := amd64 arm64

.PHONY: all build test lint clean

all: lint test build

build: $(ARCHES:%=bin/gdiskmap-linux-%)

bin/gdiskmap-linux-%: FORCE
	CGO_ENABLED=0 GOOS=linux GOARCH=$* $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $@ ./cmd/gdiskmap

test:
	$(GO) test ./...

# Lint for Linux as well as the host platform, because the ioctl code is
# Linux-only.
lint:
	golangci-lint run ./...
	GOOS=linux GOARCH=amd64 golangci-lint run ./...

clean:
	rm -rf bin

FORCE:
