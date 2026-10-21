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
let n = store.length;

const words = (s: string) => new Set(s.toLowerCase().match(/[a-z0-9]+/g) ?? []);
function similarity(a: string, b: string): number {
	const x = words(a);
	const y = words(b);
	let both = 0;
	for (const w of x) if (y.has(w)) both++;
	return Math.round((1000 * both) / Math.max(1, Math.sqrt(x.size * y.size)));
}

function tool(name: string, args: any): unknown {
	switch (name) {
		case "list_spaces":
			return {
				spaces: [
					{ name: "coding-agents", uri: "at://did:plc:a/space/coding-agents", default: true, setUp: true, indexing: "granted" },
				],
			};
		case "recall": {
			const ms = store
				.map((m) => ({ ...m, similarity: similarity(args.query, m.text) }))
				.filter((m) => m.similarity > 0)
				.sort((a, b) => b.similarity - a.similarity)
				.slice(0, args.limit ?? 10);
			return { memories: ms, mode: args.mode ?? "hybrid" };
		}
		case "remember": {
			if (String(args.text).includes("FAIL_REMEMBER")) throw new Error("the PDS said no");
			const m: Mem = {
				space: "coding-agents",
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
			store.splice(i, 1);
			return { deleted: args.uri };
		}
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
					tools: ["list_spaces", "remember", "recall", "get_memory", "list_memories", "forget", "debug_hang"].map((name) => ({
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
