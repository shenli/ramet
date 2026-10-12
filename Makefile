.PHONY: build test test-go test-python test-integration check demo bench fmt

build:
	go build -trimpath -o bin/ramet ./cmd/ramet

test-integration:
	./scripts/test-integration.sh

test-go:
	go test -race ./...

test-python: build
	PYTHONPATH=sdk/python python3 -m unittest discover -s sdk/python/tests -v

test: test-go test-python

check: test
	go vet ./...
	@test -z "$$(gofmt -l $$(git ls-files '*.go') $$(git ls-files --others --exclude-standard '*.go'))"

fmt:
	gofmt -w $$(git ls-files '*.go') $$(git ls-files --others --exclude-standard '*.go')

demo: build
	./bin/ramet demo

bench:
	go test -run '^$$' -bench . -benchmem ./...
