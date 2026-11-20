.PHONY: all build release install clean test fmt check help

all: build

## build: Build debug binary
build:
	cargo build

## release: Build optimized release binary
release:
	cargo build --release

## install: Install binary to /usr/local/bin
install: release
	sudo cp target/release/gryvia /usr/local/bin/
	@echo "✓ Installed to /usr/local/bin/gryvia"

## test: Run tests
test:
	cargo test

## fmt: Format code
fmt:
	cargo fmt

## check: Run clippy linter
check:
	cargo clippy -- -D warnings

## clean: Clean build artifacts
clean:
	cargo clean

## run: Run the CLI
run:
	cargo run -- --help

## help: Show this help message
help:
	@echo "Gryvia CLI - Makefile commands:"
	@sed -n 's/^##//p' ${MAKEFILE_LIST} | column -t -s ':' | sed -e 's/^/ /'
