// Session sync: a session's transcript, and where the work stands, kept in
// the principal's archive by the session's own identity, so other agents can
// search what it did and it can be picked up on another machine. The record
// is a garden.engram.session; the bytes go up as complete JSONL lines. This
// side only has to know how much of the file it has stored, and what to say
// about the work.

import { execFile } from "node:child_process";
import { closeSync, existsSync, mkdirSync, openSync, readSync, statSync, writeFileSync } from "node:fs";
import { homedir, hostname } from "node:os";
import { join } from "node:path";

/** SESSION_SYNC_ENTRY records how much of the transcript is stored. */
export const SESSION_SYNC_ENTRY = "engram-session-sync";

/** HARness is what this writes in a session record. */
export const HARNESS = "pi";

/** MAX_PUSH_BYTES is the most one push_session carries. */
export const MAX_PUSH_BYTES = 8 << 20;

export interface SyncRecord {
	/** Bytes of the transcript the archive reported holding after this sync. */
	bytes: number;
	/** Entries the archive reported. */
	entries?: number;
	/** When this was written. */
	at: string;
	/** Why the last attempt failed, if it did. */
	error?: string;
}

/** The parts of a session entry this module reads. */
export interface EntryLike {
	type: string;
	id: string;
	customType?: string;
	data?: any;
	message?: any;
	timestamp?: string;
}

/** Manifest is the garden.engram.session record this pushes (the archive adds kind and parent from the identity). */
export interface Manifest {
	sessionId: string;
	title: string;
	summary: string;
	status: "active" | "ended";
	cwd?: string;
	machine?: string;
	harness?: string;
	model?: string;
	repo?: string;
	branch?: string;
	gitHead?: string;
	gitDirty?: boolean;
	startedAt?: string;
}

export interface PushAnswer {
	session: string;
	uri?: string;
	bytes: number;
	entries: number;
	chunks?: number;
	storedBytes: number;
	resync?: boolean;
}

/** syncedBytes is the most of the file any sync entry on the session says was stored. */
export function syncedBytes(entries: EntryLike[]): number {
	let most = 0;
	for (const e of entries) {
		if (e.type !== "custom" || e.customType !== SESSION_SYNC_ENTRY) continue;
		const bytes = Number(e.data?.bytes ?? 0);
		if (Number.isFinite(bytes) && bytes > most) most = bytes;
	}
	return most;
}

/** lastSync is the newest sync entry, for reporting what happened. */
export function lastSync(entries: EntryLike[]): SyncRecord | undefined {
	let found: { at: number; rec: SyncRecord } | undefined;
	for (const e of entries) {
		if (e.type !== "custom" || e.customType !== SESSION_SYNC_ENTRY) continue;
		const rec = e.data as SyncRecord | undefined;
		if (!rec) continue;
		const at = Date.parse(rec.at ?? e.timestamp ?? "") || 0;
		if (!found || at >= found.at) found = { at, rec };
	}
	return found?.rec;
}

/** userText is a user message's text, whichever shape it has. */
export function userText(entry: EntryLike): string {
	const content = entry.message?.content;
	if (typeof content === "string") return content;
	if (!Array.isArray(content)) return "";
	return content
		.filter((b: any) => b?.type === "text" && typeof b.text === "string")
		.map((b: any) => b.text)
		.join(" ")
		.trim();
}

/**
 * titleOf is a short name for the work: the session's name when it has one,
 * else its first prompt, one line of it.
 */
export function titleOf(branch: EntryLike[], name?: string): string {
	if (name?.trim()) return shortenLine(name, 200);
	for (const e of branch) {
		if (e.type !== "message" || e.message?.role !== "user") continue;
		const text = userText(e);
		if (text) return shortenLine(text, 120);
	}
	return "session";
}

/**
 * summaryOf is the newest compaction summary on the branch: what the harness
 * itself already wrote down about the work, at no cost.
 */
export function summaryOf(branch: EntryLike[]): string {
	for (let i = branch.length - 1; i >= 0; i--) {
		const e = branch[i];
		if (e.type === "compaction" && typeof (e as any).summary === "string") return (e as any).summary;
	}
	return "";
}

/** startedAtOf is when the transcript began: its header's or first entry's time. */
export function startedAtOf(header: { timestamp?: string } | undefined, entries: EntryLike[]): string | undefined {
	if (header?.timestamp) return header.timestamp;
	for (const e of entries) if (e.timestamp) return e.timestamp;
	return undefined;
}

/** shortenLine flattens text to one line of at most n characters. */
export function shortenLine(text: string, n: number): string {
	const flat = text.replace(/\s+/g, " ").trim();
	return flat.length <= n ? flat : `${flat.slice(0, n - 1).trimEnd()}…`;
}

/**
 * encodeCwd names a working directory's session folder the way pi does, so a
 * pulled session lands where /resume looks for it.
 */
export function encodeCwd(cwd: string): string {
	const abs = cwd.startsWith("/") ? cwd : `/${cwd}`;
	return `--${abs.replace(/^[/\\]/, "").replace(/[/\\:]/g, "-")}--`;
}

/** agentDir is where pi keeps its sessions and settings. */
export function agentDir(env: NodeJS.ProcessEnv = process.env): string {
	return env.PI_CODING_AGENT_DIR || join(homedir(), ".pi", "agent");
}

/**
 * resumePath is where a pulled session's file goes: its working directory's
 * folder, named the way pi names its own, so /resume and --session find it.
 */
export function resumePath(sessionDir: string, cwd: string, id: string, startedAt?: string): string {
	const at = (startedAt ?? new Date().toISOString()).replace(/[:.]/g, "-");
	return join(sessionDir, encodeCwd(cwd), `${at}_${id}.jsonl`);
}

/** Git says where the code is, which resuming needs to match. */
export interface GitInfo {
	repo?: string;
	branch?: string;
	gitHead?: string;
	gitDirty?: boolean;
}

/** gitInfo reads the repository a directory is in, if it is in one. */
export async function gitInfo(cwd: string): Promise<GitInfo> {
	const run = (args: string[]) =>
		new Promise<string | undefined>((resolve) => {
			execFile("git", ["-C", cwd, ...args], { timeout: 2000 }, (err, stdout) =>
				resolve(err ? undefined : String(stdout).trim()),
			);
		});
	const [repo, branch, head, status] = await Promise.all([
		run(["config", "--get", "remote.origin.url"]),
		run(["rev-parse", "--abbrev-ref", "HEAD"]),
		run(["rev-parse", "HEAD"]),
		run(["status", "--porcelain", "--untracked-files=no"]),
	]);
	if (!repo && !branch && !head) return {};
	const info: GitInfo = {};
	if (repo) info.repo = repo;
	if (branch && branch !== "HEAD") info.branch = branch;
	if (head) info.gitHead = head.slice(0, 12);
	if (status) info.gitDirty = true;
	return info;
}

/** A transcript to push: a session file, or an in-memory session's entries. */
export interface Transcript {
	size(): number;
	/**
	 * piece reads complete lines from `from`: at most max bytes, cut after
	 * the last newline. more says whether lines beyond it remain. A line
	 * longer than max can't be stored, which is an error.
	 */
	piece(from: number, max: number): { data: string; end: number; more: boolean };
}

function cutPiece(buf: Buffer, from: number, max: number, size: number): { data: string; end: number; more: boolean } {
	const cut = buf.lastIndexOf(0x0a) + 1;
	if (cut === 0 && buf.length === max) throw new Error(`the session has a line longer than ${max} bytes at byte ${from}; it can't be stored`);
	return { data: buf.subarray(0, cut).toString("utf8"), end: from + cut, more: buf.length === max && from + cut < size };
}

/** fileTranscript reads a session file, only the part asked for. */
export function fileTranscript(path: string): Transcript {
	return {
		size: () => statSync(path).size,
		piece(from, max) {
			const size = statSync(path).size;
			const want = Math.max(0, Math.min(size - from, max));
			const buf = Buffer.alloc(want);
			const fd = openSync(path, "r");
			try {
				let got = 0;
				while (got < want) {
					const n = readSync(fd, buf, got, want - got, from + got);
					if (n <= 0) break;
					got += n;
				}
				return cutPiece(buf.subarray(0, got), from, max, size);
			} finally {
				closeSync(fd);
			}
		},
	};
}

/**
 * memoryTranscript is an in-memory session (pi -p --no-session, as
 * subagents run) written out the way pi writes a session file: the header,
 * then each entry, a line each.
 */
export function memoryTranscript(header: unknown, entries: unknown[]): Transcript {
	const lines = [header, ...entries].filter((e) => e != null).map((e) => `${JSON.stringify(e)}\n`);
	const buf = Buffer.from(lines.join(""), "utf8");
	return {
		size: () => buf.length,
		piece: (from, max) => cutPiece(buf.subarray(from, Math.min(buf.length, from + max)), from, max, buf.length),
	};
}

/** readPiece reads complete lines of a session file (fileTranscript's piece). */
export function readPiece(path: string, from: number, max = MAX_PUSH_BYTES): { data: string; end: number; more: boolean } {
	return fileTranscript(path).piece(from, max);
}

/**
 * pushSession stores what the file grew by, in pieces of at most
 * MAX_PUSH_BYTES. When the archive holds something else it asks for a resync,
 * and this sends the whole file instead: replace with the first piece, then
 * the rest.
 */
export async function pushSession(opts: {
	call: <T>(tool: string, args: Record<string, unknown>) => Promise<T>;
	/** The transcript: a session file's path, or a Transcript. */
	file: string | Transcript;
	session: Manifest;
	from: number;
	now?: Date;
	max?: number;
}): Promise<{ record: SyncRecord; answer: PushAnswer }> {
	const now = opts.now ?? new Date();
	const max = opts.max ?? MAX_PUSH_BYTES;
	const t = typeof opts.file === "string" ? fileTranscript(opts.file) : opts.file;
	let from = opts.from;
	let replace = false;
	if (from > t.size()) {
		// The file is shorter than what was stored from it: start over.
		from = 0;
		replace = true;
	}
	let answer: PushAnswer;
	for (let rounds = 0; ; rounds++) {
		if (rounds > 10_000) throw new Error("pushing the session isn't getting anywhere");
		const piece = t.piece(from, max);
		answer = await opts.call<PushAnswer>("push_session", {
			session: opts.session,
			fromBytes: from,
			...(replace ? { replace: true } : {}),
			data: piece.data,
		});
		if (answer.resync) {
			// The archive holds a different transcript: send this whole file,
			// unless another machine has it further along, in which case
			// replacing it would throw that machine's work away.
			const size = t.size();
			if (replace || answer.bytes > size)
				throw new Error(
					`another machine has this session further along (${answer.bytes} bytes stored, ${size} here); not replacing it. Pull it there, or start a new session`,
				);
			from = 0;
			replace = true;
			continue;
		}
		replace = false;
		from = answer.bytes;
		if (!piece.more) break;
	}
	return {
		answer,
		record: { bytes: answer.bytes, entries: answer.entries, at: now.toISOString() },
	};
}

/** writePulled writes a pulled transcript where pi will find it. */
export function writePulled(sessionDir: string, cwd: string, id: string, data: string, startedAt?: string): string {
	const path = resumePath(sessionDir, cwd, id, startedAt);
	mkdirSync(join(sessionDir, encodeCwd(cwd)), { recursive: true });
	if (existsSync(path)) throw new Error(`${path} is already there; not overwriting it`);
	writeFileSync(path, data, { mode: 0o600 });
	return path;
}

/** machine is this machine's name, for the session record. */
export const machine = (): string => hostname();
