BIN := kubesage
VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
TARGETS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64

.PHONY: build test run install release
build:
	go build -ldflags "$(LDFLAGS)" -o bin/$(BIN) .

test:
	go vet ./... && go test ./...

run: build
	./bin/$(BIN)

install:
	go install -ldflags "$(LDFLAGS)" .

release:
	@for t in $(TARGETS); do \
	  os=$${t%/*}; arch=$${t#*/}; ext=; if [ $$os = windows ]; then ext=.exe; fi; \
	  echo "-> $$os/$$arch"; \
	  GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/$(BIN)-$$os-$$arch$$ext . ; \
	done
