.PHONY: build frontend backend test test-integration api-types schema

# Full build: SvelteKit static site embedded into one static Go binary.
build: frontend backend

frontend:
	cd frontend && npm ci && npm run build
	find backend/web -mindepth 1 ! -name .gitkeep -delete
	cp -R frontend/build/. backend/web/

# Single static binary, no cgo, stripped. Embeds whatever is in backend/web.
backend:
	cd backend && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o ../bin/ledger .

# Regenerate frontend API types after editing api/openapi.yaml.
api-types:
	cd frontend && npm run gen:api

test:
	cd backend && go vet ./... && go test ./...
	cd frontend && npm run check && npm test

# Requires a PostgreSQL role allowed to CREATE DATABASE, e.g.
#   make test-integration TEST_DATABASE_URL=postgres://postgres@localhost/postgres
test-integration:
	cd backend && TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test -count=1 -v ./...

schema:
	psql "$(DATABASE_URL)" -v ON_ERROR_STOP=1 -f db/schema.sql
