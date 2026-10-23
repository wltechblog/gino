VERSION ?= $(shell grep 'const version' cmd/gino/main.go | awk -F'"' '{print $$2}')
LDFLAGS := -ldflags="-s -w"
PREFIX ?= /usr/local
BINDIR ?= $(PREFIX)/bin

.PHONY: build clean build-all build-telegram build-discord build-slack build-lite install install-user uninstall

# Default: full build for current platform
build:
	CGO_ENABLED=0 go build -v $(LDFLAGS) -o gino ./cmd/gino

# Install the built binary to $(BINDIR). Override PREFIX for a custom prefix,
# or DESTDIR for packaging.
install: build
	install -d "$(DESTDIR)$(BINDIR)"
	install -m 0755 gino "$(DESTDIR)$(BINDIR)/gino"

# User-local install without root: ~/.local/bin.
install-user:
	$(MAKE) install PREFIX="$(HOME)/.local"

uninstall:
	rm -f "$(DESTDIR)$(BINDIR)/gino"

# Single-channel builds (smaller binaries)
build-telegram:
	CGO_ENABLED=0 go build $(LDFLAGS) -tags only_telegram -o gino ./cmd/gino

build-discord:
	CGO_ENABLED=0 go build $(LDFLAGS) -tags only_discord -o gino ./cmd/gino

build-slack:
	CGO_ENABLED=0 go build $(LDFLAGS) -tags only_slack -o gino ./cmd/gino

# Lite: no WhatsApp (backward compat)
build-lite:
	CGO_ENABLED=0 go build $(LDFLAGS) -tags lite -o gino ./cmd/gino

# Cross-compile all variants
build-all: \
	linux_amd64 linux_arm64 mac_arm64 \
	linux_amd64_telegram linux_arm64_telegram mac_arm64_telegram \
	linux_amd64_lite linux_arm64_lite mac_arm64_lite
	@echo "All builds completed."

# Full builds (all channels)
linux_amd64:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build $(LDFLAGS) -o gino_$(VERSION)_linux_amd64 ./cmd/gino

linux_arm64:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build $(LDFLAGS) -o gino_$(VERSION)_linux_arm64 ./cmd/gino

mac_arm64:
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build $(LDFLAGS) -o gino_$(VERSION)_mac_arm64 ./cmd/gino

# Telegram-only builds (~10MB)
linux_amd64_telegram:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build $(LDFLAGS) -tags only_telegram -o gino_$(VERSION)_linux_amd64_telegram ./cmd/gino

linux_arm64_telegram:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build $(LDFLAGS) -tags only_telegram -o gino_$(VERSION)_linux_arm64_telegram ./cmd/gino

mac_arm64_telegram:
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build $(LDFLAGS) -tags only_telegram -o gino_$(VERSION)_mac_arm64_telegram ./cmd/gino

# Lite builds (no WhatsApp, backward compat)
linux_amd64_lite:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build $(LDFLAGS) -tags lite -o gino_$(VERSION)_linux_amd64_lite ./cmd/gino

linux_arm64_lite:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build $(LDFLAGS) -tags lite -o gino_$(VERSION)_linux_arm64_lite ./cmd/gino

mac_arm64_lite:
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build $(LDFLAGS) -tags lite -o gino_$(VERSION)_mac_arm64_lite ./cmd/gino

clean:
	rm -f gino gino_*
