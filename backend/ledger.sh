#!/bin/sh
# Runs Ledger (the server, or `ledger user ...`) with the Node.js runtime
# shipped in the release, or with the system's `node` (22.18 or newer).
#
# Installed as /usr/local/bin/ledger; the program lives in
# /usr/local/lib/ledger (ledger.mjs, the web UI, and the runtime).
set -eu
self=$(readlink -f "$0")
lib=${LEDGER_LIB:-$(dirname "$self")/../lib/ledger}
if [ -x "$lib/node" ]; then
	node=$lib/node
elif ! node=$(command -v node); then
	echo "ledger: Node.js not found (expected $lib/node or node in PATH)" >&2
	exit 1
fi
# Memory: a bounded heap, a small young generation and two malloc arenas
# keep the service well inside its 256 MB systemd limit, even for a
# worst-case 10 MB import running alongside large reports (measured peak
# ~235 MB; ~70 MB idle).
export MALLOC_ARENA_MAX="${MALLOC_ARENA_MAX:-2}"
exec "$node" --max-old-space-size="${LEDGER_MAX_HEAP_MB:-96}" --max-semi-space-size=2 \
	--disallow-code-generation-from-strings "$lib/ledger.mjs" "$@"
