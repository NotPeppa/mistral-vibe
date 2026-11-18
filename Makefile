# Build the mistral-vibe plugin for CLIProxyAPI.
#
#   make build        -> dist/cpa-mistral-vibe.<dylib|so|dll> for this machine
#   make release      -> dist/cpa-mistral-vibe-<os>-<arch>.<ext> for every platform
#   make universal    -> dist/cpa-mistral-vibe-darwin-universal.dylib (arm64 + amd64)
#   make test         -> unit tests + C-ABI end-to-end
#   make clean

GO      ?= go
DIST    := dist
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.buildVersion=$(VERSION)

UNAME := $(shell uname -s)
ifeq ($(UNAME),Darwin)
EXT := dylib
else ifeq ($(OS),Windows_NT)
EXT := dll
else
EXT := so
endif

OUT := $(DIST)/cpa-mistral-vibe.$(EXT)

# The platforms a release ships. CGO is required: a plugin is a cgo shared
# library, so cross-building needs a target C toolchain — the release workflow
# builds the Linux ones on Linux for that reason.
PLATFORMS ?= darwin-arm64 darwin-amd64 linux-amd64 linux-arm64

.PHONY: all build release universal test verify clean

all: build

build:
	@mkdir -p $(DIST)
	CGO_ENABLED=1 $(GO) build -buildmode=c-shared -trimpath -ldflags "$(LDFLAGS)" -o $(OUT) .
	@echo "built $(OUT) ($(VERSION))"

# One shared library per platform, each with a .sha256 beside it.
#
# CGO needs a C toolchain for the TARGET, so on macOS only the two darwin
# entries build; the Linux ones report the missing cross compiler. The release
# workflow runs this on ubuntu, where all four build. Override PLATFORMS to
# build a subset locally: make release PLATFORMS="darwin-arm64 darwin-amd64"
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

# A dylib both Apple Silicon and Intel Macs load.
universal: release
	@mkdir -p $(DIST)/tmp
	@lipo -create -output $(DIST)/cpa-mistral-vibe-darwin-universal.dylib \
		$(DIST)/cpa-mistral-vibe-darwin-arm64.dylib $(DIST)/cpa-mistral-vibe-darwin-amd64.dylib
	@( cd $(DIST) && shasum -a 256 cpa-mistral-vibe-darwin-universal.dylib > cpa-mistral-vibe-darwin-universal.dylib.sha256 )
	@echo "built $(DIST)/cpa-mistral-vibe-darwin-universal.dylib"

test:
	./tests/e2e.sh

# verify stamps and checks what a release asset must actually contain: the
# linker drops an -X that names a missing symbol without an error, so a missing
# string means the version never landed.
verify: build
	@v=$$(basename $(OUT)); \
	 nm -gU $(OUT) 2>/dev/null | grep -q cliproxy_plugin_init || nm -D $(OUT) | grep -q cliproxy_plugin_init || { echo "missing cliproxy_plugin_init"; exit 1; }; \
	 grep -aq "$(VERSION)" $(OUT) || { echo "version $(VERSION) is not stamped in $(OUT)"; exit 1; }; \
	 echo "verified $(OUT) ($(VERSION))"

clean:
	rm -rf $(DIST)
