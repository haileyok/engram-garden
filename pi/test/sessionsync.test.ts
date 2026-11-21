import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtempSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import {
	encodeCwd,
	gitInfo,
	lastSync,
	pushSession,
	readPiece,
	resumePath,
	SESSION_SYNC_ENTRY,
	shortenLine,
	startedAtOf,
	summaryOf,
	syncedBytes,
	titleOf,
	writePulled,
} from "../engram/sessionsync.ts";

const msg = (id: string, role: string, text: string) => ({
	type: "message",
	id,
	message: { role, content: [{ type: "text", text }], timestamp: 0 },
});

test("encodeCwd names a session folder the way pi does", () => {
	assert.equal(encodeCwd("/root/bluesky/attie"), "--root-bluesky-attie--");
	assert.equal(encodeCwd("/root/.pi"), "--root-.pi--");
	assert.equal(encodeCwd("/root/worktrees/pi-config/subagent-sidebar"), "--root-worktrees-pi-config-subagent-sidebar--");
});

test("resumePath puts a pulled session where /resume looks", () => {
	const path = resumePath("/agent/sessions", "/root/bluesky/attie", "01a16641", "2026-10-23T07:00:00.000Z");
	assert.equal(path, "/agent/sessions/--root-bluesky-attie--/2026-10-23T07-00-00-000Z_01a16641.jsonl");
	// Without a start time the file still lands in the right folder.
	assert.match(resumePath("/a", "/root/x", "id"), /^\/a\/--root-x--\/.+-_id\.jsonl$|^\/a\/--root-x--\/.+_id\.jsonl$/);
});

test("syncedBytes takes the most any sync entry claims", () => {
	const entries = [
		{ type: "custom", id: "c1", customType: SESSION_SYNC_ENTRY, data: { bytes: 100, at: "2026-10-23T00:00:00Z" } },
		msg("m1", "user", "hello"),
		{ type: "custom", id: "c2", customType: SESSION_SYNC_ENTRY, data: { bytes: 300, at: "2026-10-23T00:01:00Z" } },
		{ type: "custom", id: "c3", customType: "other", data: { bytes: 999 } },
	];
	assert.equal(syncedBytes(entries as any[]), 300);
	assert.equal(syncedBytes([]), 0);
	assert.equal(lastSync(entries as any[])?.bytes, 300);
});

test("titleOf uses the session name, else its first prompt", () => {
	const branch = [msg("m1", "user", "audit the template handlers for cross-tenant reads"), msg("m2", "assistant", "ok")];
	assert.equal(titleOf(branch as any[]), "audit the template handlers for cross-tenant reads");
	assert.equal(titleOf(branch as any[], "tenancy audit"), "tenancy audit");
	// Multi-line prompts flatten to one line, and long ones are cut.
	assert.equal(titleOf([msg("m1", "user", "first\n\n  second")] as any[]), "first second");
	const long = titleOf([msg("m1", "user", "x".repeat(400))] as any[], undefined);
	assert.ok(long.length <= 120 && long.endsWith("…"));
	assert.equal(titleOf([] as any[]), "session");
});

test("summaryOf is the newest compaction summary on the branch", () => {
	const branch = [
		{ type: "compaction", id: "c1", summary: "first pass" },
		msg("m1", "user", "more"),
		{ type: "compaction", id: "c2", summary: "second pass" },
	];
	assert.equal(summaryOf(branch as any[]), "second pass");
	assert.equal(summaryOf([]), "");
});

test("startedAtOf prefers the header, then the first entry", () => {
	assert.equal(startedAtOf({ timestamp: "2026-10-23T00:00:00Z" }, []), "2026-10-23T00:00:00Z");
	assert.equal(startedAtOf(undefined, [{ type: "message", id: "m1", timestamp: "2026-10-23T00:00:01Z" }]), "2026-10-23T00:00:01Z");
	assert.equal(startedAtOf(undefined, []), undefined);
});

test("shortenLine keeps short text and cuts long text", () => {
	assert.equal(shortenLine("  a   b  ", 10), "a b");
	assert.equal(shortenLine("abcdefghij", 4), "abc…");
});

test("readPiece reads complete lines from the offset, at most max bytes", () => {
	const dir = mkdtempSync(join(tmpdir(), "pi-sync-"));
	const file = join(dir, "s.jsonl");
	writeFileSync(file, "aaaa\nbbbb\ncc");
	assert.deepEqual(readPiece(file, 0), { data: "aaaa\nbbbb\n", end: 10, more: false });
	assert.deepEqual(readPiece(file, 5), { data: "bbbb\n", end: 10, more: false });
	// A partial last line waits.
	assert.deepEqual(readPiece(file, 10), { data: "", end: 10, more: false });
	// A cap cuts on a line, and says there's more.
	assert.deepEqual(readPiece(file, 0, 7), { data: "aaaa\n", end: 5, more: true });
	// Multi-byte text survives the cut.
	writeFileSync(file, "é\nü\n");
	assert.deepEqual(readPiece(file, 0), { data: "é\nü\n", end: 6, more: false });
	// A line longer than the cap can't be stored.
	writeFileSync(file, "aaaaaaaaaa\n");
	assert.throws(() => readPiece(file, 0, 4), /longer than 4 bytes/);
});

/** fakeArchive answers push_session the way the archive does. */
function fakeArchive(stored = "") {
	const calls: Array<Record<string, any>> = [];
	const call = async <T,>(_tool: string, args: Record<string, any>): Promise<T> => {
		calls.push(args);
		if (!args.replace && stored !== "" && args.fromBytes !== Buffer.byteLength(stored))
			return { session: "s", bytes: Buffer.byteLength(stored), entries: 0, storedBytes: 0, resync: true } as unknown as T;
		const data = String(args.data);
		stored = (args.replace ? "" : stored) + data;
		return { session: "s", bytes: Buffer.byteLength(stored), entries: stored.split("\n").length - 1, storedBytes: Buffer.byteLength(data) } as unknown as T;
	};
	return { call, calls, stored: () => stored };
}

const manifest = { sessionId: "s", title: "t", summary: "", status: "active" as const };

test("pushSession sends what grew, in pieces", async () => {
	const dir = mkdtempSync(join(tmpdir(), "pi-sync-"));
	const file = join(dir, "s.jsonl");
	writeFileSync(file, "one\ntwo\nthree\nfour\n");
	const a = fakeArchive();
	const { record } = await pushSession({ call: a.call, file, session: manifest, from: 0, max: 9, now: new Date("2026-10-23T07:00:00Z") });
	assert.equal(a.stored(), "one\ntwo\nthree\nfour\n");
	assert.deepEqual(
		a.calls.map((c) => c.data),
		["one\ntwo\n", "three\n", "four\n"],
	);
	assert.deepEqual(record, { bytes: 19, entries: 4, at: "2026-10-23T07:00:00.000Z" });

	// Then only what grew.
	writeFileSync(file, "one\ntwo\nthree\nfour\nfive\n");
	await pushSession({ call: a.call, file, session: manifest, from: record.bytes, max: 9 });
	assert.equal(a.calls.at(-1)!.data, "five\n");
	assert.equal(a.stored(), "one\ntwo\nthree\nfour\nfive\n");
});

test("pushSession answers a resync with the whole file, replace first", async () => {
	const dir = mkdtempSync(join(tmpdir(), "pi-sync-"));
	const file = join(dir, "s.jsonl");
	writeFileSync(file, "one\ntwo\nthree\n");
	const a = fakeArchive("old\n");
	await pushSession({ call: a.call, file, session: manifest, from: 8, max: 9 });
	assert.equal(a.stored(), "one\ntwo\nthree\n");
	assert.equal(a.calls[1].replace, true);
	assert.equal(a.calls[1].data, "one\ntwo\n");
	assert.equal(a.calls[2].replace, undefined);
});

test("gitInfo reads the repository, its branch, commit and dirty state", async (t) => {
	const dir = mkdtempSync(join(tmpdir(), "pi-git-"));
	try {
		execFileSync("git", ["-C", dir, "init", "-q"], { stdio: "ignore" });
		execFileSync("git", ["-C", dir, "config", "user.email", "t@t"], { stdio: "ignore" });
		execFileSync("git", ["-C", dir, "config", "user.name", "t"], { stdio: "ignore" });
		execFileSync("git", ["-C", dir, "remote", "add", "origin", "git@github.com:haileyok/pi-config.git"], { stdio: "ignore" });
		writeFileSync(join(dir, "a.txt"), "a\n");
		execFileSync("git", ["-C", dir, "add", "-A"], { stdio: "ignore" });
		execFileSync("git", ["-C", dir, "commit", "-qm", "one"], { stdio: "ignore" });
		const clean = await gitInfo(dir);
		assert.equal(clean.repo, "git@github.com:haileyok/pi-config.git");
		assert.equal(clean.branch, "main");
		assert.match(clean.gitHead ?? "", /^[0-9a-f]{12}$/);
		assert.equal(clean.gitDirty, undefined);
		// A modified tracked file makes it dirty; an untracked one doesn't.
		writeFileSync(join(dir, "a.txt"), "b\n");
		assert.equal((await gitInfo(dir)).gitDirty, true);
		writeFileSync(join(dir, "a.txt"), "a\n");
		writeFileSync(join(dir, "b.txt"), "untracked\n");
		assert.equal((await gitInfo(dir)).gitDirty, undefined);
	} catch (err) {
		t.skip(`git isn't available: ${err}`);
	}
	// A directory that isn't a repository says nothing rather than failing.
	assert.deepEqual(await gitInfo(mkdtempSync(join(tmpdir(), "pi-nogit-"))), {});
});

test("writePulled writes the transcript in a fresh file and refuses to overwrite", () => {
	const dir = mkdtempSync(join(tmpdir(), "pi-pull-"));
	const path = writePulled(dir, "/root/bluesky/attie", "01a16641", '{"type":"session"}\n', "2026-10-23T07:00:00.000Z");
	assert.equal(readFileSync(path, "utf8"), '{"type":"session"}\n');
	assert.throws(() => writePulled(dir, "/root/bluesky/attie", "01a16641", "x", "2026-10-23T07:00:00.000Z"), /already there/);
	// The folder is created when it's missing.
	const deep = join(dir, "made");
	mkdirSync(deep);
	writePulled(deep, "/tmp/elsewhere", "id2", "x", "2026-10-23T07:00:00.000Z");
	assert.ok(readFileSync(join(deep, "--tmp-elsewhere--", "2026-10-23T07-00-00-000Z_id2.jsonl"), "utf8").startsWith("x"));
});

test("pushSession won't replace a session another machine has further along", async () => {
	const dir = mkdtempSync(join(tmpdir(), "pi-sync-"));
	const file = join(dir, "s.jsonl");
	writeFileSync(file, "one\n");
	const call = async <T,>(_tool: string, _args: Record<string, any>): Promise<T> =>
		({ session: "s", bytes: 4096, entries: 9, storedBytes: 0, resync: true }) as unknown as T;
	await assert.rejects(
		pushSession({ call, file, session: { sessionId: "s", title: "t", summary: "", status: "active" }, from: 0 }),
		/further along/,
	);
});

// An empty transcript is still a resync: the space has bytes, this file
// doesn't, so nothing is replaced.
test("pushSession won't replace with an empty file either", async () => {
	const dir = mkdtempSync(join(tmpdir(), "pi-sync-"));
	const file = join(dir, "s.jsonl");
	writeFileSync(file, "");
	const call = async <T,>(_tool: string, _args: Record<string, any>): Promise<T> =>
		({ session: "s", bytes: 10, entries: 1, storedBytes: 0, resync: true }) as unknown as T;
	await assert.rejects(
		pushSession({ call, file, session: { sessionId: "s", title: "t", summary: "", status: "active" }, from: 0 }),
		/further along/,
	);
});
