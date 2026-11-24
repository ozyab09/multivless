BIN := bin/multivless
GOFLAGS := -trimpath
LDFLAGS := -s -w -X main.version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build web test vet run e2e smoke-api docker clean

# The Go binary embeds internal/web/dist, so rebuild the UI before Go when its
# sources changed (`make web`).
build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/multivless

web:
	npm --prefix web ci
	npm --prefix web run build

test:
	go test ./...

vet:
	go vet ./...

run: build
	$(BIN) run --config configs/config.example.yaml

e2e: build
	./scripts/e2e.sh

smoke-api: build
	./scripts/smoke-api.sh

docker:
	docker compose build

clean:
	rm -rf bin
