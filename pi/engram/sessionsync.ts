// Session sync: a session's transcript, and where the work stands, kept in
// Engram so the session can be picked up on another machine. The manifest is
// a garden.engram.session record; the bytes go up in chunks of complete JSONL
// lines. This side only has to know how much of the file it has stored, and
// what to say about the work.

import { execFile } from "node:child_process";
import { existsSync, mkdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { homedir, hostname } from "node:os";
import { join } from "node:path";

/** SESSION_SYNC_ENTRY records how much of the transcript is stored. */
export const SESSION_SYNC_ENTRY = "engram-session-sync";

/** HARness is what this writes in a session record. */
export const HARNESS = "pi";

export interface SyncRecord {
	/** Bytes of the transcript the space reported holding after this sync. */
	bytes: number;
	/** Entries the space reported, and how it stored the bytes. */
	entries?: number;
	mode?: string;
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

/** Manifest is the garden.engram.session record this pushes. */
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
	mode: string;
	bytes: number;
	entries: number;
	storedBytes: number;
	resync?: boolean;
	note?: string;
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

/**
 * readSuffix reads a session file from `from` to the end. A caller that
 * believes more is stored than the file holds gets the whole file and a
 * fromBytes of 0, so the space replaces what it has instead of guessing.
 */
export function readSuffix(path: string, from: number): { fromBytes: number; data: string; size: number } {
	const size = statSync(path).size;
	const start = from > 0 && from <= size ? from : 0;
	const buf = readFileSync(path);
	return { fromBytes: start, data: buf.subarray(start).toString("utf8"), size };
}

/**
 * pushSession stores what the file grew by. When the space holds something
 * else, it asks for a resync and this sends the whole file instead.
 */
export async function pushSession(opts: {
	call: <T>(tool: string, args: Record<string, unknown>) => Promise<T>;
	file: string;
	session: Manifest;
	from: number;
	now?: Date;
}): Promise<{ record: SyncRecord; answer: PushAnswer }> {
	const now = opts.now ?? new Date();
	let { fromBytes, data } = readSuffix(opts.file, opts.from);
	let answer = await opts.call<PushAnswer>("push_session", {
		session: { ...opts.session, sessionId: opts.session.sessionId },
		fromBytes,
		data,
	});
	if (answer.resync) {
		// The space holds a different transcript: send this whole file, unless
		// another machine has it further along, in which case replacing it
		// would throw that machine's work away.
		const whole = readSuffix(opts.file, 0);
		if (answer.bytes > whole.size)
			throw new Error(
				`another machine has this session further along (${answer.bytes} bytes stored, ${whole.size} here); not replacing it. Pull it there, or start a new session`,
			);
		answer = await opts.call<PushAnswer>("push_session", { session: opts.session, fromBytes: 0, replace: true, data: whole.data });
	}
	return {
		answer,
		record: {
			bytes: answer.bytes,
			entries: answer.entries,
			mode: answer.mode,
			at: now.toISOString(),
			...(answer.note ? { error: answer.note } : {}),
		},
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
