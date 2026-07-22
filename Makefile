# TicketDeck — build/install with the version stamped in.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)
BIN_DIR ?= $(HOME)/.local/bin

.PHONY: build install test vet fmt
build:            ## build ./ticketdeck
	go build -ldflags "$(LDFLAGS)" -o ./ticketdeck ./cmd/ticketdeck
install:          ## build into $(BIN_DIR)
	go build -ldflags "$(LDFLAGS)" -o "$(BIN_DIR)/ticketdeck" ./cmd/ticketdeck
test:
	go test ./...
vet:
	go vet ./...
fmt:
	gofmt -w .
