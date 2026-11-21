// A stand-in for engram-mcp in tests: the same tools and result shapes,
// memories kept in memory, and similarity from word overlap (like
// embed.Hashing), so tests can make memories more or less alike.
//
// FAKE_MEMORIES (JSON) seeds the store. FAKE_FAIL_START=1 exits at once.
// The extra tool debug_hang never answers.
//
// With ENGRAM_IDENTITY (as pi-engram passes for an agent identity), the
// lounge tools are there too: list_agents answers FAKE_AGENTS, set_presence
// and send_message are recorded (debug_lounge returns them), and
// lounge_events long-polls events that debug_deliver queues.

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

/** inWindow is the since (inclusive) and until (exclusive) filter the appview applies. */
const inWindow = (m: Mem, args: any) =>
	(!args.since || m.createdAt >= args.since) && (!args.until || m.createdAt < args.until);

const lounge = !!process.env.ENGRAM_IDENTITY;
import { readFileSync } from "node:fs";
const self: string = lounge ? JSON.parse(readFileSync(process.env.ENGRAM_IDENTITY!, "utf8")).did : "";
const presences: any[] = [];
const sentMessages: any[] = [];
const events: any[] = [];
let seq = 0;
let wakeEvents: (() => void) | undefined;

// Sessions, kept the way the real tool keeps them: bytes of a transcript
// per session, appended to unless the caller says replace.
// Only an agent with an identity in an archive keeps sessions (FAKE_ARCHIVE
// stands in for the archive).
interface StoredSession {
	session: Record<string, any>;
	data: string;
}
const archive = !!process.env.FAKE_ARCHIVE;
const sessions = new Map<string, StoredSession>(
	(JSON.parse(process.env.FAKE_SESSIONS ?? "[]") as any[]).map((s) => [s.session.sessionId, { session: s.session, data: s.data ?? "" }]),
);
const entriesOf = (data: string) => data.split("\n").filter(Boolean).length;

function tool(name: string, args: any): unknown {
	if (!lounge && ["lounge_status", "list_agents", "send_message", "list_messages", "set_presence", "debug_lounge", "debug_deliver"].includes(name))
		throw new Error(`unknown tool ${name}`);
	switch (name) {
		case "lounge_status":
			return { did: self, lounge: "at://did:plc:p/space/garden.engram.lounge/agents", connection: "connected", identity: process.env.ENGRAM_IDENTITY };
		case "list_agents":
			return { self, agents: JSON.parse(process.env.FAKE_AGENTS ?? "[]") };
		case "set_presence":
			presences.push(args);
			return { did: self, online: true, ...args };
		case "send_message": {
			const uri = `at://did:plc:p/space/garden.engram.lounge/agents/${self}/garden.engram.message/${sentMessages.length + 1}`;
			sentMessages.push({ ...args, uri });
			return { uri, cid: "bafy" };
		}
		case "list_messages":
			return { messages: [] };
		case "debug_lounge":
			return { presences, sent: sentMessages, identity: process.env.ENGRAM_IDENTITY };
		case "debug_deliver": {
			for (const m of args.messages ?? []) events.push({ seq: ++seq, type: "message", message: m });
			wakeEvents?.();
			return { queued: seq };
		}
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
				.filter((m) => inWindow(m, args))
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
			return { memories: [...store].filter((m) => inWindow(m, args)).reverse() };
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
					author: "did:plc:a",
				})),
			};
		case "push_session": {
			const id = args.session.sessionId;
			const cur = sessions.get(id);
			if (cur && !args.replace && args.fromBytes !== cur.data.length)
				return { session: id, bytes: cur.data.length, entries: entriesOf(cur.data), storedBytes: 0, resync: true };
			const base = args.replace ? "" : (cur?.data ?? "");
			// A trailing line without its newline is held back, like the real one.
			const add = String(args.data ?? "");
			const complete = add.slice(0, add.lastIndexOf("\n") + 1);
			const kept = base + complete;
			sessions.set(id, { session: { ...args.session, kind: "session" }, data: kept });
			return { session: id, bytes: kept.length, entries: entriesOf(kept), chunks: 1, storedBytes: complete.length };
		}
		case "pull_session": {
			const s = sessions.get(args.session);
			if (!s) throw new Error(`no session ${args.session} in the archive`);
			return { ...s.session, head: { bytes: s.data.length, entries: entriesOf(s.data) }, author: "did:plc:a", data: s.data };
		}
		case "search_sessions":
			return {
				passages: [
					{
						session: "at://did:plc:p/space/garden.engram.archive/sessions/did:plc:bob/garden.engram.session/s9",
						author: "did:plc:bob",
						sessionId: "s9",
						kind: "error",
						entry: "e42",
						offset: 4096,
						text: `Failed: $ go test ./...\nFAIL TestModelChange\n\nThen: the fix for ${args.query ?? "it"}`,
						repo: "github.com/haileyok/engram-garden",
						agent: "session",
						createdAt: "2026-11-20T10:00:00.000Z",
					},
				],
				mode: "hybrid",
				note: "Passages are quoted from other sessions' transcripts (redacted)",
			};
		case "read_session":
			return {
				session: args.session,
				author: "did:plc:bob",
				title: "model change",
				entries: [
					{ id: "e41", role: "assistant", text: "→ $ go test ./..." },
					{ id: "e42", role: "error", text: "bash: FAIL TestModelChange", target: true },
				],
				moreBefore: true,
				note: "quoted",
			};
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
	if (msg.method === "tools/call" && msg.params.name === "lounge_events" && lounge) {
		const after = msg.params.arguments?.after ?? 0;
		const pending = () => events.filter((e) => e.seq > after);
		if (!pending().length)
			await new Promise<void>((r) => {
				wakeEvents = r;
				setTimeout(r, (msg.params.arguments?.wait ?? 0) * 1000);
			});
		const out = { events: pending(), next: seq, connection: "connected" };
		return send({ jsonrpc: "2.0", id: msg.id, result: { content: [{ type: "text", text: JSON.stringify(out) }], structuredContent: out } });
	}
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
					tools: [
						"list_spaces", "remember", "recall", "get_memory", "list_memories", "forget", "debug_hang", "list_skills", "get_skill", "save_skill", "debug_skills", "debug_recalls", "debug_sessions",
						...(lounge || archive ? ["list_sessions", "push_session", "pull_session", "delete_session", "search_sessions", "read_session"] : []),
						...(lounge ? ["lounge_status", "list_agents", "send_message", "list_messages", "set_presence", "lounge_events", "debug_lounge", "debug_deliver"] : []),
					].map((name) => ({
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
