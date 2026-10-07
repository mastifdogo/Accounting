// Serves the built SvelteKit frontend. Unknown paths get the SPA fallback
// page so client-side routing handles them.

import { existsSync, promises as fsp } from "node:fs";
import type { IncomingMessage, ServerResponse } from "node:http";
import path from "node:path";

/** The page adapter-static writes for routes that are not prerendered (`fallback` in frontend/vite.config.ts). */
const SPA_FALLBACK = "200.html";

const CONTENT_TYPES: Record<string, string> = {
	".html": "text/html; charset=utf-8",
	".js": "text/javascript; charset=utf-8",
	".mjs": "text/javascript; charset=utf-8",
	".css": "text/css; charset=utf-8",
	".json": "application/json",
	".map": "application/json",
	".webmanifest": "application/manifest+json",
	".txt": "text/plain; charset=utf-8",
	".xml": "text/xml; charset=utf-8",
	".svg": "image/svg+xml",
	".png": "image/png",
	".jpg": "image/jpeg",
	".jpeg": "image/jpeg",
	".gif": "image/gif",
	".webp": "image/webp",
	".avif": "image/avif",
	".ico": "image/vnd.microsoft.icon",
	".woff": "font/woff",
	".woff2": "font/woff2",
	".wasm": "application/wasm",
};

const PLACEHOLDER =
	'<!doctype html><title>Ledger</title><p>Frontend not built. Run <code>make build</code>. The API is at <a href="/api/v1/health">/api/v1</a>.</p>';

type Handler = (req: IncomingMessage, res: ServerResponse, pathname: string) => Promise<void>;

export function staticHandler(webDir: string): Handler {
	const root = path.resolve(webDir);
	if (!existsSync(path.join(root, SPA_FALLBACK))) {
		return async (_req, res) => {
			res.writeHead(200, { "Content-Type": "text/html; charset=utf-8" });
			res.end(PLACEHOLDER);
		};
	}

	const send = async (req: IncomingMessage, res: ServerResponse, file: string, cacheControl: string) => {
		const body = await fsp.readFile(file);
		res.writeHead(200, {
			"Content-Type": CONTENT_TYPES[path.extname(file).toLowerCase()] ?? "application/octet-stream",
			"Content-Length": body.length,
			"Cache-Control": cacheControl,
		});
		res.end(req.method === "HEAD" ? undefined : body);
	};

	return async (req, res, pathname) => {
		if (req.method !== "GET" && req.method !== "HEAD") {
			res.writeHead(405, { Allow: "GET, HEAD", "Content-Type": "text/plain; charset=utf-8" });
			res.end("method not allowed\n");
			return;
		}
		let p = "";
		try {
			p = path.posix.normalize("/" + decodeURIComponent(pathname)).slice(1);
		} catch {
			// undecodable: not a file
		}
		if (p !== "" && !p.includes("\0")) {
			const file = path.join(root, p);
			const info = await fsp.stat(file).catch(() => null);
			if (info?.isFile() && file.startsWith(root + path.sep)) {
				await send(req, res, file, p.startsWith("_app/immutable/") ? "public, max-age=31536000, immutable" : "no-cache");
				return;
			}
		}
		// Client-side route: serve the SPA shell.
		await send(req, res, path.join(root, SPA_FALLBACK), "no-cache");
	};
}
