// A small multipart/form-data parser for the in-memory upload body.

export class MultipartError extends Error {}

export interface Part {
	/** The form field name. */
	name: string;
	/** The file name reduced to its base name ("" if none), like Go's Part.FileName. */
	filename: string;
	data: Buffer;
}

/** Splits a header parameter list: `form-data; name="file"; filename="a.csv"`. */
export function parseHeaderParams(value: string): { type: string; params: Map<string, string> } {
	const params = new Map<string, string>();
	let i = value.indexOf(";");
	const type = (i < 0 ? value : value.slice(0, i)).trim().toLowerCase();
	const s = i < 0 ? "" : value.slice(i);
	i = 0;
	while (i < s.length) {
		const m = /^[;\s]*([^=;\s]+)\s*=\s*/.exec(s.slice(i));
		if (!m) break;
		i += m[0].length;
		const key = m[1]!.toLowerCase();
		let val: string;
		if (s[i] === '"') {
			val = "";
			i++;
			while (i < s.length && s[i] !== '"') {
				if (s[i] === "\\" && i + 1 < s.length) i++;
				val += s[i++];
			}
			i++; // closing quote
		} else {
			const end = s.slice(i).search(/[;\s]/);
			val = end < 0 ? s.slice(i) : s.slice(i, i + end);
			i += val.length;
		}
		if (key.endsWith("*")) {
			// RFC 2231 / 5987: charset'lang'percent-encoded
			const ext = /^([^']*)'[^']*'(.*)$/.exec(val);
			if (ext && /^utf-8$/i.test(ext[1]!)) {
				try {
					params.set(key.slice(0, -1), decodeURIComponent(ext[2]!));
				} catch {
					// ignore malformed encodings
				}
			}
			continue;
		}
		if (!params.has(key)) params.set(key, val);
	}
	return { type, params };
}

/** The boundary of a multipart/form-data Content-Type, or null. */
export function formDataBoundary(contentType: string | undefined): string | null {
	if (!contentType) return null;
	const { type, params } = parseHeaderParams(contentType);
	const b = params.get("boundary");
	return type === "multipart/form-data" && b && b.length <= 70 ? b : null;
}

function baseName(name: string): string {
	if (name === "") return "";
	const trimmed = name.replace(/\/+$/, "");
	if (trimmed === "") return "/";
	return trimmed.slice(trimmed.lastIndexOf("/") + 1);
}

/** Parses a complete multipart body into its parts. */
export function parseMultipart(body: Buffer, boundary: string): Part[] {
	const delim = Buffer.from(`--${boundary}`);
	const parts: Part[] = [];
	let pos = body.indexOf(delim);
	if (pos < 0) throw new MultipartError("NextPart: EOF");
	for (;;) {
		pos += delim.length;
		if (body.subarray(pos, pos + 2).toString() === "--") return parts; // final boundary
		// Skip transport padding and the line break after the boundary.
		while (body[pos] === 0x20 || body[pos] === 0x09) pos++;
		if (body[pos] === 0x0d) pos++;
		if (body[pos] !== 0x0a) throw new MultipartError("malformed MIME header: missing line break after boundary");
		pos++;

		const headerEnd = body.indexOf("\r\n\r\n", pos);
		const headerEndLF = body.indexOf("\n\n", pos);
		let end: number;
		let sepLen: number;
		if (headerEnd >= 0 && (headerEndLF < 0 || headerEnd <= headerEndLF)) {
			end = headerEnd;
			sepLen = 4;
		} else if (headerEndLF >= 0) {
			end = headerEndLF;
			sepLen = 2;
		} else if (body.subarray(pos, pos + 2).toString() === "\r\n" || body[pos] === 0x0a) {
			end = pos; // no headers
			sepLen = body[pos] === 0x0a ? 1 : 2;
		} else {
			throw new MultipartError("malformed MIME header: unexpected EOF");
		}
		const headers = new Map<string, string>();
		for (const line of body.subarray(pos, end).toString("utf8").split(/\r?\n/)) {
			if (line === "") continue;
			const c = line.indexOf(":");
			if (c <= 0) throw new MultipartError(`malformed MIME header line: ${line}`);
			headers.set(line.slice(0, c).trim().toLowerCase(), line.slice(c + 1).trim());
		}
		pos = end + sepLen;

		// The part's content ends at the next CRLF (or LF) + delimiter.
		const next = body.indexOf(Buffer.concat([Buffer.from("\n"), delim]), pos);
		if (next < 0) throw new MultipartError("multipart: NextPart: EOF");
		const dataEnd = next > pos && body[next - 1] === 0x0d ? next - 1 : next;

		const cd = parseHeaderParams(headers.get("content-disposition") ?? "");
		parts.push({
			name: cd.type === "form-data" ? (cd.params.get("name") ?? "") : "",
			filename: baseName(cd.params.get("filename") ?? ""),
			data: body.subarray(pos, dataEnd),
		});
		pos = next + 1;
	}
}
