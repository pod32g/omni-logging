BINARY  ?= omnilog
VERSION ?= 0.1.0-dev
LDFLAGS  = -ldflags "-X main.version=$(VERSION)"

.PHONY: build test vet fmt run clean docker tidy

## build: compile the single binary (web UI is embedded)
build:
	go build $(LDFLAGS) -o $(BINARY) ./cmd/omnilog

## test: run the full test suite
test:
	go test ./...

## vet: run go vet
vet:
	go vet ./...

## fmt: format all Go code
fmt:
	gofmt -w .

## run: build and start the server with open dev settings
run: build
	./$(BINARY) serve --addr :8080 --db ./omni.db

## clean: remove build artifacts and local databases
clean:
	rm -f $(BINARY) *.db *.db-wal *.db-shm

## docker: build the container image
docker:
	docker build -t omnilog:$(VERSION) .

## tidy: tidy go modules
tidy:
	go mod tidy
