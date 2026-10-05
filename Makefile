.PHONY: build test test-integration schema

# Single static binary, no cgo, stripped.
build:
	cd backend && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o ../bin/ledger .

test:
	cd backend && go vet ./... && go test ./...

# Requires a PostgreSQL role allowed to CREATE DATABASE, e.g.
#   make test-integration TEST_DATABASE_URL=postgres://postgres@localhost/postgres
test-integration:
	cd backend && TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test -count=1 -v ./...

schema:
	psql "$(DATABASE_URL)" -v ON_ERROR_STOP=1 -f db/schema.sql
