.PHONY: build frontend backend runtime release lxc-template test test-integration api-types schema clean

VERSION      ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
ARCH         ?= amd64
SUITE        ?= bookworm
# Node.js runtime shipped in releases (an Active LTS line).
NODE_VERSION ?= 24.21.0

OUT := out
REL := dist/ledger-$(VERSION)-linux-$(ARCH)

# Full build into out/:
#   out/bin/ledger              launcher (runs the server or `ledger user ...`)
#   out/lib/ledger/ledger.mjs   the server, bundled with its dependencies
#   out/lib/ledger/web/         the SvelteKit static site
# `make build` uses the system's node; `make release` adds the runtime.
build: frontend backend
	rm -rf $(OUT)
	mkdir -p $(OUT)/bin $(OUT)/lib/ledger
	install -m 0755 backend/ledger.sh $(OUT)/bin/ledger
	cp backend/dist/ledger.mjs $(OUT)/lib/ledger/
	cp -R frontend/build $(OUT)/lib/ledger/web

frontend:
	cd frontend && npm ci && npm run build

backend:
	cd backend && npm ci && npm run build

# Official Node.js binary for ARCH (amd64 or arm64), checksum-verified.
runtime:
	backend/fetch-node.sh $(NODE_VERSION) $(ARCH) $(OUT)/lib/ledger

# Deployable tarball: launcher, server, web UI, Node.js runtime, SQL and
# deploy scripts. ARCH=arm64 for ARM hosts.
release: build runtime
	rm -rf $(REL)
	mkdir -p $(REL)/db
	cp -R $(OUT)/bin $(OUT)/lib $(REL)/
	cp db/schema.sql db/grants.sql $(REL)/db/
	cp -R deploy docs README.md $(REL)/
	tar -C dist -czf $(REL).tar.gz $(notdir $(REL))
	@echo "$(REL).tar.gz"

# Proxmox LXC template with Ledger + PostgreSQL pre-installed (needs root and
# mmdebstrap). SUITE=bookworm (Debian 12, default), trixie, noble or jammy.
lxc-template: release
	deploy/lxc-template/build-lxc-template.sh --suite $(SUITE) --arch $(ARCH) --release $(REL).tar.gz

# Regenerate frontend API types after editing api/openapi.yaml.
api-types:
	cd frontend && npm run gen:api

test:
	cd backend && npm run check && npm test
	cd frontend && npm run check && npm test

# Requires a PostgreSQL role allowed to CREATE DATABASE, e.g.
#   make test-integration TEST_DATABASE_URL=postgres://postgres:secret@localhost/postgres
test-integration:
	cd backend && TEST_DATABASE_URL="$(TEST_DATABASE_URL)" npm run test:integration

schema:
	psql "$(DATABASE_URL)" -v ON_ERROR_STOP=1 -f db/schema.sql

clean:
	rm -rf $(OUT) backend/dist dist
