#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
project="grafana-sync-test-$$"
compose=(docker compose -p "$project" -f compose.test.yaml)
trap '"${compose[@]}" down -v' EXIT
"${compose[@]}" up -d --wait --wait-timeout 180
mysql_address=$("${compose[@]}" port mysql 3306)
postgres_address=$("${compose[@]}" port postgres 5432)
export MYSQL_TEST_DSN="grafana:local-test-password@tcp($mysql_address)/grafana_sync_test?charset=utf8mb4"
export POSTGRES_TEST_DSN="postgres://grafana:local-test-password@$postgres_address/grafana_sync_test?sslmode=disable"
go test -race -tags=integration -p 1 ./internal/cli ./internal/syncdb -count=1 -v
