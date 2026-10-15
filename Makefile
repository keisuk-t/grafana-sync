.PHONY: build test lint integration

build:
	go build -o bin/grafana-sync ./cmd/grafana-sync

test:
	go test -race ./...

lint:
	@test -z "$$(gofmt -l $$(find . -name '*.go'))" || (echo 'Run gofmt on Go sources'; exit 1)
	go vet ./...

integration:
	bash scripts/test-integration.sh
