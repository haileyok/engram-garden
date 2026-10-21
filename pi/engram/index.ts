/**
 * Engram for pi: long-term memory built into the agent rather than bolted
 * on as an MCP server the model has to remember to use.
 *
 * - engram-mcp (Go) still does the memory work: signing in, spaces,
 *   embedding, signing records. This extension runs it as a child process
 *   and talks MCP to it, so nothing about memories is reimplemented here.
 * - remember, recall, get_memory, list_memories and forget are ordinary
 *   tools, and the system prompt says what memory is for.
 * - Every prompt is searched for first, and the memories that are clearly
 *   relevant are added to the conversation (auto-recall).
 * - When the agent goes idle, compacts or the session ends, the stretch of
 *   conversation not read yet goes to a model that picks out what's worth
 *   keeping; near-duplicates of stored memories are dropped and the rest is
 *   remembered (auto-capture). A custom entry records how far each branch
 *   has been read, so nothing is read twice.
 */

import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { StringEnum } from "@earendil-works/pi-ai";
import type { ExtensionAPI, ExtensionContext, Theme } from "@earendil-works/pi-coding-agent";
import { convertToLlm, serializeConversation } from "@earendil-works/pi-coding-agent";
import { Box, Text } from "@earendil-works/pi-tui";
import { Type } from "typebox";
import {
	CAPTURE_ENTRY,
	type CaptureRecord,
	chunks,
	conversationMessages,
	knownMemories,
	looksSecret,
	memoryTags,
	parseCandidates,
	RECALL_MESSAGE,
	SYSTEM_PROMPT,
	sanitizeSource,
	uncaptured,
	userPrompt,
} from "./capture.ts";
import { type Config, loadConfig } from "./config.ts";
import {
	day,
	formatMemories,
	type ListSpacesOut,
	type MemoriesOut,
	type Memory,
	recallBlock,
	shorten,
	spacesSummary,
} from "./format.ts";
import { errorText, McpServer } from "./mcp.ts";

const run = promisify(execFile);

/** Tools this extension wraps itself; engram-mcp's others pass through. */
const WRAPPED = new Set(["remember", "recall", "get_memory", "list_memories", "forget"]);
const CHUNK_CHARS = 80_000;

interface RecallDetails {
	query: string;
	memories: Memory[];
	note?: string;
}

interface Project {
	name?: string;
	remote?: string;
}

/** The memory fields worth keeping in session entries. */
function slim(m: Memory): Memory {
	return {
		space: m.space,
		uri: m.uri,
		author: m.author,
		text: m.text,
		tags: m.tags ?? [],
		source: m.source,
		createdAt: m.createdAt,
		similarity: m.similarity,
	};
}

function text(s: string) {
	return [{ type: "text" as const, text: s }];
}

async function detectProject(cwd: string): Promise<Project> {
	const git = async (...args: string[]) => {
		try {
			return (await run("git", args, { cwd, timeout: 3000 })).stdout.trim();
		} catch {
			return "";
		}
	};
	const [common, remote] = await Promise.all([
		git("rev-parse", "--path-format=absolute", "--git-common-dir"),
		git("remote", "get-url", "origin"),
	]);
	// The common dir names the repository even from a worktree.
	let name = common ? common.replace(/\/\.git\/?$/, "").split("/").pop() : undefined;
	const fromRemote = remote.match(/[/:]([^/:]+?)(\.git)?$/)?.[1];
	if (fromRemote) name = fromRemote;
	return { name: name?.toLowerCase(), remote: remote ? sanitizeSource(remote) : undefined };
}

export default function engram(pi: ExtensionAPI) {
	let cfg: Config | undefined;
	let server: McpServer | undefined;
	let spaces: ListSpacesOut | undefined;
	let project: Project = {};
	let session = 0; // bumped per session, so late work from an old one is dropped
	let warnedDuplicate = false;
	let warnedModel = false;

	// Capturing runs one pass at a time. A request while one runs is queued
	// (merged with any other queued request) and runs next.
	let capturing: { id: number; done: Promise<void> } | undefined;
	let queued: { minChars: number; report: boolean } | undefined;
	// After a failed pass, automatic passes wait for more conversation or time.
	let failure: { chars: number; at: number } | undefined;
	const RETRY_AFTER_MS = 10 * 60_000;
	let captureAbort = new AbortController();

	const need = (): McpServer => {
		if (!server) throw new Error("Engram isn't running in this session");
		return server;
	};

	// ---- status line ----------------------------------------------------

	let activity = "";
	function status(ctx: ExtensionContext) {
		if (!ctx.hasUI) return;
		const th = ctx.ui.theme;
		let s: string;
		if (server?.lastError) s = th.fg("error", "engram ✗");
		else if (!spaces) s = th.fg("dim", "engram …");
		else {
			const def = spaces.spaces.find((x) => x.default);
			const lapsed = spaces.spaces.some((x) => x.setUp && x.indexing && x.indexing !== "granted");
			s = th.fg(lapsed ? "warning" : "dim", `engram ${def?.name ?? ""}`.trim());
		}
		if (activity) s += th.fg("dim", ` · ${activity}`);
		ctx.ui.setStatus("engram", s);
	}

	async function refreshSpaces(ctx: ExtensionContext) {
		const id = session;
		try {
			const out = await need().call<ListSpacesOut>("list_spaces", {});
			if (id !== session) return;
			spaces = out;
			for (const s of out.spaces) {
				if (s.setUp && s.indexing && s.indexing !== "granted" && ctx.hasUI)
					ctx.ui.notify(`engram: space ${s.name} isn't indexed (${s.indexing}); memories stored there can't be recalled`, "warning");
			}
		} catch (err) {
			if (id === session && ctx.hasUI) ctx.ui.notify(`engram: ${errorText(err)}`, "warning");
		}
		status(ctx);
	}

	// ---- engram-mcp's other tools (spaces, members, models) for codemode --

	const passedThrough = new Set<string>();
	async function registerOtherTools() {
		const tools = await need().listTools();
		for (const t of tools) {
			if (WRAPPED.has(t.name) || passedThrough.has(t.name)) continue;
			passedThrough.add(t.name);
			const name = t.name;
			pi.registerTool({
				name: `engram_${name}`,
				label: `engram ${name}`,
				description: t.description ?? name,
				parameters: Type.Unsafe<Record<string, unknown>>({
					...t.inputSchema,
					type: "object",
					properties: (t.inputSchema.properties as object) ?? {},
				}),
				exposure: "codemode",
				namespace: {
					name: "engram",
					description: "Engram memory spaces: list them, create one, manage members, models and indexing.",
				},
				annotations: t.annotations as any,
				async execute(_id, params, signal) {
					const res = await need().callTool(name, params, signal);
					const body = (res.content ?? []).map((c) => c.text ?? "").join("\n");
					return {
						content: text(body),
						details: undefined,
						structuredContent: res.structuredContent as any,
						isError: res.isError === true,
					};
				},
			});
		}
	}

	// ---- lifecycle --------------------------------------------------------

	pi.on("session_start", async (_event, ctx) => {
		session++;
		const id = session;
		spaces = undefined;
		failure = undefined;
		captureAbort = new AbortController();
		try {
			cfg = loadConfig(ctx.isProjectTrusted() ? ctx.cwd : undefined);
		} catch (err) {
			cfg = undefined;
			if (ctx.hasUI) ctx.ui.notify(`engram: bad settings, ${errorText(err)}`, "error");
			return;
		}
		server = new McpServer({ command: cfg.command, args: cfg.args, env: cfg.env });
		status(ctx);
		void detectProject(ctx.cwd).then((p) => {
			if (id === session) project = p;
		});
		void server
			.start()
			.then(() => Promise.all([refreshSpaces(ctx), registerOtherTools()]))
			.catch((err) => {
				if (id !== session) return;
				status(ctx);
				if (ctx.hasUI)
					ctx.ui.notify(`engram: ${errorText(err)}. Run \`engram status\` to check the setup, then /engram restart.`, "error");
			});
	});

	pi.on("session_shutdown", async (event, ctx) => {
		const s = server;
		if (cfg?.autoCapture && event.reason !== "reload" && captureAllowed(ctx)) {
			// Switching sessions keeps the person waiting more than quitting does,
			// and a session switched away from is read again when resumed.
			const limit = event.reason === "quit" ? cfg.captureExitTimeoutMs : Math.min(cfg.captureExitTimeoutMs, 8000);
			await Promise.race([
				(async () => {
					if (capturing?.id === session) await capturing.done;
					await capture(ctx, cfg!.captureExitMinChars);
				})().catch(() => {}),
				new Promise((r) => setTimeout(r, limit).unref()),
			]);
		}
		captureAbort.abort();
		session++;
		s?.close();
		if (server === s) server = undefined;
	});

	// ---- the system prompt and auto-recall ----------------------------------

	function memorySection(): string {
		const parts = [
			"Engram is your long-term memory: memory spaces shared with the person's other agents and your past sessions. Use it.",
		];
		if (spaces) parts.push(spacesSummary(spaces.spaces));
		if (cfg?.autoRecall)
			parts.push(
				"Memories that look relevant to each request are recalled automatically into a <memories> block. When those miss, or before work that may have history (a project, a bug, a deploy, the person's preferences), call recall yourself.",
			);
		else parts.push("Call recall before work that may have history: a project, a bug, a deploy, the person's preferences.");
		if (cfg?.autoCapture)
			parts.push(
				"The conversation is also read in the background for memories worth keeping, so don't store routine progress. Do call remember yourself, as soon as you learn them, for decisions and their reasons, the person's preferences and corrections, gotchas, and milestones such as merges and deploys: you know best what matters.",
			);
		else
			parts.push(
				"Call remember, as soon as you learn them, for decisions and their reasons, the person's preferences and corrections, gotchas, and milestones such as merges and deploys.",
			);
		parts.push("Everyone in a space can read what's stored there: never store secrets or credentials.");
		return parts.join("\n\n");
	}

	pi.on("before_agent_start", async (event, ctx) => {
		if (!cfg || !server) return;
		event.systemPromptOptions.sections.memory = memorySection();

		if (!warnedDuplicate && pi.getAllTools().some((t) => t.name.startsWith("mcp__engram__"))) {
			warnedDuplicate = true;
			if (ctx.hasUI)
				ctx.ui.notify(
					"engram: the engram MCP server is also configured; remove it from ~/.pi/agent/mcp.json, this extension replaces it",
					"warning",
				);
		}

		if (!cfg.autoRecall || event.messageType !== "user") return;
		const prompt = event.prompt.trim();
		if (prompt.length < cfg.recallMinPromptChars || prompt.startsWith("/")) return;

		// Memories already added on this branch, since the last compaction
		// (which summarizes them out of the model's view).
		const seen = new Set<string>();
		for (const e of ctx.sessionManager.getBranch() as any[]) {
			if (e.type === "compaction") seen.clear();
			const d = e.type === "custom_message" ? e.details : e.type === "message" ? e.message?.details : undefined;
			const isOurs =
				(e.type === "custom_message" && e.customType === RECALL_MESSAGE) ||
				(e.type === "message" && e.message?.role === "toolResult" && e.message.toolName === "recall");
			if (isOurs) for (const m of d?.memories ?? []) seen.add(m.uri);
		}

		activity = "recalling";
		status(ctx);
		let out: MemoriesOut;
		try {
			const signal = AbortSignal.any([AbortSignal.timeout(cfg.recallTimeoutMs), captureAbort.signal]);
			out = await server.call<MemoriesOut>(
				"recall",
				// Naming the project leans the search toward its memories.
				{ query: shorten(project.name ? `${project.name}: ${prompt}` : prompt, 1000), limit: Math.min(cfg.recallLimit * 2 + seen.size, 30) },
				signal,
			);
		} catch {
			return; // a slow or missing memory never holds up the prompt
		} finally {
			activity = "";
			status(ctx);
		}
		const best = Math.max(0, ...out.memories.map((m) => m.similarity ?? 0));
		const floor = Math.max(cfg.recallMinSimilarity, best - cfg.recallMaxGap);
		const picked = out.memories
			.filter((m) => (m.similarity ?? 0) >= floor && !seen.has(m.uri))
			.slice(0, cfg.recallLimit)
			.map(slim);
		if (!picked.length) return;
		return {
			message: {
				customType: RECALL_MESSAGE,
				content: recallBlock(picked, cfg.recallMaxChars),
				display: true,
				details: { query: prompt, memories: picked } satisfies RecallDetails,
			},
		};
	});

	pi.registerMessageRenderer<RecallDetails>(RECALL_MESSAGE, (message, { expanded, outputPad }, theme) => {
		const ms = message.details?.memories ?? [];
		const box = new Box(outputPad, 0, (t) => theme.bg("customMessageBg", t));
		const head = `${theme.fg("accent", "◆ engram")} ${theme.fg("muted", `recalled ${ms.length} ${ms.length === 1 ? "memory" : "memories"}`)}`;
		if (!expanded) {
			const first = ms[0] ? theme.fg("dim", ` — ${shorten(ms[0].text.replace(/\s+/g, " "), 80)}`) : "";
			box.addChild(new Text(head + first, 0, 0));
			return box;
		}
		box.addChild(new Text(head, 0, 0));
		for (const m of ms) box.addChild(new Text(memoryLines(m, theme, 400), 0, 0));
		return box;
	});

	// ---- auto-capture -----------------------------------------------------

	function captureAllowed(ctx: ExtensionContext): boolean {
		// Not for throwaway runs (pi -p --no-session, as subagents use): there's
		// no transcript to come back to, and nobody to wait for at exit.
		return (ctx.mode === "tui" || ctx.mode === "rpc") && !!ctx.sessionManager.getSessionFile();
	}

	function captureModel(ctx: ExtensionContext) {
		const want = cfg?.captureModel;
		if (want) {
			const slash = want.indexOf("/");
			const m = slash > 0 ? ctx.modelRegistry.find(want.slice(0, slash), want.slice(slash + 1)) : undefined;
			if (m) return m;
			if (ctx.hasUI && !warnedModel) ctx.ui.notify(`engram: captureModel ${want} not found, using the session's model`, "warning");
			warnedModel = true;
		}
		return ctx.model;
	}

	/**
	 * capture reads the conversation not captured yet on the current branch,
	 * if there's at least minChars of it, and remembers what's worth keeping.
	 * It returns once that's done, or once a pass already running is.
	 */
	function capture(ctx: ExtensionContext, minChars: number, report = false): Promise<void> {
		if (capturing?.id === session) {
			queued = {
				minChars: Math.min(minChars, queued?.minChars ?? minChars),
				report: report || (queued?.report ?? false),
			};
			return capturing.done;
		}
		const id = session;
		queued = { minChars, report };
		const done = (async () => {
			while (queued && id === session) {
				const next = queued;
				queued = undefined;
				try {
					await captureOnce(ctx, next.minChars, next.report);
				} catch (err) {
					// Say so once, not on every turn while it keeps failing.
					if (id === session && ctx.hasUI && (!failure || next.report))
						ctx.ui.notify(`engram: capturing failed, ${errorText(err)}`, "warning");
					if (id === session) failure = { chars: (err as any).chars ?? 0, at: Date.now() };
				}
			}
		})().finally(() => {
			if (capturing?.id === id) capturing = undefined;
			if (id === session) {
				activity = "";
				status(ctx);
			}
		});
		capturing = { id, done };
		return done;
	}

	async function captureOnce(ctx: ExtensionContext, minChars: number, report: boolean) {
		if (!cfg || !server) return;
		const id = session;
		const branch = ctx.sessionManager.getBranch() as any[];
		const span = uncaptured(branch);
		if (!span.length) return;
		const transcript = serializeConversation(convertToLlm(conversationMessages(span)));
		if (transcript.trim().length < minChars) {
			if (report && ctx.hasUI) ctx.ui.notify("engram: nothing new to capture", "info");
			return;
		}
		if (
			!report &&
			failure &&
			transcript.length < failure.chars + cfg.captureMinChars &&
			Date.now() - failure.at < RETRY_AFTER_MS
		)
			return;
		try {
			await extractAndStore(ctx, branch, span, transcript, id, report);
			failure = undefined;
		} catch (err) {
			throw Object.assign(err instanceof Error ? err : new Error(String(err)), { chars: transcript.length });
		}
	}

	async function extractAndStore(
		ctx: ExtensionContext,
		branch: any[],
		span: any[],
		transcript: string,
		id: number,
		report: boolean,
	) {
		if (!cfg || !server) return;
		const signal = captureAbort.signal;
		const model = captureModel(ctx);
		if (!model) throw new Error("no model to read the conversation with");

		activity = "capturing";
		status(ctx);
		const known = knownMemories(branch);
		const record: CaptureRecord = {
			upTo: span[span.length - 1].id,
			stored: [],
			skipped: 0,
			chars: transcript.length,
			model: `${model.provider}/${model.id}`,
			at: new Date().toISOString(),
		};
		try {
			// Each chunk is read, then what it held is stored, so later chunks
			// know about it. captureMax is per chunk: a long stretch isn't cut short.
			for (const piece of chunks(transcript, CHUNK_CHARS)) {
				const res = await ctx.modelRegistry.complete(
					model,
					{
						systemPrompt: SYSTEM_PROMPT,
						messages: [
							{
								role: "user",
								content: text(
									userPrompt(piece, {
										project: project.name,
										cwd: ctx.cwd,
										date: new Date().toISOString().slice(0, 10),
										known,
										max: cfg.captureMax,
									}),
								),
								timestamp: Date.now(),
							},
						],
					},
					{ maxTokens: 4096, signal, cacheRetention: "none" } as any,
				);
				// A cut-off answer isn't "nothing worth keeping": read the stretch again later.
				if (res.stopReason !== "stop")
					throw new Error(res.errorMessage || `the model stopped early (${res.stopReason})`);
				const answer = res.content
					.filter((c): c is { type: "text"; text: string } => c.type === "text")
					.map((c) => c.text)
					.join("\n");
				for (const c of parseCandidates(answer, cfg.captureMax)) {
					if (signal.aborted) throw new Error("aborted");
					const near = await server.call<MemoriesOut>("recall", { query: c.text, limit: 3, mode: "vector" }, signal);
					if (near.memories.some((m) => (m.similarity ?? 0) >= cfg!.captureDedupeSimilarity)) {
						record.skipped++;
						continue;
					}
					const args: Record<string, unknown> = {
						text: c.text,
						tags: memoryTags([...cfg.captureTags, ...(project.name ? [project.name] : []), ...c.tags]),
						source: project.remote && !looksSecret(project.remote) ? project.remote : ctx.cwd,
					};
					if (cfg.captureSpace) args.space = cfg.captureSpace;
					const out = await server.call<{ uri: string }>("remember", args, signal);
					record.stored.push({ uri: out.uri, text: c.text, tags: args.tags as string[] });
					known.push(c.text);
				}
			}
		} catch (err) {
			// Don't advance past a stretch only partly stored, but do record what
			// was: the next pass reads it again and sees those as already known.
			if (record.stored.length && id === session)
				pi.appendEntry<CaptureRecord>(CAPTURE_ENTRY, { ...record, upTo: "", error: errorText(err) });
			throw err;
		}
		if (id !== session) return;
		// The entry goes on the current leaf. If the person moved to another
		// branch meanwhile, the stretch read isn't on it: keep only what was
		// stored, as known there, and let the first branch be read again.
		const onBranch = (ctx.sessionManager.getBranch() as any[]).some((e) => e.id === record.upTo);
		if (!onBranch) {
			if (record.stored.length) pi.appendEntry<CaptureRecord>(CAPTURE_ENTRY, { ...record, upTo: "" });
			return;
		}
		pi.appendEntry<CaptureRecord>(CAPTURE_ENTRY, record);
		if (ctx.hasUI && (record.stored.length || report))
			ctx.ui.notify(
				record.stored.length
					? `engram: remembered ${record.stored.length} ${record.stored.length === 1 ? "memory" : "memories"} from this session (/engram review)`
					: "engram: nothing worth keeping in that stretch",
				"info",
			);
	}

	pi.on("agent_settled", async (event, ctx) => {
		if (!cfg?.autoCapture || event.aborted || !captureAllowed(ctx)) return;
		void capture(ctx, cfg.captureMinChars);
	});

	pi.on("session_before_compact", async (_event, ctx) => {
		// The compacted messages stay in the session file, so this is only a
		// natural point to catch up, not a race against losing them.
		if (!cfg?.autoCapture || !captureAllowed(ctx)) return;
		void capture(ctx, cfg.captureExitMinChars);
	});

	pi.registerEntryRenderer<CaptureRecord>(CAPTURE_ENTRY, (entry, { expanded }, theme) => {
		const r = entry.data;
		if (!r || (!r.stored.length && !expanded)) return undefined;
		const box = new Box(1, 0, (t) => theme.bg("customMessageBg", t));
		const n = r.stored.length;
		let head = `${theme.fg("accent", "◆ engram")} ${theme.fg("muted", `remembered ${n} ${n === 1 ? "memory" : "memories"}`)}`;
		if (r.skipped) head += theme.fg("dim", ` (${r.skipped} already known)`);
		if (!expanded) {
			const first = r.stored[0] ? theme.fg("dim", ` — ${shorten(r.stored[0].text.replace(/\s+/g, " "), 80)}`) : "";
			box.addChild(new Text(head + first, 0, 0));
			return box;
		}
		box.addChild(new Text(head + theme.fg("dim", ` from ${r.chars.toLocaleString()} chars with ${r.model}`), 0, 0));
		for (const m of r.stored) box.addChild(new Text(`${theme.fg("dim", "•")} ${m.text}\n  ${theme.fg("dim", m.uri)}`, 0, 0));
		return box;
	});

	// ---- tools ----------------------------------------------------------------

	function memoryLines(m: Memory, theme: Theme, max: number): string {
		const meta = [m.space, day(m.createdAt), m.similarity !== undefined ? `${m.similarity}` : "", m.tags?.join(", ")]
			.filter(Boolean)
			.join(" · ");
		return `${theme.fg("dim", "•")} ${shorten(m.text, max)}\n  ${theme.fg("dim", meta)}`;
	}

	const title = (theme: Theme, name: string, arg?: string) =>
		new Text(theme.fg("toolTitle", theme.bold(`${name} `)) + theme.fg("muted", arg ?? ""), 0, 0);

	function renderMemories(result: { details?: unknown; content: any[] }, expanded: boolean, theme: Theme) {
		const d = result.details as { memories?: Memory[]; note?: string } | undefined;
		if (!d?.memories) return new Text(theme.fg("dim", result.content?.[0]?.text ?? ""), 0, 0);
		const ms = d.memories;
		const lines = [theme.fg("muted", `${ms.length} ${ms.length === 1 ? "memory" : "memories"}`)];
		for (const m of expanded ? ms : ms.slice(0, 3)) lines.push(memoryLines(m, theme, expanded ? 2000 : 120));
		if (!expanded && ms.length > 3) lines.push(theme.fg("dim", `  … ${ms.length - 3} more`));
		if (d.note) lines.push(theme.fg("warning", d.note));
		return new Text(lines.join("\n"), 0, 0);
	}

	pi.registerTool({
		name: "recall",
		label: "Recall",
		description:
			"Search long-term memory: every agent's memories in all your Engram spaces (or one space), by meaning and exact words together. Describe what you're looking for and include identifiers, names, error messages or paths you know: they're matched exactly. Returns the best matches first, each with its space, date, similarity (0-1000), tags and uri; very long memories are shortened (get_memory has the full text).",
		promptSnippet: "Search long-term memory (decisions, preferences, gotchas, project history from earlier sessions)",
		parameters: Type.Object({
			query: Type.String({ description: "What to look for, in natural language, with any exact identifiers" }),
			limit: Type.Optional(Type.Integer({ minimum: 1, maximum: 50, description: "Maximum results (default 10)" })),
			tags: Type.Optional(Type.Array(Type.String(), { description: "Only memories carrying all of these tags" })),
			since: Type.Optional(Type.String({ description: "Only memories created at or after this RFC 3339 time" })),
			space: Type.Optional(Type.String({ description: "Search only this space, by name or URI" })),
			mode: Type.Optional(
				StringEnum(["hybrid", "vector", "keyword"] as const, {
					description: "hybrid (default): meaning and exact words; vector: meaning only; keyword: exact words only",
				}),
			),
		}),
		annotations: { readOnlyHint: true, openWorldHint: false },
		async execute(_id, params, signal) {
			const out = await need().call<MemoriesOut>("recall", params, signal);
			return {
				content: text(formatMemories(out, { maxChars: 2000 })),
				details: { query: params.query, memories: out.memories.map(slim), note: out.note } satisfies RecallDetails,
			};
		},
		renderCall: (args, theme) => title(theme, "recall", args.query),
		renderResult: (result, { expanded, isPartial }, theme) =>
			isPartial ? new Text(theme.fg("dim", "searching…"), 0, 0) : renderMemories(result, expanded, theme),
	});

	pi.registerTool({
		name: "remember",
		label: "Remember",
		description:
			"Store a memory in an Engram space (the default one unless you pass space). Agents in that space, including your future sessions, find it with recall within seconds. Write it to stand on its own later, without this conversation.",
		promptSnippet: "Store a durable memory for future sessions and other agents",
		promptGuidelines: [
			"Write each memory as a few sentences that stand alone: name the project, files, people and date rather than 'this' or 'the user'; add tags (the project, and a kind such as decision, preference, gotcha or status) and a source (URL, path, PR) when there is one.",
			"Update by remembering a new memory that says what changed; use forget only for your own memories that are wrong.",
		],
		parameters: Type.Object({
			text: Type.String({ description: "The memory, written to make sense on its own later" }),
			tags: Type.Optional(Type.Array(Type.String(), { description: "Short labels: project, topic, kind" })),
			source: Type.Optional(Type.String({ description: "Where it came from: a URL, file path, PR or task" })),
			space: Type.Optional(Type.String({ description: "The space to store it in, by name or URI" })),
		}),
		async execute(_id, params, signal) {
			const args = params.source ? { ...params, source: sanitizeSource(params.source) } : params;
			if ([args.text, args.source ?? "", ...(args.tags ?? [])].some(looksSecret))
				throw new Error("This looks like it contains a credential. Memories are readable by every agent in the space: leave the secret out.");
			const out = await need().call<{ uri: string; note?: string }>("remember", args, signal);
			return {
				content: text(`Remembered: ${out.uri}${out.note ? `\nNote: ${out.note}` : ""}`),
				details: { uri: out.uri, text: params.text, tags: params.tags ?? [], note: out.note },
			};
		},
		renderCall: (args, theme) => title(theme, "remember", shorten(args.text.replace(/\s+/g, " "), 100)),
		renderResult: (result, _o, theme) => {
			const d = result.details as { note?: string } | undefined;
			return new Text(
				d?.note ? theme.fg("warning", d.note) : theme.fg("success", "✓ remembered"),
				0,
				0,
			);
		},
	});

	pi.registerTool({
		name: "get_memory",
		label: "Get memory",
		description: "Fetch one memory, in full, by its uri.",
		parameters: Type.Object({ uri: Type.String({ description: "The memory's uri, from recall or remember" }) }),
		annotations: { readOnlyHint: true, openWorldHint: false },
		async execute(_id, params, signal) {
			const out = await need().call<{ memory: Memory }>("get_memory", params, signal);
			return { content: text(formatMemories({ memories: [out.memory] })), details: { memories: [slim(out.memory)] } };
		},
		renderCall: (args, theme) => title(theme, "get_memory", args.uri),
		renderResult: (result, { expanded }, theme) => renderMemories(result, expanded, theme),
	});

	pi.registerTool({
		name: "list_memories",
		label: "List memories",
		description:
			"List a space's memories newest first (the default space unless you pass space), optionally only an author's or those with given tags. Use recall to search; this is for browsing.",
		parameters: Type.Object({
			space: Type.Optional(Type.String({ description: "The space, by name or URI" })),
			tags: Type.Optional(Type.Array(Type.String(), { description: "Only memories carrying all of these tags" })),
			author: Type.Optional(Type.String({ description: "Only memories by this agent DID" })),
			limit: Type.Optional(Type.Integer({ minimum: 1, maximum: 100, description: "Maximum results (default 25)" })),
			cursor: Type.Optional(Type.String({ description: "Cursor from a previous call, for the next page" })),
		}),
		annotations: { readOnlyHint: true, openWorldHint: false },
		async execute(_id, params, signal) {
			const out = await need().call<MemoriesOut>("list_memories", params, signal);
			return {
				content: text(formatMemories(out, { maxChars: 600 })),
				details: { memories: out.memories.map(slim), note: out.note },
			};
		},
		renderCall: (args, theme) => title(theme, "list_memories", [args.space, args.tags?.join(",")].filter(Boolean).join(" ")),
		renderResult: (result, { expanded }, theme) => renderMemories(result, expanded, theme),
	});

	pi.registerTool({
		name: "forget",
		label: "Forget",
		description: "Delete one of your own memories by uri (you can't delete other agents' memories).",
		parameters: Type.Object({ uri: Type.String({ description: "URI of one of your own memories" }) }),
		annotations: { destructiveHint: true, idempotentHint: true, openWorldHint: false },
		async execute(_id, params, signal) {
			const out = await need().call<{ deleted: string }>("forget", params, signal);
			return { content: text(`Forgot ${out.deleted}`), details: undefined };
		},
		renderCall: (args, theme) => title(theme, "forget", args.uri),
	});

	// ---- /engram ------------------------------------------------------------

	const subcommands = [
		{ value: "status", label: "status", description: "Engram's state in this session" },
		{ value: "spaces", label: "spaces", description: "The memory spaces, their models and indexing" },
		{ value: "recall", label: "recall", description: "Search memories yourself: /engram recall <query>" },
		{ value: "capture", label: "capture", description: "Capture memories from the conversation now" },
		{ value: "review", label: "review", description: "Look over (and forget) memories captured in this session" },
		{ value: "restart", label: "restart", description: "Restart engram-mcp" },
	];

	async function showMemories(ctx: ExtensionContext, title: string, ms: Memory[], canForget: boolean) {
		if (!ms.length) return ctx.ui.notify(`engram: ${title}: none`, "info");
		while (true) {
			const labels = ms.map((m, i) => `${i + 1}. ${shorten(m.text.replace(/\s+/g, " "), 110)}`);
			const pick = await ctx.ui.select(title, labels);
			if (pick === undefined) return;
			const m = ms[labels.indexOf(pick)];
			const body = `${m.text}\n\n${[m.space, day(m.createdAt), m.tags?.join(", "), m.source].filter(Boolean).join(" · ")}\n${m.uri}`;
			if (!canForget) {
				await ctx.ui.editor(`Memory (${day(m.createdAt)})`, body);
				continue;
			}
			const action = await ctx.ui.select(shorten(m.text.replace(/\s+/g, " "), 200), ["Keep", "Read in full", "Forget"]);
			if (action === "Read in full") await ctx.ui.editor("Memory", body);
			if (action === "Forget") {
				try {
					await need().call("forget", { uri: m.uri });
					ms.splice(ms.indexOf(m), 1);
					ctx.ui.notify("engram: forgotten", "info");
					if (!ms.length) return;
				} catch (err) {
					ctx.ui.notify(`engram: ${errorText(err)}`, "error");
				}
			}
		}
	}

	pi.registerCommand("engram", {
		description: "Engram memory: status, spaces, recall, capture, review, restart",
		getArgumentCompletions: (prefix) => {
			const p = prefix.trim();
			if (p.includes(" ")) return null;
			return subcommands.filter((s) => s.value.startsWith(p));
		},
		handler: async (args, ctx) => {
			const [sub = "status", ...rest] = args.trim().split(/\s+/).filter(Boolean);
			const arg = rest.join(" ");
			switch (sub) {
				case "status": {
					const lines = [
						server?.running
							? `engram-mcp ${server.serverVersion} running (${cfg?.command})`
							: `engram-mcp not running${server?.lastError ? `: ${server.lastError}` : ""}`,
					];
					if (spaces) lines.push(spacesSummary(spaces.spaces));
					lines.push(
						`auto-recall: ${cfg?.autoRecall ? `on (≥${cfg.recallMinSimilarity}, up to ${cfg.recallLimit})` : "off"}`,
						`auto-capture: ${cfg?.autoCapture ? (captureAllowed(ctx) ? `on (${cfg.captureModel ?? "session model"})` : "off for this session (not saved, or print mode)") : "off"}${capturing ? ", capturing now" : ""}`,
					);
					const recs = (ctx.sessionManager.getBranch() as any[]).filter(
						(e) => e.type === "custom" && e.customType === CAPTURE_ENTRY,
					);
					const stored = recs.reduce((n, e) => n + (e.data?.stored?.length ?? 0), 0);
					if (recs.length) lines.push(`captured on this branch: ${stored} memories in ${recs.length} passes`);
					if (server?.lastError && server.stderrTail.length) lines.push("", ...server.stderrTail.slice(-8));
					ctx.ui.notify(lines.join("\n"), server?.lastError ? "error" : "info");
					return;
				}
				case "spaces": {
					await refreshSpaces(ctx);
					if (!spaces) return;
					const lines = spaces.spaces.map(
						(s) =>
							`${s.default ? "*" : " "} ${s.name}${s.setUp ? "" : " (not set up)"} — ${s.model?.name ?? "no model"}, indexing ${s.indexing ?? "?"}${s.memories !== undefined ? `, ${s.memories} memories` : ""}${s.warning ? `\n    ${s.warning}` : ""}`,
					);
					if (spaces.note) lines.push(spaces.note);
					ctx.ui.notify(lines.join("\n"), "info");
					return;
				}
				case "recall": {
					if (!arg) return ctx.ui.notify("usage: /engram recall <query>", "warning");
					try {
						const out = await need().call<MemoriesOut>("recall", { query: arg, limit: 15 });
						await showMemories(ctx, `recall: ${shorten(arg, 60)}`, out.memories, false);
					} catch (err) {
						ctx.ui.notify(`engram: ${errorText(err)}`, "error");
					}
					return;
				}
				case "capture": {
					if (!cfg) return;
					if (!captureAllowed(ctx))
						return ctx.ui.notify("engram: only saved interactive (or RPC) sessions are captured", "warning");
					await capture(ctx, 1, true);
					return;
				}
				case "review": {
					const ms: Memory[] = [];
					for (const e of ctx.sessionManager.getBranch() as any[]) {
						if (e.type !== "custom" || e.customType !== CAPTURE_ENTRY) continue;
						const r = e.data as CaptureRecord;
						for (const m of r.stored)
							ms.push({ uri: m.uri, text: m.text, tags: m.tags, author: "", createdAt: r.at });
					}
					await showMemories(ctx, "Memories captured in this session", ms, true);
					return;
				}
				case "restart": {
					if (!cfg) return;
					server?.close();
					server = new McpServer({ command: cfg.command, args: cfg.args, env: cfg.env });
					spaces = undefined;
					status(ctx);
					try {
						await server.start();
						await Promise.all([refreshSpaces(ctx), registerOtherTools()]);
						ctx.ui.notify(`engram: engram-mcp ${server.serverVersion} running`, "info");
					} catch (err) {
						status(ctx);
						ctx.ui.notify(`engram: ${errorText(err)}`, "error");
					}
					return;
				}
				default:
					ctx.ui.notify(`engram: unknown command ${sub}; try ${subcommands.map((s) => s.value).join(", ")}`, "warning");
			}
		},
	});
}
