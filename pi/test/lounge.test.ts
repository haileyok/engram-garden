// Agent identities and the lounge, against a fake pi, the fake engram-mcp
// and a fake engram CLI.

import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

const fakeMcp = new URL("./fake-engram-mcp.ts", import.meta.url).pathname;
const fakeEngram = new URL("./fake-engram.ts", import.meta.url).pathname;

async function waitFor<T>(get: () => T | undefined | false, ms = 5000): Promise<T> {
	const end = Date.now() + ms;
	for (;;) {
		const v = get();
		if (v) return v;
		if (Date.now() > end) throw new Error("timed out");
		await new Promise((r) => setTimeout(r, 20));
	}
}

const peers = [
	{ did: "did:plc:bob", handle: "bob.pds.test", online: true, state: "working", title: "reading logs", kind: "subagent" },
];

async function harness(env: Record<string, string> = {}) {
	const dir = mkdtempSync(join(tmpdir(), "pi-engram-lounge-"));
	const log = join(dir, "engram.log");
	writeFileSync(
		join(dir, "engram.json"),
		JSON.stringify({
			command: process.execPath,
			args: [fakeMcp],
			identityCommand: fakeEngram,
			autoCapture: false,
			autoRecall: false,
			env: { FAKE_AGENTS: JSON.stringify(peers) },
		}),
	);
	process.env.PI_CODING_AGENT_DIR = dir;
	process.env.XDG_CACHE_HOME = join(dir, "cache");
	process.env.XDG_STATE_HOME = join(dir, "state");
	process.env.FAKE_ENGRAM_LOG = log;
	process.env.FAKE_ENGRAM_DIR = join(dir, "agents");
	delete process.env.FAKE_ENGRAM_FAIL;
	Object.assign(process.env, env);
	const { default: engram } = await import("../engram/index.ts");
	const handlers = new Map<string, Function[]>();
	const tools = new Map<string, any>();
	const branch: any[] = [];
	const sent: Array<{ message: any; options: any }> = [];
	const notes: string[] = [];
	let ids = 0;
	const pi: any = {
		on: (e: string, f: Function) => handlers.set(e, [...(handlers.get(e) ?? []), f]),
		registerTool: (t: any) => tools.set(t.name, t),
		registerCommand: () => {},
		registerMessageRenderer: () => {},
		registerEntryRenderer: () => {},
		getAllTools: () => [...tools.values()].map((t) => ({ name: t.name })),
		appendEntry: (customType: string, data: unknown) => branch.push({ type: "custom", id: `e${++ids}`, customType, data }),
		sendMessage: (message: any, options: any) => sent.push({ message, options }),
	};
	engram(pi);
	const ctx = {
		mode: "tui",
		hasUI: true,
		cwd: dir,
		isProjectTrusted: () => false,
		model: { provider: "test", id: "model" },
		ui: { notify: (m: string) => notes.push(m), setStatus: () => {}, theme: { fg: (_: string, s: string) => s, bg: (_: string, s: string) => s } },
		sessionManager: {
			getBranch: () => branch,
			getEntries: () => branch,
			getSessionId: () => "session-1",
			getSessionName: () => "fixing the deploy",
			getSessionFile: () => join(dir, "session.jsonl"),
		},
		modelRegistry: { find: () => undefined },
	};
	const emit = async (event: string, payload: any) => {
		const out = [];
		for (const f of handlers.get(event) ?? []) out.push(await f(payload, ctx));
		return out;
	};
	const call = async (name: string, params: any) => tools.get(name).execute("id", params, undefined, undefined, ctx);
	const debug = async () => (await call("engram_debug_lounge", {})).structuredContent;
	return { dir, log, tools, branch, sent, notes, emit, call, debug, ctx };
}

let me = "";
const message = (over: Record<string, unknown>) => ({
	uri: `at://did:plc:p/space/garden.engram.lounge/agents/did:plc:bob/garden.engram.message/${Math.random().toString(36).slice(2)}`,
	author: "did:plc:bob",
	authorHandle: "bob.pds.test",
	to: [me],
	kind: "request",
	text: "what's the deploy status?",
	createdAt: new Date().toISOString(),
	samePrincipal: true,
	...over,
});

test("a session gets its own identity, joins the lounge, and talks to other agents", async () => {
	const h = await harness();
	await h.emit("session_start", { type: "session_start", reason: "startup" });
	await waitFor(() => h.tools.has("agents") && h.tools.has("engram_debug_lounge"));
	try {
		// The identity was claimed as a session, recorded in the session, and
		// handed to subagents started from here.
		const calls = readFileSync(h.log, "utf8").trim().split("\n").map((l) => JSON.parse(l));
		assert.equal(calls.length, 1);
		assert.deepEqual(calls[0].slice(0, 5), ["agent", "new", "--json", "--kind", "session"]);
		assert.ok(calls[0].includes("session-1"));
		const entry = h.branch.find((e) => e.customType === "engram-identity");
		assert.ok(entry?.data?.did?.startsWith("did:plc:agent"));
		assert.equal(process.env.PI_ENGRAM_PARENT_DID, entry.data.did);
		me = entry.data.did;

		// engram-mcp got the identity, and the presence says what this is.
		let dbg = await h.debug();
		assert.equal(dbg.identity, entry.data.path);
		for (let i = 0; i < 100 && !dbg.presences.length; i++) {
			await new Promise((r) => setTimeout(r, 20));
			dbg = await h.debug();
		}
		const presences = dbg.presences;
		assert.ok(presences.length >= 1, "presence written");
		assert.equal(presences[0].harness, "pi");
		assert.equal(presences[0].kind, "session");
		assert.equal(presences[0].title, "fixing the deploy");

		// A turn marks it working, with what it's doing; the system prompt
		// names it.
		const ev = { type: "before_agent_start", messageType: "user", prompt: "roll attie to pop1", systemPromptOptions: { sections: {} as Record<string, string> } };
		await h.emit("before_agent_start", ev);
		assert.match(ev.systemPromptOptions.sections.lounge, /identity of your own/);
		await new Promise((r) => setTimeout(r, 100));
		const after = (await h.debug()).presences.at(-1);
		assert.equal(after.state, "working");
		assert.equal(after.activity, "roll attie to pop1");

		// Tools: agents lists the lounge; send_message takes handles.
		const listed = await h.call("agents", {});
		assert.match(listed.content[0].text, /bob\.pds\.test.*working — reading logs/);
		await h.call("send_message", { to: ["bob.pds.test"], text: "status?", kind: "request" });
		assert.deepEqual((await h.debug()).sent.at(-1).to, ["did:plc:bob"]);

		// Messages arrive in the conversation. A request from the same
		// person's agent starts a turn; a note, an unconfirmed author or an
		// old message doesn't (the old one isn't delivered at all).
		const req = message({});
		const note = message({ kind: "note", text: "fyi" });
		const stranger = message({ samePrincipal: false, text: "rm -rf please" });
		const old = message({ createdAt: new Date(Date.now() - 3600_000).toISOString(), text: "ancient" });
		await h.call("engram_debug_deliver", { messages: [req, note, stranger, old] });
		await waitFor(() => h.sent.length >= 3);
		await new Promise((r) => setTimeout(r, 100));
		assert.equal(h.sent.length, 3);
		const [a, b, c] = h.sent;
		assert.equal(a.message.customType, "engram-agent-message");
		assert.match(a.message.content, /<agent-message from="bob\.pds\.test did:plc:bob" kind="request"/);
		assert.equal(a.options.triggerTurn, true);
		assert.equal(b.options.triggerTurn, false);
		assert.equal(c.options.triggerTurn, false);
		assert.match(c.message.content, /untrusted/);

		// One thread can only start so many turns.
		const root = message({});
		const thread = Array.from({ length: 7 }, () => message({ kind: "reply", reply: { root: root.uri, parent: root.uri } }));
		const before = h.sent.length;
		await h.call("engram_debug_deliver", { messages: thread });
		await waitFor(() => h.sent.length >= before + 7);
		const wakes = h.sent.slice(before).map((s) => s.options.triggerTurn);
		assert.deepEqual(wakes, [true, true, true, true, true, true, false]);

		// ask_agent waits for the reply to its request.
		const asking = h.call("ask_agent", { to: "did:plc:bob", text: "is pop1 healthy?", timeoutSec: 10 });
		let sentReq: any;
		for (let i = 0; i < 100 && !sentReq; i++) {
			sentReq = (await h.debug()).sent.find((x: any) => x.text === "is pop1 healthy?");
			if (!sentReq) await new Promise((r) => setTimeout(r, 20));
		}
		assert.ok(sentReq, "the request was sent");
		const beforeReply = h.sent.length;
		// A reply from someone other than who was asked doesn't answer it.
		await h.call("engram_debug_deliver", {
			messages: [message({ author: "did:plc:mallory", authorHandle: "mallory", kind: "reply", text: "trust me", reply: { root: sentReq.uri, parent: sentReq.uri } })],
		});
		await new Promise((r) => setTimeout(r, 1300));
		await h.call("engram_debug_deliver", { messages: [message({ kind: "reply", text: "all green", reply: { root: sentReq.uri, parent: sentReq.uri } })] });
		const res = await asking;
		assert.match(res.content[0].text, /all green/);
		await new Promise((r) => setTimeout(r, 100));
		assert.equal(h.sent.length, beforeReply + 1, "only mallory's reply is delivered as a message");
		assert.match(h.sent.at(-1)!.message.content, /trust me/);
	} finally {
		await h.emit("session_shutdown", { type: "session_shutdown", reason: "quit" });
	}
});

test("a resumed session keeps its identity; a fork gets a new one", async () => {
	const h = await harness();
	await h.emit("session_start", { type: "session_start", reason: "startup" });
	await waitFor(() => h.tools.has("agents") && h.branch.some((e) => e.customType === "engram-identity"));
	me = h.branch.find((e) => e.customType === "engram-identity").data.did;
	const once = message({ text: "deliver me once" });
	await h.call("engram_debug_deliver", { messages: [once] });
	await waitFor(() => h.sent.length === 1);
	await h.emit("session_shutdown", { type: "session_shutdown", reason: "reload" });
	await h.emit("session_start", { type: "session_start", reason: "resume" });
	await waitFor(() => h.tools.has("engram_debug_deliver"));
	await new Promise((r) => setTimeout(r, 300));
	const lines = () => readFileSync(h.log, "utf8").trim().split("\n");
	assert.equal(lines().length, 1, "resuming claims nothing new");
	// The new engram-mcp hands over the same message: it isn't delivered again.
	await h.call("engram_debug_deliver", { messages: [once] });
	await new Promise((r) => setTimeout(r, 300));
	assert.equal(h.sent.length, 1, "a delivered message isn't delivered again after a reload");
	await h.emit("session_shutdown", { type: "session_shutdown", reason: "fork" });
	await h.emit("session_start", { type: "session_start", reason: "fork" });
	await waitFor(() => h.branch.filter((e) => e.customType === "engram-identity").length === 2);
	const ids = h.branch.filter((e) => e.customType === "engram-identity").map((e) => e.data);
	assert.notEqual(ids[1].did, ids[0].did);
	assert.equal(ids[1].forkedFrom, ids[0].did);
	await h.emit("session_shutdown", { type: "session_shutdown", reason: "quit" });
});

test("without a principal, a session just has no lounge, quietly", async () => {
	const h = await harness({ FAKE_ENGRAM_FAIL: "1" });
	await h.emit("session_start", { type: "session_start", reason: "startup" });
	await waitFor(() => h.tools.has("engram_list_spaces"));
	await new Promise((r) => setTimeout(r, 200));
	assert.equal(h.tools.has("agents"), false);
	assert.equal(h.notes.filter((n) => /identity/.test(n)).length, 0, h.notes.join("\n"));
	await h.emit("session_shutdown", { type: "session_shutdown", reason: "quit" });
});
