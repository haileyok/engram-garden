import assert from "node:assert/strict";
import { test } from "node:test";
import {
	CAPTURE_ENTRY,
	chunks,
	conversationMessages,
	knownMemories,
	looksSecret,
	memoryTags,
	parseCandidates,
	RECALL_MESSAGE,
	sanitizeSource,
	uncaptured,
	userPrompt,
} from "../engram/capture.ts";
import { formatMemories, recallBlock, shorten, spacesSummary } from "../engram/format.ts";

const msg = (id: string, role = "user", content = `text ${id}`) => ({
	type: "message",
	id,
	message: { role, content, timestamp: 0 },
});
const cap = (id: string, upTo: string, stored: any[] = []) => ({
	type: "custom",
	id,
	customType: CAPTURE_ENTRY,
	data: { upTo, stored, skipped: 0, chars: 0, at: "" },
});

test("uncaptured starts after the last capture on the branch", () => {
	const branch = [msg("a"), msg("b"), cap("c1", "b"), msg("d"), msg("e")];
	assert.deepEqual(
		uncaptured(branch).map((e) => e.id),
		["c1", "d", "e"],
	);
});

test("uncaptured ignores a capture of another branch", () => {
	const branch = [msg("a"), cap("c1", "elsewhere"), msg("b")];
	assert.equal(uncaptured(branch).length, 3);
});

test("uncaptured uses the furthest capture, wherever its entry landed", () => {
	// A capture's entry lands at the leaf when it finishes, after entries
	// it didn't read.
	const branch = [msg("a"), msg("b"), msg("c"), cap("c2", "b"), msg("d")];
	assert.deepEqual(
		uncaptured(branch).map((e) => e.id),
		["c", "c2", "d"],
	);
});

test("conversationMessages leaves out recalled memories and bookkeeping", () => {
	const entries = [
		msg("a"),
		{ type: "custom_message", id: "r", customType: RECALL_MESSAGE, content: "<memories>", display: true },
		{ type: "custom_message", id: "s", customType: "subagent", content: "done", display: true, timestamp: "2026-01-01T00:00:00Z" },
		cap("c", "a"),
		{ type: "model_change", id: "m" },
	];
	const out = conversationMessages(entries);
	assert.equal(out.length, 2);
	assert.equal(out[1].role, "custom");
	assert.equal(out[1].customType, "subagent");
});

test("knownMemories gathers recalled and captured memories", () => {
	const branch = [
		{ type: "custom_message", id: "r", customType: RECALL_MESSAGE, details: { memories: [{ uri: "u1", text: "one" }] } },
		{
			type: "message",
			id: "t",
			message: { role: "toolResult", toolName: "recall", details: { memories: [{ uri: "u2", text: "two" }, { uri: "u1", text: "one" }] } },
		},
		cap("c", "t", [{ uri: "u3", text: "three", tags: [] }]),
	];
	assert.deepEqual(knownMemories(branch).sort(), ["one", "three", "two"]);
});

test("chunks splits at line breaks", () => {
	const text = Array.from({ length: 100 }, (_, i) => `line ${i}`).join("\n");
	const parts = chunks(text, 120);
	assert.ok(parts.length > 5);
	assert.ok(parts.every((p) => p.length <= 120));
	assert.equal(parts.join(""), text);
});

test("parseCandidates reads JSON in a fence, drops thin and secret memories", () => {
	const answer = `Here you go:
\`\`\`json
{"memories": [
 {"text": "Engram Garden's pi extension runs engram-mcp as a child process and speaks MCP over stdio to it.", "tags": ["Engram-Garden", "decision", 3]},
 {"text": "too short", "tags": []},
 {"text": "The staging deploy uses the token ghp_abcdefghijklmnopqrstuvwxyz0123456789 for pulling images from GHCR.", "tags": []},
 {"text": "Hailey prefers worktrees under ~/worktrees/<repo>/<slug> with branches named hailey/<slug>.", "tags": ["preference"]}
]}
\`\`\``;
	const out = parseCandidates(answer, 5);
	assert.equal(out.length, 2);
	const tagged = parseCandidates(
		JSON.stringify({ memories: [{ text: "A memory long enough to keep, about the build cache and its eviction.", tags: ["ok", "sk-live-abcdefghijklmnopqrstu"] }] }),
		5,
	);
	assert.deepEqual(tagged[0].tags, ["ok"]);
	assert.deepEqual(out[0].tags, ["engram-garden", "decision"]);
	assert.equal(parseCandidates(answer, 1).length, 1);
	assert.throws(() => parseCandidates("nothing worth keeping", 5), /wasn't the JSON/);
	assert.throws(() => parseCandidates('{"memory": "x"}', 5), /wasn't the JSON/);
	assert.deepEqual(parseCandidates('{"memories": []}', 5), []);
	assert.deepEqual(parseCandidates(JSON.stringify({ memories: [{ text: "y".repeat(30_000) }] }), 5), []);
});

test("looksSecret", () => {
	assert.ok(looksSecret("key is sk-ant-api03abcdefghijklmnopqrstuvwx"));
	assert.ok(looksSecret("-----BEGIN OPENSSH PRIVATE KEY-----"));
	assert.ok(looksSecret("set password: hunter2hunter2"));
	assert.ok(looksSecret("Authorization: Bearer abcdefghijklmnopqrstuvwxyz012345"));
	assert.ok(looksSecret("export AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"));
	assert.ok(looksSecret('{"api_key": "abcd1234efgh5678"}'));
	assert.ok(!looksSecret("The SECRET_KEY setting is read from the environment."));
	assert.ok(!looksSecret("The token bucket is per space authority."));
	assert.ok(!looksSecret("Use `pi mcp login argocd` when the refresh token expires."));
});

test("userPrompt lists what's known and the bounds", () => {
	const p = userPrompt("[User]: hi", { cwd: "/w", project: "attie", date: "2026-10-21", known: ["a fact"], max: 3 });
	assert.match(p, /project: attie/);
	assert.match(p, /at most 3/);
	assert.match(p, /- a fact/);
	assert.match(p, /<transcript>\n\[User\]: hi\n<\/transcript>/);
});

test("shorten cuts at a word", () => {
	assert.equal(shorten("short", 10), "short");
	assert.equal(shorten("the quick brown fox jumps", 12), "the quick…");
});

test("recallBlock and formatMemories", () => {
	const m = {
		space: "coding-agents",
		uri: "at://x",
		author: "did:plc:a",
		text: "Attie deploys go through the deploy repo's workflow.",
		tags: ["attie"],
		createdAt: "2026-10-20T10:00:00Z",
		similarity: 812,
	};
	const block = recallBlock([m], 30);
	assert.match(block, /^<memories>/);
	assert.match(block, /coding-agents · 2026-10-20 · similarity 812 · tags: attie/);
	assert.match(block, /Attie deploys go through the…/);
	assert.match(block, /uri: at:\/\/x/);
	const list = formatMemories({ memories: [m], note: "space x isn't indexed" });
	assert.match(list, /^\[1\] coding-agents/);
	assert.match(list, /Note: space x/);
	assert.equal(formatMemories({ memories: [] }), "No memories found.");
});

test("spacesSummary flags spaces that aren't indexed", () => {
	const s = spacesSummary([
		{ name: "a", uri: "u", default: true, setUp: true, indexing: "granted" },
		{ name: "b", uri: "v", setUp: true, indexing: "lapsed" },
		{ name: "c", uri: "w", setUp: false },
	]);
	assert.match(s, /- a \(default\)\n/);
	assert.match(s, /- b — NOT indexed \(lapsed\)/);
	assert.doesNotMatch(s, /- c/);
});

test("sanitizeSource drops credentials from URLs", () => {
	assert.equal(sanitizeSource("https://x-access-token:ghp_abc@github.com/o/r.git"), "https://github.com/o/r.git");
	assert.equal(sanitizeSource("https://github.com/o/r"), "https://github.com/o/r");
	assert.equal(sanitizeSource("git@github.com:o/r.git"), "git@github.com:o/r.git");
	assert.equal(sanitizeSource("/home/me/repo"), "/home/me/repo");
	assert.equal(sanitizeSource("https://git.example.com/o/r.git?private_token=abc#x"), "https://git.example.com/o/r.git");
});

test("memoryTags keeps what engram accepts", () => {
	const many = Array.from({ length: 30 }, (_, i) => `t${i}`);
	assert.equal(memoryTags(many).length, 16);
	assert.deepEqual(memoryTags(["a", "a", " ", "x".repeat(129), "ghp_abcdefghijklmnopqrstuvwxyz0123456789", "b"]), ["a", "b"]);
	assert.equal(parseCandidates(JSON.stringify({ memories: [{ text: "x".repeat(50), tags: many }] }), 1)[0].tags.length, 8);
});

test("loadConfig rejects settings of the wrong type", async () => {
	const { mkdtempSync, writeFileSync } = await import("node:fs");
	const { tmpdir } = await import("node:os");
	const { join } = await import("node:path");
	const { loadConfig } = await import("../engram/config.ts");
	const dir = mkdtempSync(join(tmpdir(), "pi-engram-cfg-"));
	const prev = process.env.PI_CODING_AGENT_DIR;
	process.env.PI_CODING_AGENT_DIR = dir;
	try {
		writeFileSync(join(dir, "engram.json"), JSON.stringify({ captureTags: "auto" }));
		assert.throws(() => loadConfig(), /captureTags must be a list of strings/);
		writeFileSync(join(dir, "engram.json"), JSON.stringify({ recallLimit: "5" }));
		assert.throws(() => loadConfig(), /recallLimit must be a number/);
		writeFileSync(join(dir, "engram.json"), JSON.stringify({ recalLimit: 5 }));
		assert.throws(() => loadConfig(), /unknown setting recalLimit/);
		writeFileSync(join(dir, "engram.json"), JSON.stringify({ captureModel: "a/b", env: { X: "1" }, recallLimit: 3 }));
		assert.equal(loadConfig().recallLimit, 3);
	} finally {
		process.env.PI_CODING_AGENT_DIR = prev;
	}
});
