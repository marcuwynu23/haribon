.PHONY: dev start build dist release clean

# Safe version fallback (no tags = dev)
VERSION := $(shell git describe --tags --abbrev=0 2>/dev/null || echo dev)

SRC := ./cli

# Supported platforms and architectures
PLATFORMS := windows linux
ARCHS := amd64 386

dev:
	air

start:
	go run $(SRC) start --config haribon-config.yml

build:
	@mkdir -p bin
	@echo "Building for $(shell go env GOOS)/$(shell go env GOARCH)"
	GOOS=$(shell go env GOOS) GOARCH=$(shell go env GOARCH) CGO_ENABLED=0 \
	go build -ldflags "-X main.version=$(VERSION)" \
	-o bin/haribon$(if $(findstring windows,$(shell go env GOOS)),.exe,) \
	$(SRC)

# Build all distributions (uncompressed)
dist: $(foreach platform,$(PLATFORMS),$(foreach arch,$(ARCHS),dist-$(platform)-$(arch)))

dist-%:
	@mkdir -p dist/haribon-$*-$(VERSION)
	GOOS=$(word 1,$(subst -, ,$*)) GOARCH=$(word 2,$(subst -, ,$*)) \
	go build \
	-o dist/haribon-$*-$(VERSION)/haribon$(if $(findstring windows,$*),.exe,) \
	$(SRC)

	cp haribon-config.yml dist/haribon-$*-$(VERSION)/haribon-config.yml

# Release all compressed builds
release: $(foreach platform,$(PLATFORMS),$(foreach arch,$(ARCHS),release-$(platform)-$(arch)))

release-%: dist-%
	@mkdir -p releases
	tar -czf releases/haribon-$*-$(VERSION).tar.gz -C dist haribon-$*-$(VERSION)

# Clean build artifacts
clean:
	rm -rf dist
	rm -rf releases/*.tar.gz bin

test:
	go test ./... -count=1 -coverprofile=coverage.out
	go tool cover -func coverage.out