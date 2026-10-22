// Capturing: reading a stretch of conversation and keeping what a future
// session would want to know. This file holds the parts that don't need a
// running pi: finding the stretch not captured yet, the extraction prompt,
// and checking what the model returned.

import { shorten } from "./format.ts";

/** The custom entry recording what has been captured on a branch. */
export const CAPTURE_ENTRY = "engram-capture";
/** The custom message auto-recall adds. */
export const RECALL_MESSAGE = "engram-recall";

export interface CaptureRecord {
	/**
	 * The last entry the capture read. Empty when it failed partway: what it
	 * stored still counts as known, and the stretch is read again.
	 */
	upTo: string;
	/** Memories stored. */
	stored: Array<{ uri: string; text: string; tags: string[]; replaced?: string[]; problems?: string[] }>;
	/** Memories left out because a similar one exists. */
	skipped: number;
	/** Characters of conversation read. */
	chars: number;
	model?: string;
	error?: string;
	at: string;
}

export interface Candidate {
	text: string;
	tags: string[];
}

/** The parts of a session entry capturing looks at. */
export interface EntryLike {
	type: string;
	id: string;
	customType?: string;
	data?: unknown;
	message?: any;
	content?: unknown;
	details?: unknown;
	display?: boolean;
	timestamp?: string;
}

/**
 * uncaptured returns the entries on a branch after the last capture of it.
 * A capture on another branch doesn't count: its upTo isn't on this one.
 */
export function uncaptured<E extends EntryLike>(branch: E[]): E[] {
	const index = new Map(branch.map((e, i) => [e.id, i]));
	let from = 0;
	for (const e of branch) {
		if (e.type !== "custom" || e.customType !== CAPTURE_ENTRY) continue;
		const at = index.get((e.data as CaptureRecord | undefined)?.upTo ?? "");
		if (at !== undefined && at + 1 > from) from = at + 1;
	}
	return branch.slice(from);
}

/**
 * conversationMessages picks out the messages to read, leaving out what
 * auto-recall added: those memories are already stored.
 */
export function conversationMessages(entries: EntryLike[]): any[] {
	const out: any[] = [];
	for (const e of entries) {
		if (e.type === "message" && e.message) out.push(e.message);
		else if (e.type === "custom_message" && e.customType !== RECALL_MESSAGE)
			out.push({
				role: "custom",
				customType: e.customType,
				content: e.content ?? "",
				display: e.display ?? true,
				details: e.details,
				timestamp: e.timestamp ? Date.parse(e.timestamp) : 0,
			});
	}
	return out;
}

/** known lists memories this branch has already seen, recalled or stored. */
export function knownMemories(branch: EntryLike[]): string[] {
	const seen = new Map<string, string>();
	for (const e of branch) {
		const d: any = e.type === "custom" ? e.data : e.type === "custom_message" ? e.details : e.message?.details;
		if (e.type === "custom" && e.customType === CAPTURE_ENTRY) {
			for (const m of (d as CaptureRecord)?.stored ?? []) seen.set(m.uri, m.text);
		} else if (e.type === "custom_message" && e.customType === RECALL_MESSAGE) {
			for (const m of d?.memories ?? []) seen.set(m.uri, m.text);
		} else if (e.type === "message" && e.message?.role === "toolResult" && e.message.toolName === "recall") {
			for (const m of d?.memories ?? []) seen.set(m.uri, m.text);
		}
	}
	return [...seen.values()];
}

/** chunks splits a transcript at line breaks into pieces of at most max. */
export function chunks(text: string, max: number): string[] {
	const out: string[] = [];
	let rest = text;
	while (rest.length > max) {
		let cut = rest.lastIndexOf("\n", max);
		if (cut < max / 2) cut = max;
		out.push(rest.slice(0, cut));
		rest = rest.slice(cut);
	}
	if (rest.trim()) out.push(rest);
	return out;
}

export interface PromptContext {
	project?: string;
	cwd: string;
	date: string;
	known: string[];
	max: number;
}

export const SYSTEM_PROMPT = `You read part of a session between a person and an AI coding agent, and pick out the rare things worth keeping in a long-term memory store that agents search in later sessions. Those agents will not have this conversation. The store is shared and fills up fast, so the bar is high: keep a fact only if a future session would be noticeably worse off without it, and it couldn't easily find it out again.

Keep only:
- decisions the person made or agreed to, with the reason
- the person's preferences, ways of working, and corrections they gave the agent
- gotchas that cost real time: a non-obvious cause, what failed and what worked
- milestones: something merged, deployed, released, published or abandoned, with identifiers (PR, commit, version) and the date

Leave out, even if true:
- anything a future agent can learn by reading the code, its docs, the git log or the command's own help: file layout, function and type names, what a module does, how tests are set up
- progress on work that isn't finished (built, reviewed, started, in a branch): only the milestone when it lands, or a decision that shapes it
- what the agent did step by step, review rounds, tool output, plans that weren't acted on
- one-off details of this machine or session unless they will bite again
- anything listed under "Already known", and anything the agent stored itself with a remember tool call in the transcript
- secrets of any kind: passwords, tokens, API keys, private keys, credentials, personal data

Write each memory as two to five plain sentences, at most about 600 characters, that stand alone: name the project, the people and the date instead of "this", "here" or "the user". One subject per memory. Most stretches of conversation hold nothing worth keeping; returning none is the usual answer.

Answer with JSON only, no prose and no code fence:
{"memories": [{"text": "...", "tags": ["short", "labels"]}]}
Tags are a few lowercase labels such as the project and the kind of memory (decision, preference, gotcha, milestone).`;

export function userPrompt(transcript: string, c: PromptContext): string {
	const known = c.known.length
		? c.known
				.slice(-40)
				.map((k) => `- ${shorten(k, 300)}`)
				.join("\n")
		: "(none)";
	return [
		`Today is ${c.date}. The session ran in ${c.cwd}${c.project ? ` (project: ${c.project})` : ""}.`,
		`Return at most ${c.max} memories.`,
		"",
		"Already known:",
		known,
		"",
		"<transcript>",
		transcript,
		"</transcript>",
	].join("\n");
}

/** engram-mcp takes at most 30000 bytes; leave room. */
const MAX_TEXT_BYTES = 28_000;

/**
 * parseCandidates reads the model's answer, tolerating a code fence or prose
 * around the JSON. An answer that isn't the JSON asked for throws, so the
 * stretch is read again rather than taken to hold nothing.
 */
export function parseCandidates(answer: string, max: number): Candidate[] {
	const start = answer.indexOf("{");
	const end = answer.lastIndexOf("}");
	let parsed: any;
	try {
		parsed = start >= 0 && end > start ? JSON.parse(answer.slice(start, end + 1)) : undefined;
	} catch {}
	if (!Array.isArray(parsed?.memories)) throw new Error("the model's answer wasn't the JSON asked for");
	const out: Candidate[] = [];
	for (const m of parsed.memories) {
		const text = typeof m?.text === "string" ? m.text.trim() : "";
		if (text.length < 40 || Buffer.byteLength(text) > MAX_TEXT_BYTES || looksSecret(text)) continue;
		const tags = Array.isArray(m.tags)
			? m.tags
					.filter((t: unknown): t is string => typeof t === "string")
					.map((t: string) => t.trim().toLowerCase())
					.filter((t: string) => t && t.length <= 40 && !looksSecret(t))
					.slice(0, 8)
			: [];
		out.push({ text, tags });
		if (out.length >= max) break;
	}
	return out;
}

const SECRET_PATTERNS = [
	/-----BEGIN [A-Z ]*PRIVATE KEY-----/,
	/\b(sk|pk|rk)[-_](live|test|proj|ant)?[-_]?[A-Za-z0-9]{16,}/,
	/\bgh[pousr]_[A-Za-z0-9]{30,}/,
	/\bgithub_pat_[A-Za-z0-9_]{30,}/,
	/\bxox[abposr]-[A-Za-z0-9-]{10,}/,
	/\bAKIA[0-9A-Z]{16}\b/,
	/\beyJ[A-Za-z0-9_-]{15,}\.[A-Za-z0-9_-]{15,}\./, // a JWT
	// NAME=value where the name says what it is: AWS_SECRET_ACCESS_KEY=…, api_key: …
	/\b[\w.-]*(password|passwd|secret|api[_-]?key|access[_-]?key|token)[\w.-]*["']?\s*[:=]\s*["']?[^\s"'`]{8,}/i,
	/\bBearer\s+[A-Za-z0-9._~+/-]{20,}/,
];

/**
 * memoryTags makes a tag list engram accepts (at most 16, each at most 128
 * bytes) and leaves out duplicates and anything that looks like a credential.
 */
export function memoryTags(tags: string[]): string[] {
	const out: string[] = [];
	for (const t of tags) {
		const tag = t.trim();
		if (!tag || out.includes(tag) || Buffer.byteLength(tag) > 128 || looksSecret(tag)) continue;
		out.push(tag);
		if (out.length === 16) break;
	}
	return out;
}

/**
 * sanitizeSource drops what can carry credentials from a URL: the user info
 * (https://user:token@host/...) and the query and fragment.
 */
export function sanitizeSource(source: string): string {
	if (!/^[a-z][a-z0-9+.-]*:\/\//i.test(source)) return source;
	return source.replace(/^([a-z][a-z0-9+.-]*:\/\/)[^/@\s]+@/i, "$1").replace(/[?#].*$/, "");
}

/** looksSecret is true for text that seems to hold a credential. */
export function looksSecret(text: string): boolean {
	return SECRET_PATTERNS.some((p) => p.test(text));
}
