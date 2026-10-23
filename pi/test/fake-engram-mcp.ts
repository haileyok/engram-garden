// A stand-in for engram-mcp in tests: the same tools and result shapes,
// memories kept in memory, and similarity from word overlap (like
// embed.Hashing), so tests can make memories more or less alike.
//
// FAKE_MEMORIES (JSON) seeds the store. FAKE_FAIL_START=1 exits at once.
// The extra tool debug_hang never answers.

import { createInterface } from "node:readline";

if (process.env.FAKE_FAIL_START) {
	process.stderr.write("level=ERROR msg=exiting err=\"signing in: no account; run engram login\"\n");
	process.exit(1);
}

interface Mem {
	space: string;
	uri: string;
	author: string;
	text: string;
	tags: string[];
	source?: string;
	createdAt: string;
}

const store: Mem[] = JSON.parse(process.env.FAKE_MEMORIES ?? "[]");
const recalls: string[] = [];
// Skills by name; FAKE_SKILLS seeds them.
const skills = new Map<string, any>((JSON.parse(process.env.FAKE_SKILLS ?? "[]") as any[]).map((s) => [s.name, s]));
let n = store.length;

const words = (s: string) => new Set(s.toLowerCase().match(/[a-z0-9]+/g) ?? []);
function similarity(a: string, b: string): number {
	const x = words(a);
	const y = words(b);
	let both = 0;
	for (const w of x) if (y.has(w)) both++;
	return Math.round((1000 * both) / Math.max(1, Math.sqrt(x.size * y.size)));
}

// Sessions, kept the way the real tool keeps them: bytes of a transcript
// per session, appended to unless the caller says replace.
interface StoredSession {
	session: Record<string, any>;
	data: string;
	mode: string;
}
const sessions = new Map<string, StoredSession>(
	(JSON.parse(process.env.FAKE_SESSIONS ?? "[]") as any[]).map((s) => [s.session.sessionId, { session: s.session, data: s.data ?? "", mode: s.mode ?? "blob" }]),
);
const entriesOf = (data: string) => data.split("\n").filter(Boolean).length;

function tool(name: string, args: any): unknown {
	switch (name) {
		case "list_spaces":
			return {
				spaces: [
					{ name: "coding-agents", uri: "at://did:plc:a/space/coding-agents", default: true, setUp: true, indexing: "granted" },
					{ name: "attie", uri: "at://did:plc:a/space/attie", setUp: true, indexing: "granted" },
				],
			};
		case "debug_recalls":
			return { queries: recalls };
		case "recall": {
			recalls.push(args.query);
			const ms = store
				.filter((m) => !args.space || m.space === args.space)
				.map((m) => ({ ...m, similarity: similarity(args.query, m.text) }))
				.filter((m) => m.similarity > 0)
				.sort((a, b) => b.similarity - a.similarity)
				.slice(0, args.limit ?? 10);
			return { memories: ms, mode: args.mode ?? "hybrid" };
		}
		case "remember": {
			if (String(args.text).includes("FAIL_REMEMBER")) throw new Error("the PDS said no");
			const m: Mem = {
				space: args.space || "coding-agents",
				uri: `at://did:plc:a/space/coding-agents/did:plc:a/garden.engram.memory/${++n}`,
				author: "did:plc:a",
				text: args.text,
				tags: args.tags ?? [],
				source: args.source,
				createdAt: new Date().toISOString(),
			};
			store.push(m);
			return { uri: m.uri, cid: "bafy" };
		}
		case "get_memory": {
			const m = store.find((x) => x.uri === args.uri);
			if (!m) throw new Error("no such memory");
			return { memory: m };
		}
		case "list_memories":
			return { memories: [...store].reverse() };
		case "forget": {
			const i = store.findIndex((x) => x.uri === args.uri);
			if (i < 0) throw new Error("no such memory");
			if (store[i].author !== "did:plc:a") throw new Error(`that memory belongs to ${store[i].author}; you can only forget your own`);
			if (process.env.FAKE_FORGET_FAILS) throw new Error("the PDS is down");
			store.splice(i, 1);
			return { deleted: args.uri };
		}
		case "list_skills":
			if (process.env.FAKE_SLOW_LIST) {
				const until = Date.now() + Number(process.env.FAKE_SLOW_LIST);
				while (Date.now() < until) {} // the fake answers one request at a time anyway
			}
			return {
				skills: [...skills.values()].map((s) => (args.content ? s : { ...s, instructions: undefined, files: undefined })),
			};
		case "get_skill": {
			const s = skills.get(args.name);
			if (!s) throw new Error(`no skill named ${args.name}`);
			return { skill: s };
		}
		case "save_skill": {
			if (!/^[a-z0-9]+(-[a-z0-9]+)*$/.test(args.name)) throw new Error("bad name");
			const cur = skills.get(args.name);
			const s = {
				space: args.space || cur?.space || "coding-agents",
				author: "did:plc:a",
				uri: `at://did:plc:a/space/coding-agents/did:plc:a/garden.engram.skill/${args.name}`,
				name: args.name,
				description: args.description,
				instructions: args.instructions,
				files: args.files ?? cur?.files ?? [],
				disableModelInvocation: args.disableModelInvocation ?? cur?.disableModelInvocation ?? false,
				note: args.note,
			};
			skills.set(args.name, s);
			return { uri: s.uri, cid: "bafy", created: !cur, space: s.space };
		}
		case "debug_skills":
			return { skills: [...skills.values()] };
		case "list_sessions":
			return {
				sessions: [...sessions.values()].map((s) => ({
					...s.session,
					head: { bytes: s.data.length, entries: entriesOf(s.data) },
					space: "coding-agents",
				})),
			};
		case "push_session": {
			const id = args.session.sessionId;
			const cur = sessions.get(id);
			if (!args.replace && args.fromBytes !== (cur?.data.length ?? 0))
				return { session: id, mode: cur?.mode ?? "metadata", bytes: cur?.data.length ?? 0, entries: entriesOf(cur?.data ?? ""), storedBytes: 0, resync: true };
			const base = args.replace ? "" : (cur?.data ?? "");
			const data = base + String(args.data ?? "");
			// A trailing line without its newline is held back, like the real one.
			const cut = data.lastIndexOf("\n") + 1;
			const kept = data.slice(0, cut);
			const entry = { session: { ...args.session }, data: kept, mode: process.env.NO_BLOBS ? "record" : "blob" };
			sessions.set(id, entry);
			return {
				session: id,
				mode: entry.mode,
				bytes: kept.length,
				entries: entriesOf(kept),
				chunks: 1,
				storedBytes: kept.length - base.length,
				...(process.env.NO_BLOBS ? { note: "the transcript is in chunk records, not blobs" } : {}),
			};
		}
		case "pull_session": {
			const s = sessions.get(args.session);
			if (!s) throw new Error(`no session ${args.session} in any of your spaces`);
			return { session: { ...s.session, head: { bytes: s.data.length, entries: entriesOf(s.data) } }, data: s.data, space: "coding-agents" };
		}
		case "delete_session":
			return { deleted: `at://did:plc:a/garden.engram.session/${args.session}` };
		case "debug_sessions":
			return { sessions: [...sessions.values()] };
		default:
			throw new Error(`unknown tool ${name}`);
	}
}

const send = (msg: unknown) => process.stdout.write(`${JSON.stringify(msg)}\n`);

const slowStart = Number(process.env.FAKE_SLOW_START ?? 0);
const started = Date.now();

createInterface({ input: process.stdin }).on("line", async (line) => {
	if (slowStart) await new Promise((r) => setTimeout(r, Math.max(0, started + slowStart - Date.now())));
	const msg = JSON.parse(line);
	if (msg.id === undefined) return;
	switch (msg.method) {
		case "initialize":
			return send({
				jsonrpc: "2.0",
				id: msg.id,
				result: {
					protocolVersion: msg.params.protocolVersion,
					capabilities: { tools: {} },
					serverInfo: { name: "engram-garden", version: "test" },
					instructions: "fake",
				},
			});
		case "tools/list":
			return send({
				jsonrpc: "2.0",
				id: msg.id,
				result: {
					tools: ["list_spaces", "remember", "recall", "get_memory", "list_memories", "forget", "debug_hang", "list_skills", "get_skill", "save_skill", "debug_skills", "debug_recalls", "list_sessions", "push_session", "pull_session", "delete_session", "debug_sessions"].map((name) => ({
						name,
						description: name,
						inputSchema: { type: "object", properties: {} },
					})),
				},
			});
		case "tools/call": {
			if (msg.params.name === "debug_hang") return; // never answers
			let out: unknown;
			try {
				out = tool(msg.params.name, msg.params.arguments ?? {});
			} catch (err) {
				return send({
					jsonrpc: "2.0",
					id: msg.id,
					result: { content: [{ type: "text", text: (err as Error).message }], isError: true },
				});
			}
			return send({
				jsonrpc: "2.0",
				id: msg.id,
				result: { content: [{ type: "text", text: JSON.stringify(out) }], structuredContent: out },
			});
		}
		default:
			send({ jsonrpc: "2.0", id: msg.id, error: { code: -32601, message: "method not found" } });
	}
});
