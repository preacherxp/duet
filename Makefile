BINARY := duet
PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin

.PHONY: build install uninstall test

build:
	go build -trimpath -o $(BINARY) ./cmd/duet

install:
	mkdir -p $(BINDIR)
	go build -trimpath -o $(BINDIR)/$(BINARY) ./cmd/duet
	@echo "Installed $(BINDIR)/$(BINARY)"
	@echo "Make sure $(BINDIR) is on PATH."

uninstall:
	rm -f $(BINDIR)/$(BINARY)

test:
	go test ./...
	go test -race ./...
	go vet ./...
