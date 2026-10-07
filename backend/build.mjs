// Bundles the server and its dependencies (pg, bcryptjs) into one ES module,
// dist/ledger.mjs, so a release needs no node_modules.
import { build } from "esbuild";

await build({
	entryPoints: ["src/main.ts"],
	outfile: "dist/ledger.mjs",
	bundle: true,
	platform: "node",
	format: "esm",
	target: "node22",
	// Optional pg integrations that are never used here.
	external: ["pg-native", "cloudflare:sockets"],
	// pg is CommonJS and requires Node built-ins at run time.
	banner: { js: "import { createRequire as __ledgerRequire } from 'node:module';\nconst require = __ledgerRequire(import.meta.url);" },
	legalComments: "eof",
	logLevel: "warning",
});
