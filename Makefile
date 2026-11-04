VERSION ?= 0.3.0
BIN     ?= bin/aish
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test vet fmt install doctor clean

all: build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/aish

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w cmd internal

install:
	go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/aish

doctor: build
	./$(BIN) doctor

clean:
	rm -rf bin
