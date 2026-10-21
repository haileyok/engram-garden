// The extension against a fake pi and the fake engram-mcp: what reaches
// the model on a prompt, and what capturing stores.

import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

const fake = new URL("./fake-engram-mcp.ts", import.meta.url).pathname;

const seeded = [
	{
		space: "coding-agents",
		uri: "at://m/1",
		author: "did:plc:b",
		text: "Attie deploys to pop1 go through the deploy repo's Deploy Attie workflow, one SHA for every service.",
		tags: ["attie"],
		createdAt: "2026-10-20T00:00:00Z",
	},
	{
		space: "coding-agents",
		uri: "at://m/2",
		author: "did:plc:b",
		text: "The office coffee machine needs descaling every month.",
		tags: [],
		createdAt: "2026-10-01T00:00:00Z",
	},
];

interface Harness {
	handlers: Map<string, Function[]>;
	tools: Map<string, any>;
	commands: Map<string, any>;
	branch: any[];
	notes: string[];
	ctx: any;
	emit(event: string, payload: any): Promise<any[]>;
	completions: Array<string | ((signal: AbortSignal) => Promise<any>)>;
	prompts: string[];
	addMessage(role: string, content: string): void;
}

async function harness(settings: Record<string, unknown> = {}): Promise<Harness> {
	const dir = mkdtempSync(join(tmpdir(), "pi-engram-"));
	writeFileSync(
		join(dir, "engram.json"),
		JSON.stringify({ command: process.execPath, args: [fake], env: { FAKE_MEMORIES: JSON.stringify(seeded) }, ...settings }),
	);
	process.env.PI_CODING_AGENT_DIR = dir;
	process.env.XDG_CACHE_HOME = join(dir, "cache");
	const { default: engram } = await import("../engram/index.ts");

	const h: Harness = {
		handlers: new Map(),
		tools: new Map(),
		commands: new Map(),
		branch: [],
		notes: [],
		completions: [],
		prompts: [],
		ctx: undefined,
		addMessage: () => {},
		async emit(event, payload) {
			const out = [];
			for (const f of h.handlers.get(event) ?? []) out.push(await f(payload, h.ctx));
			return out;
		},
	};
	let ids = 0;
	const pi: any = {
		on: (e: string, f: Function) => h.handlers.set(e, [...(h.handlers.get(e) ?? []), f]),
		registerTool: (t: any) => h.tools.set(t.name, t),
		registerCommand: (n: string, c: any) => h.commands.set(n, c),
		registerMessageRenderer: () => {},
		registerEntryRenderer: () => {},
		getAllTools: () => [...h.tools.values()].map((t) => ({ name: t.name })),
		appendEntry: (customType: string, data: unknown) =>
			h.branch.push({ type: "custom", id: `e${++ids}`, customType, data }),
	};
	engram(pi);
	h.ctx = {
		mode: "tui",
		hasUI: true,
		cwd: dir,
		isProjectTrusted: () => false,
		model: { provider: "test", id: "model" },
		ui: {
			notify: (m: string) => h.notes.push(m),
			setStatus: () => {},
			theme: { fg: (_: string, s: string) => s, bg: (_: string, s: string) => s, bold: (s: string) => s },
		},
		sessionManager: { getBranch: () => h.branch, getSessionFile: () => join(dir, "session.jsonl") },
		modelRegistry: {
			find: () => undefined,
			complete: async (_model: any, context: any, opts: any) => {
				h.prompts.push(context.messages[0].content[0].text);
				const next = h.completions.shift() ?? '{"memories": []}';
				if (typeof next === "function") return next(opts.signal);
				return { stopReason: "stop", content: [{ type: "text", text: next }] };
			},
		},
	};
	h.addMessage = (role: string, content: string) =>
		h.branch.push({ type: "message", id: `e${++ids}`, message: { role, content: [{ type: "text", text: content }], timestamp: 0 } });
	return h;
}

async function waitFor(cond: () => boolean, ms = 3000) {
	const end = Date.now() + ms;
	while (!cond()) {
		if (Date.now() > end) throw new Error("timed out");
		await new Promise((r) => setTimeout(r, 20));
	}
}

async function start(h: Harness) {
	await h.emit("session_start", { type: "session_start" });
	await h.emit("before_agent_start", {
		type: "before_agent_start",
		messageType: "custom",
		systemPromptOptions: { sections: {} },
	}); // warms nothing; just sets the section
}

const startEvent = (prompt: string) => ({
	type: "before_agent_start",
	messageType: "user",
	prompt,
	systemPromptOptions: { sections: {} as Record<string, string> },
});

test("auto-recall adds relevant memories once, and the memory section", async () => {
	const h = await harness({ recallMinSimilarity: 300 });
	try {
		await start(h);
		const ev = startEvent("how do attie deploys to pop1 work, which workflow?");
		const [res] = await h.emit("before_agent_start", ev);
		assert.match(ev.systemPromptOptions.sections.memory, /Engram is your long-term memory/);
		assert.ok(res?.message, "a recall message");
		assert.match(res.message.content, /Deploy Attie workflow/);
		assert.doesNotMatch(res.message.content, /coffee/);
		assert.deepEqual(
			res.message.details.memories.map((m: any) => m.uri),
			["at://m/1"],
		);

		// The same memory isn't added again on this branch.
		h.branch.push({ type: "custom_message", id: "r1", customType: "engram-recall", details: res.message.details });
		const [again] = await h.emit("before_agent_start", startEvent("and attie deploys to pop2, same workflow?"));
		assert.equal(again, undefined);

		// Short prompts aren't searched.
		const [short] = await h.emit("before_agent_start", startEvent("go ahead"));
		assert.equal(short, undefined);
	} finally {
		await h.emit("session_shutdown", { type: "session_shutdown", reason: "reload" });
	}
});

test("auto-recall gives up quietly when engram-mcp can't start", async () => {
	const h = await harness({ env: { FAKE_FAIL_START: "1" } });
	try {
		await h.emit("session_start", { type: "session_start" });
		await waitFor(() => h.notes.some((n) => /run engram login/.test(n)));
		const [res] = await h.emit("before_agent_start", startEvent("how do attie deploys to pop1 work?"));
		assert.equal(res, undefined);
	} finally {
		await h.emit("session_shutdown", { type: "session_shutdown", reason: "reload" });
	}
});

test("capture stores new memories, skips known ones, and doesn't reread", async () => {
	const h = await harness({ captureMinChars: 100, captureExitMinChars: 50, captureTags: ["auto", "token=abcdefgh12345678"] });
	try {
		await start(h);
		h.addMessage("user", "Let's figure out how Attie deploys work and fix the flaky queue test while we're at it.");
		h.addMessage(
			"assistant",
			"The flaky queue test was racing the janitor goroutine; I gave it its own clock and it passes 500 runs in a row.",
		);
		h.completions.push(
			JSON.stringify({
				memories: [
					{
						text: "Attie's queue test was flaky because it raced the janitor goroutine; giving the test its own clock fixed it (500 clean runs).",
						tags: ["gotcha"],
					},
					// A restatement of a stored memory.
					{
						text: "Attie deploys to pop1 go through the deploy repo's Deploy Attie workflow, one SHA for every service.",
						tags: ["attie"],
					},
				],
			}),
		);
		await h.emit("agent_settled", { type: "agent_settled", aborted: false });
		await waitFor(() => h.branch.some((e) => e.customType === "engram-capture"));

		const rec = h.branch.find((e) => e.customType === "engram-capture").data;
		assert.equal(rec.stored.length, 1);
		assert.equal(rec.skipped, 1);
		assert.deepEqual(rec.stored[0].tags.slice(0, 1), ["auto"]);
		assert.ok(!rec.stored[0].tags.some((t: string) => t.startsWith("token=")), "secret-looking tags left out");
		assert.ok(rec.stored[0].tags.includes("gotcha"));
		assert.match(h.prompts[0], /janitor goroutine/);
		assert.match(h.prompts[0], /Already known:\n\(none\)/);
		assert.ok(h.notes.some((n) => /remembered 1 memory/.test(n)));

		// Nothing new: no second read.
		await h.emit("agent_settled", { type: "agent_settled", aborted: false });
		await new Promise((r) => setTimeout(r, 100));
		assert.equal(h.prompts.length, 1);

		// What's added later is read, and what was stored counts as known.
		h.addMessage("user", "Also: from now on always run the race detector when testing the queue package, please remember that.");
		await h.emit("session_shutdown", { type: "session_shutdown", reason: "quit" });
		assert.equal(h.prompts.length, 2);
		assert.doesNotMatch(h.prompts[1], /janitor goroutine; I gave/);
		assert.match(h.prompts[1], /race detector/);
		assert.match(h.prompts[1], /Already known:\n- Attie's queue test was flaky/);
	} finally {
		await h.emit("session_shutdown", { type: "session_shutdown", reason: "reload" });
	}
});

test("capture is off for sessions that aren't saved", async () => {
	const h = await harness({ captureMinChars: 10 });
	try {
		await start(h);
		h.ctx.sessionManager.getSessionFile = () => undefined;
		h.addMessage("user", "a long enough message to capture something from it");
		await h.emit("agent_settled", { type: "agent_settled", aborted: false });
		await h.emit("session_shutdown", { type: "session_shutdown", reason: "quit" });
		assert.equal(h.prompts.length, 0);
	} finally {
		await h.emit("session_shutdown", { type: "session_shutdown", reason: "reload" });
	}
});

test("remember refuses text that holds a credential", async () => {
	const h = await harness();
	try {
		await start(h);
		const remember = h.tools.get("remember");
		await assert.rejects(
			remember.execute("t1", { text: "The deploy key is ghp_abcdefghijklmnopqrstuvwxyz0123456789" }, undefined, undefined, h.ctx),
			/credential/,
		);
		const out = await remember.execute("t2", { text: "Deploys need the GHCR pull secret from 1Password." }, undefined, undefined, h.ctx);
		assert.match(out.content[0].text, /^Remembered: at:\/\//);
		const recall = h.tools.get("recall");
		const found = await recall.execute("t3", { query: "GHCR pull secret" }, undefined, undefined, h.ctx);
		assert.match(found.content[0].text, /^\[1\] coding-agents/);
		assert.equal(found.details.memories[0].text, "Deploys need the GHCR pull secret from 1Password.");
		// engram-mcp's other tools are there for codemode.
		await waitFor(() => h.tools.has("engram_list_spaces"));
		assert.equal(h.tools.get("engram_list_spaces").exposure, "codemode");
	} finally {
		await h.emit("session_shutdown", { type: "session_shutdown", reason: "reload" });
	}
});

const answer = (...texts: string[]) =>
	JSON.stringify({ memories: texts.map((text) => ({ text, tags: [] })) });

/** A model answer that waits until release() (or the abort signal). */
function gate() {
	let release!: () => void;
	const opened = new Promise<void>((r) => (release = r));
	const fn = async (signal: AbortSignal) => {
		await Promise.race([opened, new Promise((r) => signal.addEventListener("abort", r))]);
		if (signal.aborted) return { stopReason: "aborted", content: [], errorMessage: "aborted" };
		return { stopReason: "stop", content: [{ type: "text", text: '{"memories": []}' }] };
	};
	return { fn, release };
}

test("a capture asked for during a pass runs next, with its own threshold", async () => {
	const h = await harness({ captureMinChars: 100 });
	try {
		await start(h);
		h.addMessage("user", "x".repeat(200));
		const g = gate();
		h.completions.push(g.fn);
		await h.emit("agent_settled", { type: "agent_settled", aborted: false });
		await waitFor(() => h.prompts.length === 1);
		// A short tail, below captureMinChars: only /engram capture reads it.
		h.addMessage("user", "Hailey wants the queue tests run with -race from now on, every time.");
		h.completions.push(answer("Hailey wants the queue package's tests always run with the race detector (-race)."));
		const cmd = h.commands.get("engram").handler("capture", h.ctx);
		g.release();
		await cmd;
		assert.equal(h.prompts.length, 2);
		const recs = h.branch.filter((e) => e.customType === "engram-capture").map((e) => e.data);
		assert.equal(recs.length, 2);
		assert.equal(recs[1].stored.length, 1);
	} finally {
		await h.emit("session_shutdown", { type: "session_shutdown", reason: "reload" });
	}
});

test("captureMax applies to each chunk of a long transcript", async () => {
	const h = await harness({ captureMinChars: 100, captureMax: 3 });
	try {
		await start(h);
		for (let i = 0; i < 12; i++) h.addMessage("user", `${"lorem ipsum ".repeat(800)} part ${i}`);
		h.completions.push(
			answer("First distinct memory about the alpha subsystem and its deploy.", "Second distinct memory about the beta queue and retries."),
			answer("Third distinct memory about the gamma cache eviction policy.", "Fourth distinct memory about the delta ingest backfill job."),
		);
		await h.commands.get("engram").handler("capture", h.ctx);
		assert.equal(h.prompts.length, 2, "two chunks read");
		assert.match(h.prompts[0], /at most 3 memories/);
		assert.match(h.prompts[1], /at most 3 memories/);
		assert.match(h.prompts[1], /Already known:\n- First distinct memory/, "chunk 2 knows what chunk 1 stored");
		const rec = h.branch.find((e) => e.customType === "engram-capture").data;
		assert.equal(rec.stored.length, 4);
	} finally {
		await h.emit("session_shutdown", { type: "session_shutdown", reason: "reload" });
	}
});

test("a cut-off answer doesn't mark the stretch as read", async () => {
	const h = await harness({ captureMinChars: 10 });
	try {
		await start(h);
		h.addMessage("user", "something long enough to be read by capturing");
		h.completions.push(async () => ({ stopReason: "length", content: [{ type: "text", text: '{"memories": [{"te' }] }));
		await h.commands.get("engram").handler("capture", h.ctx);
		assert.equal(h.branch.filter((e) => e.customType === "engram-capture").length, 0);
		assert.ok(h.notes.some((n) => /stopped early \(length\)/.test(n)));
	} finally {
		await h.emit("session_shutdown", { type: "session_shutdown", reason: "reload" });
	}
});

test("a capture that fails partway records what it stored and reads the stretch again", async () => {
	const h = await harness({ captureMinChars: 10 });
	try {
		await start(h);
		h.addMessage("user", "a conversation with two things worth keeping in it, at least");
		h.completions.push(
			answer("The janitor goroutine owns the queue's clock in Attie's tests.", "FAIL_REMEMBER this one can't be stored anywhere."),
		);
		await h.commands.get("engram").handler("capture", h.ctx);
		const recs = h.branch.filter((e) => e.customType === "engram-capture").map((e) => e.data);
		assert.equal(recs.length, 1);
		assert.equal(recs[0].upTo, "");
		assert.equal(recs[0].stored.length, 1);
		assert.match(recs[0].error, /PDS said no/);

		await h.commands.get("engram").handler("capture", h.ctx);
		assert.equal(h.prompts.length, 2);
		assert.match(h.prompts[1], /two things worth keeping/, "read again");
		assert.match(h.prompts[1], /Already known:\n- The janitor goroutine/);
	} finally {
		await h.emit("session_shutdown", { type: "session_shutdown", reason: "reload" });
	}
});

test("a pass outliving its session doesn't disturb the next one", async () => {
	const h = await harness({ captureMinChars: 10, captureExitTimeoutMs: 100 });
	try {
		await start(h);
		h.addMessage("user", "a conversation long enough to capture from");
		// A model call that ignores the abort.
		let releaseOld!: () => void;
		const old = new Promise<void>((r) => (releaseOld = r));
		h.completions.push(async () => {
			await old;
			return { stopReason: "stop", content: [{ type: "text", text: answer("Old session memory that must not be stored.") }] };
		});
		await h.emit("agent_settled", { type: "agent_settled", aborted: false });
		await waitFor(() => h.prompts.length === 1);
		// Switching sessions gives up on it after captureExitTimeoutMs.
		await h.emit("session_shutdown", { type: "session_shutdown", reason: "new" });
		h.branch.length = 0;
		await start(h);
		h.addMessage("user", "the new session's conversation, also long enough");
		const g = gate();
		h.completions.push(g.fn);
		await h.emit("agent_settled", { type: "agent_settled", aborted: false });
		await waitFor(() => h.prompts.length === 2);

		// The old pass ends while the new one runs; it must not free the slot.
		releaseOld();
		await new Promise((r) => setTimeout(r, 50));
		await h.emit("agent_settled", { type: "agent_settled", aborted: false });
		await new Promise((r) => setTimeout(r, 50));
		assert.equal(h.prompts.length, 2, "no second pass alongside the running one");
		g.release();
		await waitFor(() => h.branch.some((e) => e.customType === "engram-capture"));
		assert.ok(!h.branch.some((e) => e.data?.stored?.some((m: any) => /Old session/.test(m.text))));
	} finally {
		await h.emit("session_shutdown", { type: "session_shutdown", reason: "reload" });
	}
});

test("remember strips credentials from the source; pass-through tools run", async () => {
	const h = await harness();
	try {
		await start(h);
		const out = await h.tools
			.get("remember")
			.execute("t", { text: "Builds pull from the internal mirror.", source: "https://bot:tok123456@git.example.com/o/r" }, undefined, undefined, h.ctx);
		const uri = out.content[0].text.replace("Remembered: ", "");
		const got = await h.tools.get("get_memory").execute("t2", { uri }, undefined, undefined, h.ctx);
		assert.match(got.content[0].text, /source: https:\/\/git\.example\.com\/o\/r/);
		await waitFor(() => h.tools.has("engram_list_spaces"));
		const spaces = await h.tools.get("engram_list_spaces").execute("t3", {}, undefined, undefined, h.ctx);
		assert.equal(spaces.isError, false);
		assert.equal((spaces.structuredContent as any).spaces[0].name, "coding-agents");
	} finally {
		await h.emit("session_shutdown", { type: "session_shutdown", reason: "reload" });
	}
});

test("after a failed pass, automatic passes wait for more conversation", async () => {
	const h = await harness({ captureMinChars: 50 });
	try {
		await start(h);
		h.addMessage("user", "a conversation with something in it worth keeping for later on");
		h.completions.push(answer("FAIL_REMEMBER a memory the PDS keeps refusing to store."));
		await h.emit("agent_settled", { type: "agent_settled", aborted: false });
		await waitFor(() => h.notes.some((n) => /capturing failed/.test(n)));
		h.completions.push(answer("FAIL_REMEMBER a memory the PDS keeps refusing to store."));
		await h.emit("agent_settled", { type: "agent_settled", aborted: false });
		await new Promise((r) => setTimeout(r, 100));
		assert.equal(h.prompts.length, 1, "no second read of the same stretch");
		assert.equal(h.notes.filter((n) => /capturing failed/.test(n)).length, 1);

		// Enough new conversation tries again; /engram capture always does.
		h.addMessage("assistant", "y".repeat(100));
		await h.emit("agent_settled", { type: "agent_settled", aborted: false });
		await waitFor(() => h.prompts.length === 2);
	} finally {
		await h.emit("session_shutdown", { type: "session_shutdown", reason: "reload" });
	}
});

test("a pass that finishes after moving to another branch doesn't mark it read", async () => {
	const h = await harness({ captureMinChars: 10 });
	try {
		await start(h);
		h.addMessage("user", "branch A's conversation, long enough to capture from");
		let release!: () => void;
		const opened = new Promise<void>((r) => (release = r));
		h.completions.push(async () => {
			await opened;
			return { stopReason: "stop", content: [{ type: "text", text: answer("Branch A taught us the ingest job needs a backfill flag.") }] };
		});
		await h.emit("agent_settled", { type: "agent_settled", aborted: false });
		await waitFor(() => h.prompts.length === 1);
		// /tree to a sibling: a different path from the root.
		const a = h.branch.splice(0);
		h.addMessage("user", "branch B's own conversation");
		release();
		await waitFor(() => h.branch.some((e) => e.customType === "engram-capture"));
		const rec = h.branch.find((e) => e.customType === "engram-capture").data;
		assert.equal(rec.upTo, "", "not a read marker on branch B");
		assert.equal(rec.stored.length, 1);
		assert.ok(a.length);
	} finally {
		await h.emit("session_shutdown", { type: "session_shutdown", reason: "reload" });
	}
});

test("remember refuses credential-looking tags", async () => {
	const h = await harness();
	try {
		await start(h);
		await assert.rejects(
			h.tools.get("remember").execute("t", { text: "A fine memory.", tags: ["ghp_abcdefghijklmnopqrstuvwxyz0123456789"] }, undefined, undefined, h.ctx),
			/credential/,
		);
	} finally {
		await h.emit("session_shutdown", { type: "session_shutdown", reason: "reload" });
	}
});

test("queued capture requests merge: lowest threshold, any report", async () => {
	const h = await harness({ captureMinChars: 10_000, captureExitMinChars: 5_000 });
	try {
		await start(h);
		h.addMessage("user", "z".repeat(10_050));
		const g = gate();
		h.completions.push(g.fn);
		await h.emit("agent_settled", { type: "agent_settled", aborted: false });
		await waitFor(() => h.prompts.length === 1);
		h.addMessage("user", "a short tail the user wants captured now, please");
		await h.emit("agent_settled", { type: "agent_settled", aborted: false }); // 10k: wouldn't read it
		await h.emit("session_before_compact", { type: "session_before_compact" }); // 5k: wouldn't either
		const cmd = h.commands.get("engram").handler("capture", h.ctx); // 1, report
		g.release();
		await cmd;
		assert.equal(h.prompts.length, 2);
		assert.ok(h.notes.some((n) => /nothing worth keeping/.test(n)), "reported");
	} finally {
		await h.emit("session_shutdown", { type: "session_shutdown", reason: "reload" });
	}
});

test("an answer that isn't JSON doesn't mark the stretch read", async () => {
	const h = await harness({ captureMinChars: 10 });
	try {
		await start(h);
		h.addMessage("user", "something long enough to be read by capturing");
		h.completions.push("I found a couple of things worth noting: the build is slow.");
		await h.commands.get("engram").handler("capture", h.ctx);
		assert.equal(h.branch.filter((e) => e.customType === "engram-capture").length, 0);
		assert.ok(h.notes.some((n) => /wasn't the JSON/.test(n)));
	} finally {
		await h.emit("session_shutdown", { type: "session_shutdown", reason: "reload" });
	}
});

test("after compaction, recalled memories can be added again", async () => {
	const h = await harness({ recallMinSimilarity: 300 });
	try {
		await start(h);
		const [res] = await h.emit("before_agent_start", startEvent("how do attie deploys to pop1 work, which workflow?"));
		h.branch.push({ type: "custom_message", id: "r1", customType: "engram-recall", details: res.message.details });
		h.branch.push({ type: "compaction", id: "c1" });
		const [again] = await h.emit("before_agent_start", startEvent("and attie deploys to pop1 again, which workflow?"));
		assert.ok(again?.message);
	} finally {
		await h.emit("session_shutdown", { type: "session_shutdown", reason: "reload" });
	}
});

test("the first prompt waits for a slow engram-mcp start", async () => {
	const h = await harness({ recallMinSimilarity: 300, recallTimeoutMs: 500, env: { FAKE_MEMORIES: JSON.stringify(seeded), FAKE_SLOW_START: "1500" } });
	try {
		await h.emit("session_start", { type: "session_start" });
		const [res] = await h.emit("before_agent_start", startEvent("how do attie deploys to pop1 work, which workflow?"));
		assert.ok(res?.message, "recalled after the start, not given up at recallTimeoutMs");
	} finally {
		await h.emit("session_shutdown", { type: "session_shutdown", reason: "reload" });
	}
});

const seededSkills = [
	{
		name: "attie-deploy",
		description: "Deploy Attie to pop1.",
		instructions: "# Deploy\n\nDispatch the workflow.\n",
		files: [{ path: "scripts/check.sh", content: "#!/bin/sh\n", executable: true }],
		space: "attie",
		author: "did:plc:a",
		uri: "at://x/attie-deploy",
	},
];

test("skills from Engram are written where pi loads them, and edits there are saved", async () => {
	const h = await harness({ env: { FAKE_MEMORIES: "[]", FAKE_SKILLS: JSON.stringify(seededSkills) } });
	try {
		await h.emit("session_start", { type: "session_start" });
		const [found] = await h.emit("resources_discover", { type: "resources_discover", cwd: h.ctx.cwd, reason: "startup" });
		const dir = found.skillPaths[0];
		assert.match(dir, /pi-engram\/skills$/);
		const skillMd = join(dir, "attie-deploy", "SKILL.md");
		assert.match(readFileSync(skillMd, "utf8"), /^---\nname: attie-deploy\ndescription: "Deploy Attie to pop1."\n---\n\n# Deploy/);

		// The system prompt says where they are.
		const ev = startEvent("go");
		await h.emit("before_agent_start", ev);
		assert.ok(ev.systemPromptOptions.sections.memory.includes(dir));

		// The agent fixes a step with edit: it's saved, in the skill's own space.
		writeFileSync(skillMd, readFileSync(skillMd, "utf8").replace("Dispatch the workflow.", "Dispatch the workflow, then check Argo."));
		const [res] = await h.emit("tool_result", {
			type: "tool_result", toolName: "edit", toolCallId: "t", input: { path: skillMd }, content: [{ type: "text", text: "edited" }], isError: false,
		});
		assert.match(res.content.at(-1).text, /\[engram\] Saved skill attie-deploy/);
		const stored = await h.tools.get("engram_debug_skills").execute("d", {}, undefined, undefined, h.ctx);
		const s = (stored.structuredContent as any).skills.find((k: any) => k.name === "attie-deploy");
		assert.match(s.instructions, /check Argo/);
		assert.equal(s.space, "attie");
		assert.equal(s.files.length, 1, "files sent along");

		// A new skill written into the directory is added; a name that doesn't
		// match its directory is reported, not saved.
		const fresh = join(dir, "create-pr", "SKILL.md");
		mkdirSync(join(dir, "create-pr"));
		writeFileSync(fresh, "---\nname: create-pr\ndescription: Open a draft PR with gh.\n---\n\nUse gh.\n");
		const [added] = await h.emit("tool_result", { type: "tool_result", toolName: "write", toolCallId: "t2", input: { path: fresh }, content: [], isError: false });
		assert.match(added.content.at(-1).text, /Added skill create-pr/);
		writeFileSync(fresh, "---\nname: other\ndescription: d\n---\nx\n");
		const [bad] = await h.emit("tool_result", { type: "tool_result", toolName: "write", toolCallId: "t3", input: { path: fresh }, content: [], isError: false });
		assert.match(bad.content.at(-1).text, /wasn't saved to Engram: the frontmatter's name \(other\)/);

		// Edits elsewhere are left alone.
		const [other] = await h.emit("tool_result", { type: "tool_result", toolName: "edit", toolCallId: "t4", input: { path: join(h.ctx.cwd, "x.ts") }, content: [], isError: false });
		assert.equal(other, undefined);
	} finally {
		await h.emit("session_shutdown", { type: "session_shutdown", reason: "reload" });
	}
});

test("save_skill saves and refreshes the copy on disk", async () => {
	const h = await harness({ env: { FAKE_MEMORIES: "[]", FAKE_SKILLS: JSON.stringify(seededSkills) } });
	try {
		await h.emit("session_start", { type: "session_start" });
		const [found] = await h.emit("resources_discover", { type: "resources_discover", cwd: h.ctx.cwd, reason: "startup" });
		const out = await h.tools.get("save_skill").execute(
			"s", { name: "attie-deploy", description: "Deploy Attie.", instructions: "# Deploy v2", note: "rewrote it" }, undefined, undefined, h.ctx,
		);
		assert.match(out.content[0].text, /^Saved skill attie-deploy/);
		assert.match(readFileSync(join(found.skillPaths[0], "attie-deploy", "SKILL.md"), "utf8"), /# Deploy v2/);
		await assert.rejects(
			h.tools.get("save_skill").execute("s2", { name: "x", description: "d", instructions: "token=abcdefgh12345678" }, undefined, undefined, h.ctx),
			/credential/,
		);
		await assert.rejects(
			h.tools.get("save_skill").execute("s3", { name: "Bad", description: "d", instructions: "i" }, undefined, undefined, h.ctx),
			/lowercase/,
		);
		await assert.rejects(
			h.tools.get("save_skill").execute("s4", { name: "x", description: "d", instructions: "i", note: "token=abcdefgh12345678" }, undefined, undefined, h.ctx),
			/credential/,
		);
	} finally {
		await h.emit("session_shutdown", { type: "session_shutdown", reason: "reload" });
	}
});

test("an edit that can't be saved is kept on disk, and push saves it later", async () => {
	const h = await harness({ env: { FAKE_MEMORIES: "[]", FAKE_SKILLS: JSON.stringify(seededSkills) } });
	try {
		await h.emit("session_start", { type: "session_start" });
		const [found] = await h.emit("resources_discover", { type: "resources_discover", cwd: h.ctx.cwd, reason: "startup" });
		const md = join(found.skillPaths[0], "attie-deploy", "SKILL.md");
		writeFileSync(md, readFileSync(md, "utf8").replace("Dispatch the workflow.", "Dispatch it with token=abcdefgh12345678."));
		const [res] = await h.emit("tool_result", { type: "tool_result", toolName: "edit", toolCallId: "t", input: { path: md }, content: [], isError: false });
		assert.match(res.content.at(-1).text, /wasn't saved to Engram: it looks like it contains a credential/);

		// A new session syncs: the unsaved edit stays, with a warning.
		await h.emit("session_shutdown", { type: "session_shutdown", reason: "reload" });
		h.notes.length = 0;
		await h.emit("session_start", { type: "session_start" });
		await h.emit("resources_discover", { type: "resources_discover", cwd: h.ctx.cwd, reason: "startup" });
		await waitFor(() => h.notes.some((n) => /attie-deploy changed here but not in Engram/.test(n)));
		assert.match(readFileSync(md, "utf8"), /token=/);

		// Take the secret out by hand (not through edit), then push.
		writeFileSync(md, readFileSync(md, "utf8").replace(" with token=abcdefgh12345678", ", then check Argo"));
		await h.commands.get("engram").handler("skills push", h.ctx);
		assert.ok(h.notes.some((n) => /saved attie-deploy/.test(n)));
		await h.commands.get("engram").handler("skills", h.ctx);
		assert.ok(!h.notes.at(-1)!.includes("changed here, not saved"));
	} finally {
		await h.emit("session_shutdown", { type: "session_shutdown", reason: "reload" });
	}
});

test("skills changed by a shell command in the skills directory are saved", async () => {
	const h = await harness({ env: { FAKE_MEMORIES: "[]", FAKE_SKILLS: JSON.stringify(seededSkills) } });
	try {
		await h.emit("session_start", { type: "session_start" });
		const [found] = await h.emit("resources_discover", { type: "resources_discover", cwd: h.ctx.cwd, reason: "startup" });
		const md = join(found.skillPaths[0], "attie-deploy", "SKILL.md");
		writeFileSync(md, readFileSync(md, "utf8").replace("Dispatch the workflow.", "Dispatch it (sed edit)."));
		const [res] = await h.emit("tool_result", {
			type: "tool_result", toolName: "bash", toolCallId: "b", input: { command: `sed -i s/x/y/ ${md}` }, content: [],
			details: { exitCode: 0 }, structuredContent: { exitCode: 0 }, isError: false,
		});
		assert.match(res.content.at(-1).text, /\[engram\] Saved skill attie-deploy/);
		assert.deepEqual(res.details, { exitCode: 0 }, "details kept");
		assert.deepEqual(res.structuredContent, { exitCode: 0 }, "structured content kept");
		const [none] = await h.emit("tool_result", { type: "tool_result", toolName: "bash", toolCallId: "b2", input: { command: "ls" }, content: [], isError: false });
		assert.equal(none, undefined);
	} finally {
		await h.emit("session_shutdown", { type: "session_shutdown", reason: "reload" });
	}
});

test("an edit while a slow sync runs stays on disk, unsaved, and isn't marked saved", async () => {
	const h = await harness({ env: { FAKE_MEMORIES: "[]", FAKE_SKILLS: JSON.stringify(seededSkills) } });
	try {
		// A first session leaves a cache.
		await h.emit("session_start", { type: "session_start" });
		const [found] = await h.emit("resources_discover", { type: "resources_discover", cwd: h.ctx.cwd, reason: "startup" });
		await h.emit("session_shutdown", { type: "session_shutdown", reason: "reload" });
		const md = join(found.skillPaths[0], "attie-deploy", "SKILL.md");

		// The next one's sync is slow; an edit lands meanwhile.
		process.env.PI_ENGRAM_SYNC_WAIT_MS = "100";
		const settings = JSON.parse(readFileSync(join(process.env.PI_CODING_AGENT_DIR!, "engram.json"), "utf8"));
		settings.env.FAKE_SLOW_LIST = "1500";
		writeFileSync(join(process.env.PI_CODING_AGENT_DIR!, "engram.json"), JSON.stringify(settings));
		await h.emit("session_start", { type: "session_start" });
		writeFileSync(md, readFileSync(md, "utf8").replace("Dispatch the workflow.", "Dispatch it, mid-sync."));
		const [res] = await h.emit("tool_result", { type: "tool_result", toolName: "edit", toolCallId: "t", input: { path: md }, content: [], isError: false });
		assert.match(res.content.at(-1).text, /still being fetched/);
		await waitFor(() => h.notes.some((n) => /attie-deploy changed here but not in Engram/.test(n)), 8000);
		assert.match(readFileSync(md, "utf8"), /mid-sync/);
	} finally {
		delete process.env.PI_ENGRAM_SYNC_WAIT_MS;
		await h.emit("session_shutdown", { type: "session_shutdown", reason: "reload" });
	}
});
