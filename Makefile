APP := mdnsbridge
BIN := /usr/local/bin/$(APP)
SERVICE := /etc/systemd/system/$(APP).service

# Build outputs
DISTDIR := dist
AMD64 := $(DISTDIR)/$(APP)-linux-amd64
ARMV7 := $(DISTDIR)/$(APP)-linux-armv7

# Auto-detect architecture
UNAME_M := $(shell uname -m)
ifeq ($(UNAME_M),x86_64)
    ARCH_BINARY := $(AMD64)
else ifeq ($(UNAME_M),aarch64)
    ARCH_BINARY := $(ARMV7)
else ifeq ($(UNAME_M),armv7l)
    ARCH_BINARY := $(ARMV7)
else
    ARCH_BINARY := $(APP)
endif

.DEFAULT_GOAL := help

all: build ## Build for current platform (alias for build)

deps: ## Initialize go.mod and tidy dependencies
	@[ -f go.mod ] || go mod init $(APP)
	@go mod tidy

build: deps ## Build binary for current platform
	go build -o $(APP) .

build-all: deps $(DISTDIR) ## Cross-compile for amd64 and armv7
	GOOS=linux GOARCH=amd64 go build -o $(AMD64) .
	GOOS=linux GOARCH=arm GOARM=7 go build -o $(ARMV7) .

build-amd64: deps $(DISTDIR) ## Build for Linux amd64 only
	GOOS=linux GOARCH=amd64 go build -o $(AMD64) .

build-armv7: deps $(DISTDIR) ## Build for Linux armv7 only
	GOOS=linux GOARCH=arm GOARM=7 go build -o $(ARMV7) .

$(DISTDIR):
	mkdir -p $(DISTDIR)

install: build-all ## Install binary and systemd service (uses sudo)
	@echo "Detected arch: $(UNAME_M) -> installing $(ARCH_BINARY)"
	sudo install -m 0755 $(ARCH_BINARY) $(BIN)
	sudo install -m 0644 $(APP).service $(SERVICE)
	sudo systemctl daemon-reload
	sudo systemctl enable --now $(APP).service

uninstall: ## Remove binary and systemd service (uses sudo)
	sudo systemctl disable --now $(APP).service || true
	sudo rm -f $(BIN)
	sudo rm -f $(SERVICE)
	sudo systemctl daemon-reload

clean: ## Remove build artifacts
	rm -f $(APP)
	rm -rf $(DISTDIR)

help: ## Show this help message
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-15s %s\n", $$1, $$2}'

.PHONY: all deps build build-all build-amd64 build-armv7 install uninstall clean help
