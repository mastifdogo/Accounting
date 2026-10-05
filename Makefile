.PHONY: build frontend backend release test test-integration api-types schema

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOARCH  ?= amd64

# Full build: SvelteKit static site embedded into one static Go binary.
build: frontend backend

frontend:
	cd frontend && npm ci && npm run build
	find backend/web -mindepth 1 ! -name .gitkeep -delete
	cp -R frontend/build/. backend/web/

# Single static binary, no cgo, stripped. Embeds whatever is in backend/web.
backend:
	cd backend && CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -trimpath -ldflags="-s -w" -o ../bin/ledger .

# Deployable tarball: binary, SQL and deploy scripts. GOARCH=arm64 for ARM hosts.
release: build
	rm -rf dist/ledger-$(VERSION)-linux-$(GOARCH)
	mkdir -p dist/ledger-$(VERSION)-linux-$(GOARCH)/bin dist/ledger-$(VERSION)-linux-$(GOARCH)/db
	cp bin/ledger dist/ledger-$(VERSION)-linux-$(GOARCH)/bin/
	cp db/schema.sql db/grants.sql dist/ledger-$(VERSION)-linux-$(GOARCH)/db/
	cp -R deploy docs README.md dist/ledger-$(VERSION)-linux-$(GOARCH)/
	tar -C dist -czf dist/ledger-$(VERSION)-linux-$(GOARCH).tar.gz ledger-$(VERSION)-linux-$(GOARCH)
	@echo "dist/ledger-$(VERSION)-linux-$(GOARCH).tar.gz"

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
