GO      ?= GOTOOLCHAIN=local go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
BUILD   := CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)'
BUF     := PATH=$(HOME)/go/bin:$$PATH buf

PLATFORMS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64

.PHONY: all build test proto lint vet fmt cross clean tailwind web web-watch icons $(PLATFORMS)

# Tailwind v4 standalone CLI (no node_modules). app.css is committed, so
# `make build` never needs it; run `make web` after editing the frontend.
TAILWIND_VERSION := v4.3.3
TAILWIND         := bin/tools/tailwindcss
TAILWIND_OS      := $(subst Darwin,macos,$(subst Linux,linux,$(shell uname -s)))
TAILWIND_ARCH    := $(subst x86_64,x64,$(subst amd64,x64,$(subst aarch64,arm64,$(shell uname -m))))
WEB_STATIC       := internal/web/static

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

tailwind:
	@mkdir -p bin/tools
	curl -fsSL -o $(TAILWIND).tmp \
		https://github.com/tailwindlabs/tailwindcss/releases/download/$(TAILWIND_VERSION)/tailwindcss-$(TAILWIND_OS)-$(TAILWIND_ARCH)
	chmod +x $(TAILWIND).tmp && mv $(TAILWIND).tmp $(TAILWIND)
	@$(TAILWIND) --help | head -n 1

$(TAILWIND):
	$(MAKE) tailwind

web: $(TAILWIND)
	$(TAILWIND) -i $(WEB_STATIC)/input.css -o $(WEB_STATIC)/app.css --minify

web-watch: $(TAILWIND)
	$(TAILWIND) -i $(WEB_STATIC)/input.css -o $(WEB_STATIC)/app.css --minify --watch

# Favicon and app icons, drawn by internal/web/gen_icons.go (committed).
icons:
	$(GO) generate ./internal/web

clean:
	rm -rf bin
