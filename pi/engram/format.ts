// How memories read to the model. engram-mcp's results carry everything
// (match explanations, snippets with byte offsets); the model needs the text,
// where it's from and how to fetch it, in far fewer tokens.

export interface Memory {
	space?: string;
	uri: string;
	author: string;
	text: string;
	tags: string[];
	source?: string;
	createdAt: string;
	similarity?: number;
	/** The part of the memory that bears on the prompt, as the model picked it. */
	relevant?: string;
	match?: {
		keyword?: { terms?: Array<{ term: string; kind?: string }> };
	};
}

export interface MemoriesOut {
	memories: Memory[];
	cursor?: string;
	mode?: string;
	note?: string;
}

export interface SpaceInfo {
	name: string;
	uri: string;
	default?: boolean;
	setUp: boolean;
	model?: { name: string };
	indexing?: string;
	memories?: number;
	warning?: string;
}

export interface ListSpacesOut {
	spaces: SpaceInfo[];
	note?: string;
}

/** shorten cuts text to max characters at a word boundary. */
export function shorten(text: string, max: number): string {
	const t = text.trim();
	if (t.length <= max) return t;
	const cut = t.slice(0, max);
	const sp = cut.lastIndexOf(" ");
	return `${(sp > max * 0.7 ? cut.slice(0, sp) : cut).trimEnd()}…`;
}

/** humanBytes writes a size the way a person reads it. */
export function humanBytes(n: number): string {
	if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MB`;
	if (n >= 1 << 10) return `${Math.round(n / (1 << 10))} kB`;
	return `${n} B`;
}

export function day(iso: string): string {
	return iso ? iso.slice(0, 10) : "";
}

function header(m: Memory): string {
	const parts = [m.space, day(m.createdAt)];
	if (m.similarity !== undefined) parts.push(`similarity ${m.similarity}`);
	if (m.tags?.length) parts.push(`tags: ${m.tags.join(", ")}`);
	if (m.source) parts.push(`source: ${m.source}`);
	return parts.filter(Boolean).join(" · ");
}

/** formatMemories is a list of memories for a tool result. */
export function formatMemories(out: MemoriesOut, opts: { maxChars?: number } = {}): string {
	const lines: string[] = [];
	if (!out.memories.length) lines.push("No memories found.");
	out.memories.forEach((m, i) => {
		lines.push(`[${i + 1}] ${header(m)}`);
		lines.push(opts.maxChars ? shorten(m.text, opts.maxChars) : m.text.trim());
		lines.push(`uri: ${m.uri}`);
		lines.push("");
	});
	if (out.cursor) lines.push(`More: pass cursor ${out.cursor}`);
	if (out.note) lines.push(`Note: ${out.note}`);
	return lines.join("\n").trim();
}

/**
 * recallBlock is what auto-recall adds to the conversation: the memories
 * most like the prompt, shortened, marked as recalled rather than asked for.
 */
export function recallBlock(memories: Memory[], maxChars: number, picked = false): string {
	const lines = [
		"<memories>",
		picked
			? "Memories from Engram recalled automatically and picked as relevant to this request; a long one shows the part that matters (get_memory with its uri has the full text). They were written in earlier sessions and may be out of date: verify before relying on them."
			: "Memories from Engram recalled automatically for this request. They were written by agents in earlier sessions and may be stale or beside the point: use what helps, verify before relying on it, and call get_memory with a uri for a shortened one's full text.",
		"",
	];
	for (const m of memories) {
		lines.push(`- (${header(m)}) ${m.relevant ? shorten(m.relevant, maxChars) : shorten(m.text, maxChars)}`);
		lines.push(`  uri: ${m.uri}`);
	}
	lines.push("</memories>");
	return lines.join("\n");
}

/** spacesSummary describes the spaces for the system prompt. */
export function spacesSummary(spaces: SpaceInfo[]): string {
	const set = spaces.filter((s) => s.setUp);
	if (!set.length) return "No memory spaces are set up yet; the person can run `engram login` and `engram use <space>`.";
	const lines = ["Memory spaces (recall searches all of them; remember writes to the default unless you pass space):"];
	for (const s of set) {
		let line = `- ${s.name}${s.default ? " (default)" : ""}`;
		if (s.indexing && s.indexing !== "granted")
			line += ` — NOT indexed (${s.indexing}): what's stored there can't be recalled until its owner approves indexing (\`engram index ${s.name}\`)`;
		lines.push(line);
	}
	return lines.join("\n");
}
