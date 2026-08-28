VERSION := $(shell git describe --tags --always --dirty 2>/dev/null)
BUILD   := CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=$(VERSION)" -trimpath

# GOARM=6 supports Raspberry Pi Zero and ARMv7 hosts.
PLATFORMS := linux/amd64 linux/arm64 linux/arm linux/386 linux/riscv64 \
             darwin/amd64 darwin/arm64 windows/amd64 windows/386 windows/arm64

# Rebuild every invocation; Go's cache keeps no-op builds fast.
omny:
	$(BUILD) -o omny ./cmd/omny

dist:
	@rm -rf dist && mkdir -p dist
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; out=dist/omny-$$os-$$arch; \
	  case $$os in windows) out=$$out.exe ;; esac; \
	  GOOS=$$os GOARCH=$$arch GOARM=6 $(BUILD) -o $$out ./cmd/omny || exit 1; \
	done
	@cd dist && sha256sum omny-* > SHA256SUMS && ls -l omny-* | awk '{printf "%8.1f MB  %s\n", $$5/1048576, $$9}'

test:
	go test -race ./...

lint:
	golangci-lint run ./...

# Local only because it needs live keys.
e2e: omny
	./scripts/e2e.sh

fmt:
	gofmt -s -w .

.PHONY: omny dist test lint e2e fmt
