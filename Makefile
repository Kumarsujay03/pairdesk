# Build PairDesk with your Supabase project baked in.
#
#   cp .env.example .env     # then fill in the two values once
#   make build               # -> ~/.local/bin/pairdesk
#   make run                 # build and start it
#
# .env is gitignored. The values are public by design (Row Level Security is
# the gate), but keeping them out of the repo lets anyone build their own
# instance from the same source.
-include .env
export

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
BIN     ?= $(HOME)/.local/bin/pairdesk
LDFLAGS  = -s -w -X main.version=$(VERSION) -X main.supabaseURL=$(PAIRDESK_SUPABASE_URL) -X main.supabaseAnon=$(PAIRDESK_SUPABASE_ANON)

.PHONY: build run test check-env

build: check-env
	@mkdir -p $(dir $(BIN))
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/pairdesk
	@echo "built $(BIN) ($(VERSION))"

run: build
	$(BIN)

test:
	go vet ./... && go test -race ./...

check-env:
	@test -n "$(PAIRDESK_SUPABASE_URL)" -a -n "$(PAIRDESK_SUPABASE_ANON)" || \
	  { echo "set PAIRDESK_SUPABASE_URL and PAIRDESK_SUPABASE_ANON in .env (see .env.example)"; exit 1; }
