GO ?= go

.PHONY: build test race lint fmt

build:
	$(GO) build ./...

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

lint:
	@files="$$(gofmt -l .)"; if [ -n "$$files" ]; then echo "gofmt needed:"; echo "$$files"; exit 1; fi
	$(GO) vet ./...

fmt:
	gofmt -w .