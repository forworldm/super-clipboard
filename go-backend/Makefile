BINARY  := super-clipboard
PKG     := ./...
PORT    ?= 5173

.PHONY: all run dev build test race cover vet fmt tidy clean docker

all: vet test build

# go run . is the equivalent of `python -m backend`
run dev:
	SUPER_CLIPBOARD_APP_PORT=$(PORT) go run .

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/$(BINARY) .

# equivalent of `pytest backend/tests`
test:
	go test $(PKG)

race:
	go test -race $(PKG)

cover:
	go test -coverprofile=coverage.out $(PKG)
	go tool cover -func=coverage.out | tail -1

vet:
	go vet $(PKG)

fmt:
	gofmt -l -w .

tidy:
	go mod tidy

clean:
	rm -rf bin coverage.out storage

docker:
	docker build -t super-clipboard-go .
