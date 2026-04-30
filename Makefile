# Makefile for litesvm-go.
#
# Day-to-day:
#   make dev         build the local debug archive for litesvm_dev mode
#   make test        run the Go test suite against the vendored archive
#   make test-dev    run the Go test suite against a local cargo build
#
# Release / refresh vendored archives (committed to git, used by `go get`):
#   make vendor      build all archives and copy into litesvm_vendor/
#   make vendor-darwin  vendor-linux-glibc  vendor-linux-musl  vendor-windows
#
# Cleanup:
#   make clean       remove cargo target/ and vendored archives
#
# Cross-compilation requirements:
#   - rustup targets:
#       aarch64-apple-darwin       x86_64-apple-darwin
#       aarch64-unknown-linux-gnu  x86_64-unknown-linux-gnu
#       aarch64-unknown-linux-musl x86_64-unknown-linux-musl
#       x86_64-pc-windows-gnu
#   - cargo-zigbuild and zig (used for the linux targets):
#       cargo install cargo-zigbuild
#       brew install zig    # or your package manager equivalent
#   - mingw-w64 (used for the windows target; must NOT be replaced with
#     zigbuild — zig's lld emits a static archive whose TLS / unwinder /
#     __chkstk_ms ABI does not survive the cross-link to cgo's MinGW gcc on
#     the consumer machine):
#       brew install mingw-w64    # or your package manager equivalent
#   - nightly toolchain with rust-src (used by `vendor` builds for -Z build-std):
#       rustup toolchain install nightly
#       rustup component add rust-src --toolchain nightly
#   - llvm-strip (ships with LLVM; used to strip debug info from .a archives):
#       brew install llvm    # or your package manager equivalent

VENDOR_DIR := litesvm_vendor
HEADER_SRC := include/litesvm.h

DARWIN_AMD64_ARCHIVE      := $(VENDOR_DIR)/liblitesvm_go_darwin_amd64.a
DARWIN_ARM64_ARCHIVE      := $(VENDOR_DIR)/liblitesvm_go_darwin_arm64.a
GLIBC_LINUX_AMD64_ARCHIVE := $(VENDOR_DIR)/liblitesvm_go_glibc_linux_amd64.a
GLIBC_LINUX_ARM64_ARCHIVE := $(VENDOR_DIR)/liblitesvm_go_glibc_linux_arm64.a
MUSL_LINUX_AMD64_ARCHIVE  := $(VENDOR_DIR)/liblitesvm_go_musl_linux_amd64.a
MUSL_LINUX_ARM64_ARCHIVE  := $(VENDOR_DIR)/liblitesvm_go_musl_linux_arm64.a
WINDOWS_AMD64_ARCHIVE     := $(VENDOR_DIR)/liblitesvm_go_windows_amd64.a

VENDORED_ARCHIVES := \
	$(DARWIN_AMD64_ARCHIVE) \
	$(DARWIN_ARM64_ARCHIVE) \
	$(GLIBC_LINUX_AMD64_ARCHIVE) \
	$(GLIBC_LINUX_ARM64_ARCHIVE) \
	$(MUSL_LINUX_AMD64_ARCHIVE) \
	$(MUSL_LINUX_ARM64_ARCHIVE) \
	$(WINDOWS_AMD64_ARCHIVE)

# Pin the macOS deployment target so consumers on older SDKs link cleanly
# (avoids "built for newer macOS than being linked" warnings).
MACOS_DEPLOYMENT_TARGET ?= 11.0

# Vendored release builds use nightly + -Z build-std so we can rebuild std
# with the immediate-abort panic strategy (drops panic-formatting code from
# std, big win for static archive size). On current nightlies this is enabled
# via the -Cpanic=immediate-abort rustc flag (gated behind -Zunstable-options),
# not the older -Z build-std-features=panic_immediate_abort.
CARGO_NIGHTLY            := cargo +nightly
BUILD_STD_FLAGS          := -Z build-std=std,panic_abort
IMMEDIATE_ABORT_RUSTFLAGS := -Cpanic=immediate-abort -Zunstable-options
VENDOR_RUSTFLAGS         := RUSTFLAGS="$(IMMEDIATE_ABORT_RUSTFLAGS)"
STRIP_DEBUG              := llvm-strip --strip-debug

.PHONY: all vendor sizes header
.PHONY: vendor-darwin vendor-darwin-amd64 vendor-darwin-arm64
.PHONY: vendor-linux-glibc vendor-glibc-linux-amd64 vendor-glibc-linux-arm64
.PHONY: vendor-linux-musl vendor-musl-linux-amd64 vendor-musl-linux-arm64
.PHONY: vendor-windows vendor-windows-amd64
.PHONY: dev test test-dev clean clean-vendor

all: vendor

# Refresh the vendored header from the in-tree source. The header in
# litesvm_vendor/ is what `go get` consumers see, so it must stay in sync
# with src/lib.rs (which include/litesvm.h tracks).
header:
	cp $(HEADER_SRC) $(VENDOR_DIR)/litesvm.h

# ---- Day-to-day ------------------------------------------------------------

# Build the debug archive used by -tags litesvm_dev.
dev:
	cargo build

# Run tests against the vendored static archive (the path consumers use).
test:
	go test -count=1 ./...

# Run tests in dev mode (needs cargo build to have produced target/debug/).
test-dev: dev
	go test -count=1 -tags litesvm_dev ./...

# ---- Release: build + vendor archives --------------------------------------

vendor: header $(VENDORED_ARCHIVES) sizes

vendor-darwin: vendor-darwin-amd64 vendor-darwin-arm64

vendor-darwin-amd64: header $(DARWIN_AMD64_ARCHIVE)

vendor-darwin-arm64: header $(DARWIN_ARM64_ARCHIVE)

vendor-linux-glibc: vendor-glibc-linux-amd64 vendor-glibc-linux-arm64

vendor-glibc-linux-amd64: header $(GLIBC_LINUX_AMD64_ARCHIVE)

vendor-glibc-linux-arm64: header $(GLIBC_LINUX_ARM64_ARCHIVE)

vendor-linux-musl: vendor-musl-linux-amd64 vendor-musl-linux-arm64

vendor-musl-linux-amd64: header $(MUSL_LINUX_AMD64_ARCHIVE)

vendor-musl-linux-arm64: header $(MUSL_LINUX_ARM64_ARCHIVE)

vendor-windows: vendor-windows-amd64

vendor-windows-amd64: header $(WINDOWS_AMD64_ARCHIVE)

$(DARWIN_AMD64_ARCHIVE):
	MACOSX_DEPLOYMENT_TARGET=$(MACOS_DEPLOYMENT_TARGET) $(VENDOR_RUSTFLAGS) $(CARGO_NIGHTLY) build --release $(BUILD_STD_FLAGS) --target x86_64-apple-darwin
	@mkdir -p $(VENDOR_DIR)
	cp target/x86_64-apple-darwin/release/liblitesvm_go.a $@
	$(STRIP_DEBUG) $@
	@echo "Built $@"

$(DARWIN_ARM64_ARCHIVE):
	MACOSX_DEPLOYMENT_TARGET=$(MACOS_DEPLOYMENT_TARGET) $(VENDOR_RUSTFLAGS) $(CARGO_NIGHTLY) build --release $(BUILD_STD_FLAGS) --target aarch64-apple-darwin
	@mkdir -p $(VENDOR_DIR)
	cp target/aarch64-apple-darwin/release/liblitesvm_go.a $@
	$(STRIP_DEBUG) $@
	@echo "Built $@"

$(GLIBC_LINUX_AMD64_ARCHIVE):
	$(VENDOR_RUSTFLAGS) $(CARGO_NIGHTLY) zigbuild --release $(BUILD_STD_FLAGS) --target x86_64-unknown-linux-gnu
	@mkdir -p $(VENDOR_DIR)
	cp target/x86_64-unknown-linux-gnu/release/liblitesvm_go.a $@
	$(STRIP_DEBUG) $@
	@echo "Built $@"

$(GLIBC_LINUX_ARM64_ARCHIVE):
	$(VENDOR_RUSTFLAGS) $(CARGO_NIGHTLY) zigbuild --release $(BUILD_STD_FLAGS) --target aarch64-unknown-linux-gnu
	@mkdir -p $(VENDOR_DIR)
	cp target/aarch64-unknown-linux-gnu/release/liblitesvm_go.a $@
	$(STRIP_DEBUG) $@
	@echo "Built $@"

$(MUSL_LINUX_AMD64_ARCHIVE):
	$(VENDOR_RUSTFLAGS) $(CARGO_NIGHTLY) zigbuild --release $(BUILD_STD_FLAGS) --target x86_64-unknown-linux-musl
	@mkdir -p $(VENDOR_DIR)
	cp target/x86_64-unknown-linux-musl/release/liblitesvm_go.a $@
	$(STRIP_DEBUG) $@
	@echo "Built $@"

$(MUSL_LINUX_ARM64_ARCHIVE):
	$(VENDOR_RUSTFLAGS) $(CARGO_NIGHTLY) zigbuild --release $(BUILD_STD_FLAGS) --target aarch64-unknown-linux-musl
	@mkdir -p $(VENDOR_DIR)
	cp target/aarch64-unknown-linux-musl/release/liblitesvm_go.a $@
	$(STRIP_DEBUG) $@
	@echo "Built $@"

# Note: windows uses plain `cargo build` (not `cargo zigbuild`) because zig's
# lld emits a staticlib with TLS / unwinder / __chkstk_ms conventions that
# clash with cgo's MinGW linker on consumer machines and crash on first call.
# The .cargo/config.toml in the repo points cargo at the host's MinGW
# toolchain (brew install mingw-w64) for this target. Same toolchain family
# as the GitHub windows runner, so the resulting .a links cleanly there.
$(WINDOWS_AMD64_ARCHIVE):
	CC_x86_64_pc_windows_gnu=x86_64-w64-mingw32-gcc \
	AR_x86_64_pc_windows_gnu=x86_64-w64-mingw32-ar \
	$(VENDOR_RUSTFLAGS) $(CARGO_NIGHTLY) build --release $(BUILD_STD_FLAGS) --target x86_64-pc-windows-gnu
	@mkdir -p $(VENDOR_DIR)
	cp target/x86_64-pc-windows-gnu/release/liblitesvm_go.a $@
	@# Two windows-gnu-only fixups in one extract/repack pass:
	@#   1. rustc bundles openssl-sys's vendored libcrypto.a + libssl.a into
	@#      the staticlib on linux but NOT on windows-gnu, so consumers see
	@#      undefined references to BN_cmp / EVP_sha256 / etc. Merge them in.
	@#   2. llvm-strip rejects COFF archives ("unsupported object file
	@#      format"), so we strip extracted members one-by-one and repack.
	@tmpdir=$$(mktemp -d) && abs=$$(cd $(@D) && pwd)/$(@F) && \
		ossl=$$(find "$$(pwd)/target/x86_64-pc-windows-gnu/release/build" \
		             -path '*/openssl-sys-*/out/openssl-build/install/lib' \
		             -type d | head -1) && \
		( cd $$tmpdir && llvm-ar x $$abs && \
		  if [ -n "$$ossl" ]; then \
		    llvm-ar x $$ossl/libcrypto.a && \
		    llvm-ar x $$ossl/libssl.a; \
		  fi && \
		  for f in *; do llvm-strip --strip-debug "$$f"; done && \
		  llvm-ar rcsD $$abs.new * ) && \
		mv $$abs.new $$abs && rm -rf $$tmpdir
	@echo "Built $@"

sizes:
	@echo "Vendored archive sizes:"
	@ls -lh $(VENDOR_DIR)/*.a 2>/dev/null || echo "(no archives yet)"

# ---- Cleanup ---------------------------------------------------------------

clean:
	cargo clean
	$(MAKE) clean-vendor

clean-vendor:
	rm -f $(VENDOR_DIR)/*.a
