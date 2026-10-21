import assert from "node:assert/strict";
import { test } from "node:test";
import { McpServer } from "../engram/mcp.ts";

const fake = new URL("./fake-engram-mcp.ts", import.meta.url).pathname;

test("calls tools and restarts after the server exits", async () => {
	const s = new McpServer({ command: process.execPath, args: [fake] });
	try {
		await s.start();
		assert.equal(s.serverVersion, "test");
		const tools = await s.listTools();
		assert.ok(tools.some((t) => t.name === "recall"));
		const out = await s.call<{ uri: string }>("remember", { text: "a memory about kittens" });
		assert.match(out.uri, /^at:\/\//);
		const found = await s.call<{ memories: any[] }>("recall", { query: "kittens" });
		assert.equal(found.memories.length, 1);
		await assert.rejects(s.call("get_memory", { uri: "nope" }), /no such memory/);

		// A crash costs the calls in flight, then the next call starts it again.
		(s as any).proc.kill("SIGKILL");
		await new Promise((r) => setTimeout(r, 100));
		assert.equal(s.running, false);
		const again = await s.call<{ memories: any[] }>("recall", { query: "kittens" });
		assert.equal(again.memories.length, 0); // a new process, a new store
	} finally {
		s.close();
	}
});

test("a call can be aborted", async () => {
	const s = new McpServer({ command: process.execPath, args: [fake] });
	try {
		await s.start();
		const ctrl = new AbortController();
		const p = s.call("recall", { query: "x" }, ctrl.signal);
		ctrl.abort();
		await assert.rejects(p, /aborted/);
		// The server still answers.
		await s.call("list_spaces", {});
	} finally {
		s.close();
	}
});

test("a failed start reports the server's stderr", async () => {
	const s = new McpServer({ command: process.execPath, args: [fake], env: { FAKE_FAIL_START: "1" } });
	try {
		await assert.rejects(s.start(), /run engram login/);
		assert.match(s.lastError ?? "", /run engram login/);
	} finally {
		s.close();
	}
});

test("a missing executable is reported", async () => {
	const s = new McpServer({ command: "/nonexistent/engram-mcp" });
	try {
		await assert.rejects(s.start(), /can't run \/nonexistent\/engram-mcp/);
	} finally {
		s.close();
	}
});

test("an aborted call doesn't wait for a slow start", async () => {
	const s = new McpServer({ command: process.execPath, args: [fake], env: { FAKE_SLOW_START: "3000" } });
	try {
		const t0 = Date.now();
		await assert.rejects(s.call("recall", { query: "x" }, AbortSignal.timeout(200)), /aborted/);
		assert.ok(Date.now() - t0 < 1500, "gave up on time");
	} finally {
		s.close();
	}
});

test("a call the server never answers times out, and close rejects calls in flight", async () => {
	const s = new McpServer({ command: process.execPath, args: [fake], requestTimeoutMs: 200 });
	try {
		await assert.rejects(s.call("debug_hang", {}), /didn't answer tools\/call within 0.2s/);
		await s.call("list_spaces", {}); // still usable
	} finally {
		s.close();
	}
	const t = new McpServer({ command: process.execPath, args: [fake] });
	await t.start();
	const p = t.call("debug_hang", {});
	await new Promise((r) => setTimeout(r, 50)); // in flight
	t.close();
	await assert.rejects(p, /stopped/);
});

test("an answer over the size limit stops the server", async () => {
	const s = new McpServer({ command: process.execPath, args: [fake], maxLine: 500 });
	try {
		await s.start();
		await s.call("remember", { text: "x".repeat(600) });
		await assert.rejects(s.call("list_memories", {}), /stopped/);
		assert.match(s.stderrTail.join("\n"), /too large/);
	} finally {
		s.close();
	}
});
