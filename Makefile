.PHONY: build test lint integration grafana-integration

build:
	go build -o bin/grafana-sync ./cmd/grafana-sync

test:
	go test -race ./...

lint:
	@test -z "$$(gofmt -l $$(find . -name '*.go'))" || (echo 'Run gofmt on Go sources'; exit 1)
	go vet ./...

integration:
	bash scripts/test-integration.sh

grafana-integration: build
	CGO_ENABLED=0 GOOS=linux go build -o bin/grafana-header-probe ./scripts/grafana-probe
	python3 scripts/test-grafana.py
