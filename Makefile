# Build the mistral-vibe plugin for CLIProxyAPI.
#
#   make build        -> dist/cpa-mistral-vibe.<dylib|so|dll> for this machine
#   make release      -> dist/cpa-mistral-vibe-<os>-<arch>.<ext> for every platform
#   make test         -> unit tests + C-ABI end-to-end
#   make clean

GO      ?= go
DIST    := dist
# CPA validates the reported version with ^[0-9][0-9A-Za-z.+-]*$ (no leading "v"),
# and a git describe like "7d997db" fails it: the plugin would never register.
# So a local build reports a numeric dev version and a tag reports its number.
# An explicit VERSION (from a tag, or the command line) is normalised too, so
# `make VERSION=v0.1.0` and `make VERSION=7d997db` both end up valid.
RAW_VERSION := $(or $(VERSION),$(shell git describe --tags --always 2>/dev/null || echo 0.0.0))
PLUGIN_VERSION := $(patsubst v%,%,$(RAW_VERSION))
ifeq ($(shell echo '$(PLUGIN_VERSION)' | grep -Eq '^[0-9][0-9A-Za-z.+-]*$$' && echo ok),)
PLUGIN_VERSION := 0.0.0-dev.$(shell git rev-parse --short=7 HEAD 2>/dev/null || echo local)
endif
LDFLAGS := -s -w -X main.buildVersion=$(PLUGIN_VERSION)

UNAME := $(shell uname -s)
ifeq ($(UNAME),Darwin)
EXT := dylib
else ifeq ($(OS),Windows_NT)
EXT := dll
else
EXT := so
endif

OUT := $(DIST)/cpa-mistral-vibe.$(EXT)

# The platforms a release ships. Linux only: macOS builds queue for minutes on
# a hosted runner and used to hold the whole release behind them, so a Mac user
# builds locally with `make build` instead.
PLATFORMS ?= linux-amd64 linux-arm64

.PHONY: all build release test verify clean

all: build

build:
	@mkdir -p $(DIST)
	CGO_ENABLED=1 $(GO) build -buildmode=c-shared -trimpath -ldflags "$(LDFLAGS)" -o $(OUT) .
	@echo "built $(OUT) ($(PLUGIN_VERSION))"

# One shared library per platform, each with a .sha256 beside it.
#
# CGO needs a C toolchain for the TARGET. On Linux, arm64 needs
# gcc-aarch64-linux-gnu, which the release workflow installs. On macOS this
# still works for the darwin targets via
#   make release PLATFORMS="darwin-arm64 darwin-amd64"
release:
	@mkdir -p $(DIST)
	@for p in $(PLATFORMS); do \
		os=$${p%-*}; arch=$${p#*-}; \
		case $$os in darwin) ext=dylib ;; linux) ext=so ;; *) ext=dll ;; esac; \
		out=$(DIST)/cpa-mistral-vibe-$$os-$$arch.$$ext; \
		echo "building $$out"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=1 $(GO) build -buildmode=c-shared -trimpath -ldflags "$(LDFLAGS)" -o $$out . || exit 1; \
		( cd $(DIST) && shasum -a 256 $$(basename $$out) > $$(basename $$out).sha256 ) || exit 1; \
	done
	@echo "release artifacts in $(DIST)/"

test:
	./tests/e2e.sh

# verify stamps and checks what a release asset must actually contain: the
# linker drops an -X that names a missing symbol without an error, so a missing
# string means the version never landed.
verify: build
	@v=$$(basename $(OUT)); \
	 nm -gU $(OUT) 2>/dev/null | grep -q cliproxy_plugin_init || nm -D $(OUT) | grep -q cliproxy_plugin_init || { echo "missing cliproxy_plugin_init"; exit 1; }; \
	 grep -aq "$(PLUGIN_VERSION)" $(OUT) || { echo "version $(PLUGIN_VERSION) is not stamped in $(OUT)"; exit 1; }; \
	 echo "verified $(OUT) ($(PLUGIN_VERSION))"

clean:
	rm -rf $(DIST)
