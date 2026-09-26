GO      ?= GOTOOLCHAIN=local go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
BUILD   := CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)'
BUF     := PATH=$(HOME)/go/bin:$$PATH buf

PLATFORMS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64

.PHONY: all build test proto lint vet fmt cross clean $(PLATFORMS)

all: build

build:
	$(BUILD) -o bin/fleet ./cmd/fleet

test:
	$(GO) test ./...

proto:
	$(BUF) generate

vet:
	$(GO) vet ./...

lint: vet
	$(BUF) lint
	@test -z "$$(gofmt -l cmd internal)" || { gofmt -l cmd internal; echo "gofmt needed"; exit 1; }

fmt:
	gofmt -w cmd internal

# bin/fleet-<os>-<arch>
cross: $(PLATFORMS)

$(PLATFORMS):
	GOOS=$(word 1,$(subst /, ,$@)) GOARCH=$(word 2,$(subst /, ,$@)) \
		$(BUILD) -o bin/fleet-$(word 1,$(subst /, ,$@))-$(word 2,$(subst /, ,$@)) ./cmd/fleet

clean:
	rm -rf bin
