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
 * - The session's own transcript, and where the work stands, are kept in
 *   the space too (session sync), so the same session can be resumed on
 *   another machine: the manifest is a garden.engram.session record and the
 *   bytes go up as blobs. A custom entry records how much of the file is
 *   stored.
 */

import { execFile } from "node:child_process";
import { appendFileSync, existsSync, mkdirSync, statSync } from "node:fs";
import { homedir } from "node:os";
import { join, resolve } from "node:path";
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
	agentDir,
	HARNESS,
	lastSync,
	type Manifest,
	machine,
	SESSION_SYNC_ENTRY,
	type SyncRecord,
	gitInfo,
	pushSession,
	startedAtOf,
	syncedBytes,
	summaryOf,
	titleOf,
	writePulled,
} from "./sessionsync.ts";
import {
	day,
	formatMemories,
	humanBytes,
	type ListSpacesOut,
	type MemoriesOut,
	type Memory,
	recallBlock,
	shorten,
	spacesSummary,
} from "./format.ts";
import {
	CONSOLIDATE_PROMPT,
	consolidatePrompt,
	type Decision,
	FILTER_PROMPT,
	filterPrompt,
	grounded,
	keepsFacts,
	lastExchange,
	parseDecision,
	parseFilter,
	sameMemory,
	ungrounded,
} from "./judge.ts";
import { errorText, McpServer } from "./mcp.ts";
import {
	cachedSkills,
	dirHash,
	dirtySkills,
	markSaved,
	readSkillDir,
	replaceSkill,
	type Skill,
	skillDirOf,
	skillsCacheDir,
	validSkillName,
	writeSkills,
} from "./skills.ts";

const run = promisify(execFile);

/** Tools this extension wraps itself; engram-mcp's others pass through. */
const WRAPPED = new Set(["remember", "recall", "get_memory", "list_memories", "forget", "save_skill"]);
const CHUNK_CHARS = 80_000;

interface RecallDetails {
	query: string;
	memories: Memory[];
	note?: string;
	/** Whether the model picked these, and from how many. */
	filtered?: boolean;
	considered?: number;
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
	let projectReady: Promise<void> = Promise.resolve();
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

	// Session sync: one push at a time, and not more often than the settings
	// say unless the session is ending.
	let sessionPush: Promise<void> | undefined;
	let lastPushAt = 0;
	let warnedSync = false;

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

	// ---- skills -------------------------------------------------------------

	const skillsDir = skillsCacheDir();
	let skillsSynced: Promise<void> = Promise.resolve();
	let syncing = false;

	/**
	 * syncSettled waits a while for a sync that's running. An edit made while
	 * one still runs stays on disk unsaved (a sync never overwrites it), for
	 * the next edit or /engram skills push.
	 */
	async function syncSettled(): Promise<boolean> {
		const wait = Number(process.env.PI_ENGRAM_SYNC_WAIT_MS) || 10_000; // tests shorten it
		await Promise.race([skillsSynced, new Promise((r) => setTimeout(r, wait).unref())]);
		return !syncing;
	}
	const stillSyncing = "[engram] Skills are still being fetched from Engram, so this change wasn't saved yet; it stays on disk, and /engram skills push saves it.";

	/** syncSkills writes the spaces' skills into the cache pi loads them from. */
	async function syncSkills(ctx: ExtensionContext, hadCache: boolean, report = false) {
		const id = session;
		const out = await need().call<{ skills: Skill[]; note?: string }>("list_skills", { content: true });
		if (id !== session) return;
		// Settings order is precedence: the first space with a name wins.
		const { changed, kept } = writeSkills(skillsDir, out.skills);
		if (!ctx.hasUI) return;
		if (changed && hadCache)
			ctx.ui.notify(`engram: skills changed in Engram (${out.skills.length} in all); /reload to load them`, "info");
		else if (report)
			ctx.ui.notify(`engram: ${out.skills.length} skills, ${changed ? "updated" : "already up to date"}${out.note ? ` (${out.note})` : ""}`, "info");
		if (kept.length)
			ctx.ui.notify(
				`engram: ${kept.join(", ")} changed here but not in Engram, so left as is; /engram skills push saves them`,
				"warning",
			);
	}

	pi.on("resources_discover", async () => {
		// The first time, there's no cache yet: wait a little for it.
		if (!existsSync(skillsDir))
			await Promise.race([skillsSynced, new Promise((r) => setTimeout(r, 8000).unref())]);
		return existsSync(skillsDir) ? { skillPaths: [skillsDir] } : undefined;
	});

	/** saveAfterBash saves skills a shell command in the skills directory changed. */
	async function saveAfterBash(event: { input: Record<string, unknown>; content: any[]; details?: unknown; structuredContent?: any }) {
		const cmd = String(event.input.command ?? "");
		if (!cmd.includes(skillsDir) && !cmd.includes("pi-engram/skills")) return;
		if (!(await syncSettled())) return dirtySkills(skillsDir).length ? appended(event, stillSyncing) : undefined;
		const lines: string[] = [];
		for (const name of dirtySkills(skillsDir)) {
			try {
				const out = await saveSkillDir(join(skillsDir, name));
				lines.push(`[engram] ${out.created ? "Added" : "Saved"} skill ${name} for every agent in its space (${out.uri}).`);
			} catch (err) {
				lines.push(`[engram] Skill ${name} changed, but wasn't saved to Engram: ${errorText(err)}`);
			}
		}
		if (!lines.length) return;
		return appended(event, lines.join("\n"));
	}

	/** appended is a tool result with a line added, keeping its details and structured content. */
	function appended(event: { content: any[]; details?: unknown; structuredContent?: any }, line: string) {
		return {
			content: [...event.content, { type: "text" as const, text: `\n${line}` }],
			details: event.details,
			structuredContent: event.structuredContent,
		};
	}

	/** saveSkillDir saves a skill directory in the cache to Engram. */
	async function saveSkillDir(dir: string, note?: string) {
		const before = dirHash(dir);
		const s = readSkillDir(dir);
		const name = dir.split("/").pop()!;
		if (s.name !== name) throw new Error(`the frontmatter's name (${s.name || "none"}) must be the directory's name, ${name}`);
		if ([s.description, s.instructions, ...(s.files ?? []).map((f) => f.content)].some(looksSecret))
			throw new Error("it looks like it contains a credential, and every agent in the space reads skills: take the secret out");
		const known = cachedSkills(skillsDir).find((k) => k.name === name);
		const args: Record<string, unknown> = {
			name,
			description: s.description,
			instructions: s.instructions,
			files: s.files ?? [],
			disableModelInvocation: !!s.disableModelInvocation,
		};
		if (note) args.note = note;
		if (known?.space) args.space = known.space;
		const out = await need().call<{ uri: string; created: boolean; note?: string; space?: string }>("save_skill", args);
		markSaved(skillsDir, name, { space: out.space, uri: out.uri }, before);
		return out;
	}

	// Edits to the cached skills are edits to the shared ones.
	pi.on("tool_result", async (event, ctx) => {
		if (event.toolName === "bash" && !event.isError && server) return saveAfterBash(event);
		if ((event.toolName !== "edit" && event.toolName !== "write") || event.isError || !server) return;
		const raw = String(event.input.path ?? event.input.file_path ?? "");
		if (!raw) return;
		const abs = resolve(ctx.cwd, raw.startsWith("~/") ? join(homedir(), raw.slice(2)) : raw);
		const dir = skillDirOf(abs, skillsDir);
		if (!dir || !existsSync(join(dir, "SKILL.md"))) return;
		// Let a sync that's running finish first; it leaves edited skills alone.
		if (!(await syncSettled())) return appended(event, stillSyncing);
		let line: string;
		try {
			const out = await saveSkillDir(dir);
			line = `[engram] ${out.created ? "Added" : "Saved"} skill ${dir.split("/").pop()} for every agent in its space (${out.uri}).${out.note ? ` ${out.note}` : ""}`;
		} catch (err) {
			line = `[engram] The file changed, but the skill wasn't saved to Engram: ${errorText(err)}`;
		}
		return appended(event, line);
	});

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
		projectReady = detectProject(ctx.cwd).then((p) => {
			if (id === session) project = p;
		});
		const hadCache = existsSync(skillsDir);
		syncing = true;
		skillsSynced = server
			.start()
			.then(() => syncSkills(ctx, hadCache))
			.catch((err) => {
				// A failed start is reported below; anything else here.
				if (id === session && ctx.hasUI && server?.handshakeDone)
					ctx.ui.notify(`engram: skills not ${hadCache ? "updated" : "loaded"}: ${errorText(err)}`, "warning");
			})
			.finally(() => {
				if (id === session) syncing = false;
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
		if (event.reason !== "reload" && captureAllowed(ctx)) {
			// The last push carries everything this session added.
			await Promise.race([
				syncSession(ctx, { final: true }),
				new Promise((r) => setTimeout(r, event.reason === "quit" ? 15_000 : 5_000).unref()),
			]);
		}
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

	// ---- session sync --------------------------------------------------------

	/** StoredSession is a session as list_sessions or pull_session returns it. */
	interface StoredSession {
		sessionId: string;
		title?: string;
		summary?: string;
		status?: string;
		cwd?: string;
		machine?: string;
		branch?: string;
		gitHead?: string;
		gitDirty?: boolean;
		updatedAt?: string;
		head?: { bytes?: number; entries?: number };
		space?: string;
	}

	/**
	 * putSession writes a pulled transcript where pi looks for sessions: its
	 * own working directory's folder when that directory is here, else this
	 * one's, so /resume shows it either way.
	 */
	function putSession(ctx: ExtensionContext, session: StoredSession, data: string): string {
		const home = !session.cwd || existsSync(session.cwd) === false;
		const dir = home ? ctx.sessionManager.getSessionDir() : join(agentDir(), "sessions");
		const cwd = home ? ctx.cwd : (session.cwd ?? ctx.cwd);
		return writePulled(dir, cwd, session.sessionId, data, session.updatedAt);
	}


	/** syncSession stores what the session file grew by, and where the work stands. */
	async function syncSession(ctx: ExtensionContext, opts: { final?: boolean } = {}): Promise<void> {
		if (!cfg?.autoSessionSync || !server || !captureAllowed(ctx)) return;
		const file = ctx.sessionManager.getSessionFile();
		const id = ctx.sessionManager.getSessionId();
		if (!file || !id) return;
		// One push at a time; the next one carries what this one missed.
		if (sessionPush) {
			if (!opts.final) return;
			await sessionPush.catch(() => {});
		}
		const run = (async () => {
			const entries = ctx.sessionManager.getEntries() as any[];
			const from = syncedBytes(entries);
			const branch = ctx.sessionManager.getBranch() as any[];
			const git = await gitInfo(ctx.cwd).catch(() => ({}));
			const model = ctx.model ? `${ctx.model.provider}/${ctx.model.id}` : undefined;
			const manifest: Manifest = {
				sessionId: id,
				title: titleOf(branch, ctx.sessionManager.getSessionName()),
				summary: summaryOf(branch),
				status: opts.final ? "ended" : "active",
				cwd: ctx.cwd,
				machine: machine(),
				harness: HARNESS,
				startedAt: startedAtOf(ctx.sessionManager.getHeader() as any, entries),
				...(model ? { model } : {}),
				...git,
			};
			const { record, answer } = await pushSession({
				call: (tool, args) => need().call(tool, args),
				file,
				session: manifest,
				from,
			});
			pi.appendEntry<SyncRecord>(SESSION_SYNC_ENTRY, record);
			lastPushAt = Date.now();
			if (answer.note && !warnedSync && ctx.hasUI) {
				warnedSync = true;
				ctx.ui.notify(`engram: sessions aren't fully stored: ${answer.note}`, "warning");
			}
		})();
		const push: Promise<void> = run
			.catch((err) => {
				// Never in the way: a failed push is retried at the next turn end.
				lastPushAt = Date.now();
				if (opts.final && ctx.hasUI) ctx.ui.notify(`engram: session not stored: ${errorText(err)}`, "warning");
			})
			.finally(() => {
				if (sessionPush === push) sessionPush = undefined;
			});
		sessionPush = push;
		await push;
	}

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
		parts.push(
			`Skills kept in Engram are in ${skillsDir} and listed with your other skills. Every agent in their space uses them, and you should improve them: when a skill's steps turned out wrong, out of date or incomplete, fix its SKILL.md (or files) there with edit or write, and the change is saved to Engram. To make a new shared skill, write ${skillsDir}/<name>/SKILL.md (frontmatter with name and description, then instructions), or call save_skill.`,
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

		// A short follow-up ("ok, do that") is searched for with what it follows.
		const branch = ctx.sessionManager.getBranch() as any[];
		const context = lastExchange(branch);
		const query = prompt.length < 200 && context ? `${prompt}\n\n${context}` : prompt;

		activity = "recalling";
		status(ctx);
		let out: MemoriesOut;
		try {
			// engram-mcp takes a few seconds to sign in and read the spaces. The
			// first prompt is when memories help most, so it waits for that,
			// unless starting has already failed.
			if (!server.handshakeDone && !server.lastError)
				await Promise.race([
					server.start(),
					new Promise((_, reject) =>
						setTimeout(() => reject(new Error("slow start")), cfg!.recallStartTimeoutMs).unref(),
					),
				]);
			const signal = AbortSignal.any([AbortSignal.timeout(cfg.recallTimeoutMs), captureAbort.signal]);
			out = await server.call<MemoriesOut>(
				"recall",
				{ query: shorten(query, 1600), limit: Math.min(cfg.recallCandidates + seen.size, 30) },
				signal,
			);
		} catch {
			activity = "";
			status(ctx);
			return; // a slow or missing memory never holds up the prompt
		}
		const candidates = out.memories
			.filter((m) => (m.similarity ?? 0) >= cfg!.recallCandidateSimilarity && !seen.has(m.uri))
			.slice(0, cfg.recallCandidates)
			.map(slim);
		let picked: Memory[] = [];
		let filtered = false;
		// One or two clearly close results need no second opinion.
		const obvious = candidates.length > 0 && candidates.length <= 2 && candidates.every((m) => (m.similarity ?? 0) >= 750);
		if (obvious) {
			picked = candidates;
			filtered = true;
		} else if (candidates.length && cfg.recallFilter && memoryModel(ctx)) {
			// The model picks what bears on the prompt, and the part of each that does.
			activity = "choosing memories";
			status(ctx);
			try {
				const signal = AbortSignal.any([AbortSignal.timeout(cfg.recallFilterTimeoutMs), captureAbort.signal]);
				const numbered = candidates.map((m, i) => ({ id: i, text: m.text, createdAt: m.createdAt, space: m.space }));
				const answer = await ask(ctx, FILTER_PROMPT, filterPrompt(prompt, context, numbered), signal, 1500);
				picked = parseFilter(answer, numbered.map((m) => m.id))
					.slice(0, cfg.recallLimit)
							// A short memory is shown whole. A long one shows the extract, unless the
					// extract names something the memory doesn't.
					.map((k) => {
						const c = candidates[k.id];
						const useExtract = k.relevant && c.text.length > cfg!.recallMaxChars && grounded(k.relevant, c.text);
						return { ...c, relevant: useExtract ? k.relevant : undefined };
					});
				filtered = true;
			} catch {
				filtered = false;
			}
		}
		activity = "";
		status(ctx);
		if (!filtered) {
			// Similarity alone: only the clearly close ones.
			const best = Math.max(0, ...candidates.map((m) => m.similarity ?? 0));
			const floor = Math.max(cfg.recallMinSimilarity, best - cfg.recallMaxGap);
			picked = candidates.filter((m) => (m.similarity ?? 0) >= floor).slice(0, cfg.recallLimit);
		}
		if (!picked.length) return;
		return {
			message: {
				customType: RECALL_MESSAGE,
				content: recallBlock(picked, cfg.recallMaxChars, filtered),
				display: true,
				details: { query: prompt, memories: picked, filtered, considered: candidates.length } satisfies RecallDetails,
			},
		};
	});

	pi.registerMessageRenderer<RecallDetails>(RECALL_MESSAGE, (message, { expanded, outputPad }, theme) => {
		const d = message.details;
		const ms = d?.memories ?? [];
		const box = new Box(outputPad, 0, (t) => theme.bg("customMessageBg", t));
		const of = d?.filtered && d.considered ? ` (picked from ${d.considered})` : "";
		const head = `${theme.fg("accent", "◆ engram")} ${theme.fg("muted", `recalled ${ms.length} ${ms.length === 1 ? "memory" : "memories"}${of}`)}`;
		if (!expanded) {
			const first = ms[0] ? theme.fg("dim", ` — ${shorten((ms[0].relevant ?? ms[0].text).replace(/\s+/g, " "), 80)}`) : "";
			box.addChild(new Text(head + first, 0, 0));
			return box;
		}
		box.addChild(new Text(head, 0, 0));
		for (const m of ms) box.addChild(new Text(memoryLines({ ...m, text: m.relevant ?? m.text }, theme, 400), 0, 0));
		return box;
	});

	// ---- auto-capture -----------------------------------------------------

	function captureAllowed(ctx: ExtensionContext): boolean {
		// Not for throwaway runs (pi -p --no-session, as subagents use): there's
		// no transcript to come back to, and nobody to wait for at exit.
		return (ctx.mode === "tui" || ctx.mode === "rpc") && !!ctx.sessionManager.getSessionFile();
	}

	/** memoryModel is the model for the memory work (cfg.model, else the session's). */
	function memoryModel(ctx: ExtensionContext) {
		const want = cfg?.model;
		if (want) {
			const slash = want.indexOf("/");
			const m = slash > 0 ? ctx.modelRegistry.find(want.slice(0, slash), want.slice(slash + 1)) : undefined;
			if (m) return m;
			if (ctx.hasUI && !warnedModel) ctx.ui.notify(`engram: model ${want} not found, using the session's model`, "warning");
			warnedModel = true;
		}
		return ctx.model;
	}

	/**
	 * ask puts one question to the memory model, with no reasoning (pi sends
	 * the model's "off" level when none is asked for), and returns its answer.
	 */
	async function ask(
		ctx: ExtensionContext,
		system: string,
		user: string,
		signal: AbortSignal,
		maxTokens = 2048,
		reasoning?: "low",
	): Promise<string> {
		const model = memoryModel(ctx);
		if (!model) throw new Error("no model for the memory work");
		const res = await ctx.modelRegistry.complete(
			model,
			{ systemPrompt: system, messages: [{ role: "user", content: text(user), timestamp: Date.now() }] },
			{ maxTokens, signal, cacheRetention: "none", ...(reasoning ? { reasoning } : {}) } as any,
		);
		if (res.stopReason !== "stop") throw new Error(res.errorMessage || `the model stopped early (${res.stopReason})`);
		return res.content
			.filter((c): c is { type: "text"; text: string } => c.type === "text")
			.map((c) => c.text)
			.join("\n");
	}

	interface StoreIn {
		text: string;
		tags: string[];
		source?: string;
		space?: string;
	}
	type Stored =
		| {
				action: "stored";
				uri: string;
				text: string;
				tags: string[];
				replaced: string[];
				kept: string[];
				problems: string[];
				note?: string;
				/** Why it was stored as written, when a comparison could have merged or skipped it. */
				why?: string;
		  }
		| { action: "skipped"; existing: Memory[] };

	/**
	 * targetSpace names the space a memory goes to: the one given (by name or
	 * URI), else the default. Results are labeled by name, so a URI is turned
	 * into its name. Undefined when it can't be told.
	 */
	async function targetSpace(given: string | undefined, signal: AbortSignal): Promise<string | undefined> {
		if (!spaces) {
			try {
				spaces = await need().call<ListSpacesOut>("list_spaces", {}, signal);
			} catch {
				return given && !given.startsWith("at://") ? given : undefined;
			}
		}
		if (!given) return spaces.spaces.find((x) => x.default)?.name;
		return spaces.spaces.find((x) => x.name === given || x.uri === given)?.name ?? (given.startsWith("at://") ? undefined : given);
	}

	/** keepForgotten writes a memory about to be forgotten to a local log, so a bad merge can be undone. */
	function keepForgotten(m: Memory, mergedInto: string) {
		try {
			const dir = join(process.env.XDG_STATE_HOME || join(homedir(), ".local", "state"), "pi-engram");
			mkdirSync(dir, { recursive: true });
			appendFileSync(join(dir, "forgotten.jsonl"), `${JSON.stringify({ at: new Date().toISOString(), mergedInto, memory: m })}\n`);
		} catch {}
	}

	/**
	 * storeConsolidated stores a memory unless one already says it, and merges
	 * it with your own memories it updates (forgetting those), all within the
	 * one space it's stored in. A merge forgets a memory only if the merged
	 * text keeps its identifiers and most of its length; the forgotten ones
	 * are logged locally first. Without a model, or when the comparison fails
	 * or takes too long, it stores the memory as it is.
	 *
	 * quickSkip (capture) skips a near-copy without asking the model; an
	 * explicit remember always gets the model's judgment.
	 */
	async function storeConsolidated(ctx: ExtensionContext, m: StoreIn, signal: AbortSignal, quickSkip: boolean): Promise<Stored> {
		const srv = need();
		const space = await targetSpace(m.space, signal);
		const plain = async (why?: string): Promise<Stored> => {
			const args: Record<string, unknown> = { text: m.text, tags: m.tags };
			if (m.source) args.source = m.source;
			if (m.space || space) args.space = m.space || space;
			const out = await srv.call<{ uri: string; note?: string }>("remember", args, signal);
			return { action: "stored", uri: out.uri, text: m.text, tags: m.tags, replaced: [], kept: [], problems: [], note: out.note, why };
		};
		// Without knowing the space, comparing would mean searching all of them,
		// and merging across them: store it as it is.
		if (!cfg!.consolidate && !quickSkip) return plain();
		if (!space) return plain("couldn't tell which space it goes to, so it wasn't compared");
		let near: Memory[];
		try {
			near = (await srv.call<MemoriesOut>("recall", { query: m.text, limit: 5, mode: "vector", space }, signal)).memories.filter(
				(x) => (x.similarity ?? 0) >= cfg!.consolidateMinSimilarity && x.space === space,
			);
		} catch (err) {
			if (signal.aborted) throw err;
			return plain(`couldn't search for similar memories: ${errorText(err)}`);
		}
		if (quickSkip) {
			const same = near.filter((x) => (x.similarity ?? 0) >= cfg!.captureDedupeSimilarity);
			if (same.length) return { action: "skipped", existing: same };
		}
		if (!cfg!.consolidate || !near.length) return plain();
		if (!memoryModel(ctx)) return plain("no model to compare it with similar memories");
		let decision: Decision;
		try {
			const timed = AbortSignal.any([signal, AbortSignal.timeout(cfg!.consolidateTimeoutMs)]);
			const numbered = near.map((x, i) => ({ id: i, text: x.text, createdAt: x.createdAt, space: x.space }));
			// A little reasoning: without it the small model's choice between skip,
			// new and merge varies from one call to the next.
			const answer = await ask(
				ctx,
				CONSOLIDATE_PROMPT,
				consolidatePrompt(m.text, numbered, new Date().toISOString().slice(0, 10)),
				timed,
				8192,
				"low",
			);
			decision = parseDecision(answer, numbered.map((x) => x.id));
		} catch (err) {
			if (signal.aborted) throw err;
			return plain(`comparing it with similar memories failed: ${errorText(err)}`);
		}
		if (decision.action === "skip") return { action: "skipped", existing: decision.existing.map((i) => near[i]) };
		if (decision.action === "new") return plain();
		if (looksSecret(decision.text)) return plain("the merged text looked like it held a secret");
		// A "merge" that comes back as one old memory word for word added nothing.
		// It's a skip if the old memory has everything the new one names;
		// otherwise the model may have dropped the new fact, so store it as written.
		const unchanged = decision.replaces.map((i) => near[i]).find((o) => sameMemory(decision.text, o.text));
		if (unchanged)
			return ungrounded(m.text, unchanged.text).length
				? plain("the merge left out what the new memory names")
				: { action: "skipped", existing: [unchanged] };

		// A merge forgets only the memories whose facts it kept.
		const body = decision.text;
		// A merge may name only what the memories it combines (or today's date) do.
		const madeUp = ungrounded(
			body,
			[m.text, ...decision.replaces.map((i) => `${near[i].text}\n${near[i].createdAt ?? ""}`), new Date().toISOString()].join("\n"),
		);
		if (madeUp.length) return plain(`the merge named things none of the memories do (${madeUp.join(", ")})`);
		const chosen = decision.replaces.map((i) => near[i]);
		const olds = chosen.filter((o) => keepsFacts(body, o.text));
		const kept = chosen.filter((o) => !olds.includes(o)).map((o) => o.uri);
		if (!olds.length) return plain("the merge would have lost facts from the memories it replaced");
		const tags = memoryTags([...m.tags, ...olds.flatMap((o) => o.tags ?? [])]);
		const sources = [...new Set([m.source, ...olds.map((o) => o.source)].filter((x): x is string => !!x))];
		const args: Record<string, unknown> = { text: body, tags };
		if (sources.length) args.source = shorten(sources.join(" ; "), 1900);
		if (space) args.space = space;
		const out = await srv.call<{ uri: string; note?: string }>("remember", args, signal);
		const replaced: string[] = [];
		const problems: string[] = [];
		for (const o of olds) {
			keepForgotten(o, out.uri);
			try {
				await srv.call("forget", { uri: o.uri }, signal);
				replaced.push(o.uri);
			} catch (err) {
				if (signal.aborted) throw err;
				const msg = errorText(err);
				if (/only forget your own|belongs to/i.test(msg)) kept.push(o.uri); // someone else's: it stays
				else problems.push(`couldn't forget ${o.uri}: ${msg}`);
			}
		}
		return { action: "stored", uri: out.uri, text: body, tags, replaced, kept, problems, note: out.note };
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
		const model = memoryModel(ctx);
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
				const shown = userPrompt(piece, {
					project: project.name,
					cwd: ctx.cwd,
					date: new Date().toISOString().slice(0, 10),
					known,
					max: cfg.captureMax,
				});
				const res = await ctx.modelRegistry.complete(
					model,
					{
						systemPrompt: SYSTEM_PROMPT,
						messages: [
							{
								role: "user",
								content: text(shown),
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
					// A memory that names a commit, path or version the model wasn't
					// shown was made up, at least in part: agents would act on it.
					// Checked against the conversation itself (and where and when it ran),
					// not the memories the prompt listed as known.
					// What this pass already stored was grounded in this conversation too.
					const missing = ungrounded(
						c.text,
						[piece, ctx.cwd, project.name ?? "", new Date().toISOString(), ...record.stored.map((x) => x.text)].join("\n"),
					);
					if (missing.length) {
						(record.rejected ??= []).push({ text: c.text, missing });
						continue;
					}
					const r = await storeConsolidated(
						ctx,
						{
							text: c.text,
							tags: memoryTags([...cfg.captureTags, ...(project.name ? [project.name] : []), ...c.tags]),
							source: project.remote && !looksSecret(project.remote) ? project.remote : ctx.cwd,
							space: cfg.captureSpace,
						},
						signal,
						true,
					);
					if (r.action === "skipped") {
						record.skipped++;
						continue;
					}
					record.stored.push({
						uri: r.uri,
						text: r.text,
						tags: r.tags,
						...(r.replaced.length ? { replaced: r.replaced } : {}),
						...(r.problems.length ? { problems: r.problems } : {}),
					});
					known.push(r.text);
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
		const dropped = record.rejected?.length ?? 0;
		if (ctx.hasUI && (record.stored.length || dropped || report))
			ctx.ui.notify(
				(record.stored.length
					? `engram: remembered ${record.stored.length} ${record.stored.length === 1 ? "memory" : "memories"} from this session (/engram review)`
					: dropped
						? "engram: nothing stored from that stretch"
						: "engram: nothing worth keeping in that stretch") +
					(dropped ? `; dropped ${dropped} that named things not in the conversation (/engram review)` : ""),
				dropped && !record.stored.length ? "warning" : "info",
			);
	}

	pi.on("agent_settled", async (event, ctx) => {
		if (!event.aborted) {
			// A session that keeps working still gets stored as it goes: after
			// the first turn end, or once it has grown enough.
			const since = Date.now() - lastPushAt;
			const grown = (() => {
				try {
					const file = ctx.sessionManager.getSessionFile();
					if (!file) return 0;
					return statSync(file).size - syncedBytes(ctx.sessionManager.getEntries() as any[]);
				} catch {
					return 0;
				}
			})();
			if (since >= (cfg?.sessionSyncSeconds ?? 60) * 1000 || grown >= (cfg?.sessionSyncMinBytes ?? 200_000))
				void syncSession(ctx);
		}
		if (!cfg?.autoCapture || event.aborted || !captureAllowed(ctx)) return;
		void capture(ctx, cfg.captureMinChars);
	});

	pi.on("session_before_compact", async (_event, ctx) => {
		// The compacted messages stay in the session file, so this is only a
		// natural point to catch up, not a race against losing them.
		void syncSession(ctx);
		if (!cfg?.autoCapture || !captureAllowed(ctx)) return;
		void capture(ctx, cfg.captureExitMinChars);
	});

	pi.registerEntryRenderer<CaptureRecord>(CAPTURE_ENTRY, (entry, { expanded }, theme) => {
		const r = entry.data;
		if (!r || (!r.stored.length && !r.rejected?.length && !expanded)) return undefined;
		const box = new Box(1, 0, (t) => theme.bg("customMessageBg", t));
		const n = r.stored.length;
		let head = `${theme.fg("accent", "◆ engram")} ${theme.fg("muted", `remembered ${n} ${n === 1 ? "memory" : "memories"}`)}`;
		if (r.skipped) head += theme.fg("dim", ` (${r.skipped} already known)`);
		if (r.rejected?.length) head += theme.fg("warning", ` (${r.rejected.length} dropped: named things not in the conversation)`);
		if (!expanded) {
			const first = r.stored[0] ? theme.fg("dim", ` — ${shorten(r.stored[0].text.replace(/\s+/g, " "), 80)}`) : "";
			box.addChild(new Text(head + first, 0, 0));
			return box;
		}
		box.addChild(new Text(head + theme.fg("dim", ` from ${r.chars.toLocaleString()} chars with ${r.model}`), 0, 0));
		for (const m of r.stored) {
			const merged = m.replaced?.length ? theme.fg("dim", `  (merged with ${m.replaced.length} older, forgotten)`) : "";
			const probs = (m.problems ?? []).map((p) => `\n  ${theme.fg("warning", `⚠ ${p}`)}`).join("");
			box.addChild(new Text(`${theme.fg("dim", "•")} ${m.text}${merged}\n  ${theme.fg("dim", m.uri)}${probs}`, 0, 0));
		}
		for (const x of r.rejected ?? [])
			box.addChild(new Text(theme.fg("warning", `✗ ${shorten(x.text, 200)}\n  not in the conversation: ${x.missing.join(", ")}`), 0, 0));
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
			"Store a memory in an Engram space (the default one unless you pass space). Agents in that space, including your future sessions, find it with recall within seconds. Write it to stand on its own later, without this conversation. It's compared with the most similar memories first: if one already says it, nothing is stored; if it updates your own older ones (a later status, a correction), they're merged into one and the old ones forgotten. The result says which.",
		promptSnippet: "Store a durable memory for future sessions and other agents",
		promptGuidelines: [
			"Write each memory as a few sentences that stand alone: name the project, files, people and date rather than 'this' or 'the user'; add tags (the project, and a kind such as decision, preference, gotcha or status) and a source (URL, path, PR) when there is one.",
			"To update something, remember what changed: it's merged with the older memory. Use forget only for your own memories that are simply wrong.",
		],
		parameters: Type.Object({
			text: Type.String({ description: "The memory, written to make sense on its own later" }),
			tags: Type.Optional(Type.Array(Type.String(), { description: "Short labels: project, topic, kind" })),
			source: Type.Optional(Type.String({ description: "Where it came from: a URL, file path, PR or task" })),
			space: Type.Optional(Type.String({ description: "The space to store it in, by name or URI" })),
		}),
		async execute(_id, params, signal, _onUpdate, ctx): Promise<any> {
			const args = params.source ? { ...params, source: sanitizeSource(params.source) } : params;
			if ([args.text, args.source ?? "", ...(args.tags ?? [])].some(looksSecret))
				throw new Error("This looks like it contains a credential. Memories are readable by every agent in the space: leave the secret out.");
			const r = await storeConsolidated(
				ctx,
				{ text: args.text, tags: memoryTags(args.tags ?? []), source: args.source, space: args.space },
				signal ?? new AbortController().signal,
				false,
			);
			if (r.action === "skipped")
				return {
					content: text(
						`Not stored: a memory already says this. If it doesn't (a fact differs), remember again saying what changed.\n\n${formatMemories({ memories: r.existing }, { maxChars: 1500 })}`,
					),
					details: { skipped: r.existing.map((x) => x.uri), text: params.text },
				};
			const lines = [`Remembered: ${r.uri}`];
			if (r.replaced.length)
				lines.push(
					`Merged with ${r.replaced.length} older ${r.replaced.length === 1 ? "memory" : "memories"} it updates, now forgotten (${r.replaced.join(", ")}). Stored text:\n${r.text}`,
				);
			if (r.kept.length) lines.push(`Kept, though similar: ${r.kept.join(", ")}`);
			if (r.why) lines.push(`Stored as written: ${r.why}.`);
			for (const p of r.problems) lines.push(`Note: ${p}`);
			if (r.note) lines.push(`Note: ${r.note}`);
			return {
				content: text(lines.join("\n")),
				details: { uri: r.uri, text: r.text, tags: r.tags, note: r.note, replaced: r.replaced },
			};
		},
		renderCall: (args, theme) => title(theme, "remember", shorten(args.text.replace(/\s+/g, " "), 100)),
		renderResult: (result, _o, theme) => {
			const d = result.details as { note?: string; skipped?: string[]; replaced?: string[] } | undefined;
			if (d?.skipped) return new Text(theme.fg("dim", "already known; not stored"), 0, 0);
			if (d?.note) return new Text(theme.fg("warning", d.note), 0, 0);
			const n = d?.replaced?.length ?? 0;
			return new Text(theme.fg("success", n ? `✓ remembered, merged with ${n} older` : "✓ remembered"), 0, 0);
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

	pi.registerTool({
		name: "save_skill",
		label: "Save skill",
		description:
			"Create or update a shared skill in Engram: instructions for a kind of work that every agent in the space loads. Give the whole new instructions (Markdown, without frontmatter), not a diff. Files are kept unless you pass them. Prefer editing the skill's SKILL.md in the skills directory, which saves too; use this to create one with a note, or for skills you can't edit there.",
		promptSnippet: "Create or update a shared skill (instructions every agent in the space uses)",
		parameters: Type.Object({
			name: Type.String({ description: "Lowercase letters, digits and hyphens, such as attie-deploy" }),
			description: Type.String({ description: "What the skill does and when to use it; agents see only this until they load it" }),
			instructions: Type.String({ description: "The whole instructions, in Markdown" }),
			files: Type.Optional(
				Type.Array(
					Type.Object({
						path: Type.String({ description: "Relative to the skill's directory" }),
						content: Type.String(),
						executable: Type.Optional(Type.Boolean()),
					}),
					{ description: "Files beside SKILL.md, as text; omit to keep the current ones" },
				),
			),
			note: Type.Optional(Type.String({ description: "What this version changes and why" })),
			space: Type.Optional(Type.String({ description: "The space, by name or URI (default: where the skill is, else the default space)" })),
		}),
		async execute(_id, params, signal) {
			if (!validSkillName(params.name))
				throw new Error("A skill's name is lowercase letters, digits and single hyphens, at most 64 characters.");
			const blob = [params.instructions, params.description, params.note ?? "", ...(params.files ?? []).map((f) => f.content)];
			if (blob.some(looksSecret))
				throw new Error("This looks like it contains a credential. Skills are readable by every agent in the space: leave the secret out.");
			const args: Record<string, unknown> = { ...params };
			if (!args.space) {
				const known = cachedSkills(skillsDir).find((k) => k.name === params.name);
				if (known?.space) args.space = known.space;
			}
			const out = await need().call<{ uri: string; created: boolean; note?: string; space?: string }>("save_skill", args, signal);
			// Keep the copy on disk in step, so reading it shows the new version
			// now, unless it has changes of its own not in Engram.
			let kept = false;
			try {
				const full = await need().call<{ skill: Skill }>("get_skill", { name: params.name, space: out.space ?? args.space ?? "" }, signal);
				kept = !replaceSkill(skillsDir, full.skill);
			} catch {}
			return {
				content: text(
					`${out.created ? "Added" : "Saved"} skill ${params.name}: ${out.uri}${out.note ? `\nNote: ${out.note}` : ""}${out.created ? `\nIt's in ${join(skillsDir, params.name)}; pi lists it after /reload.` : ""}${kept ? `\nThe copy in ${join(skillsDir, params.name)} has changes of its own that aren't in Engram, so it was left as is: save them with /engram skills push, or redo them in this skill.` : ""}`,
				),
				details: { uri: out.uri, name: params.name, note: out.note },
			};
		},
		renderCall: (args, theme) => title(theme, "save_skill", `${args.name}${args.note ? ` — ${shorten(args.note, 80)}` : ""}`),
		renderResult: (result, _o, theme) => {
			const d = result.details as { note?: string } | undefined;
			return new Text(d?.note ? theme.fg("warning", d.note) : theme.fg("success", "✓ saved"), 0, 0);
		},
	});

	// ---- /engram ------------------------------------------------------------

	const subcommands = [
		{ value: "status", label: "status", description: "Engram's state in this session" },
		{ value: "spaces", label: "spaces", description: "The memory spaces, their models and indexing" },
		{ value: "recall", label: "recall", description: "Search memories yourself: /engram recall <query>" },
		{ value: "capture", label: "capture", description: "Capture memories from the conversation now" },
		{ value: "review", label: "review", description: "Look over (and forget) memories captured in this session" },
		{ value: "skills", label: "skills", description: "The skills from Engram; skills sync fetches them, skills push saves local changes" },
		{ value: "sessions", label: "sessions", description: "The sessions stored in Engram, with their machines and branches" },
		{ value: "pull", label: "pull", description: "Put a stored session on this machine: /engram pull <id>" },
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
		description: "Engram memory: status, spaces, recall, capture, review, skills, restart",
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
						`auto-capture: ${cfg?.autoCapture ? (captureAllowed(ctx) ? `on (${cfg.model ?? "session model"})` : "off for this session (not saved, or print mode)") : "off"}${capturing ? ", capturing now" : ""}`,
					);
					if (cfg?.autoSessionSync && captureAllowed(ctx)) {
						const stored = syncedBytes(ctx.sessionManager.getEntries() as any[]);
						const file = ctx.sessionManager.getSessionFile();
						let size = 0;
						try {
							size = file ? statSync(file).size : 0;
						} catch {
							size = 0;
						}
						const last = lastSync(ctx.sessionManager.getEntries() as any[]);
						lines.push(
							stored
								? `session stored: ${humanBytes(stored)} of ${humanBytes(size)}${last?.mode ? ` in ${last.mode}` : ""}${last?.error ? ` (${last.error})` : ""}`
								: `session stored: nothing yet (${humanBytes(size)})`,
						);
					} else lines.push("session sync: off for this session (not saved, print mode, or disabled)");
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
						for (const x of r.rejected ?? [])
							ms.push({
								uri: "(not stored)",
								text: `DROPPED, named things not in the conversation (${x.missing.join(", ")}): ${x.text}`,
								tags: [],
								author: "",
								createdAt: r.at,
							});
					}
					await showMemories(ctx, "Memories captured in this session", ms, true);
					return;
				}
				case "skills": {
					if (arg === "push") {
						const dirty = dirtySkills(skillsDir);
						if (!dirty.length) return ctx.ui.notify("engram: no skills changed here that aren't in Engram", "info");
						for (const name of dirty) {
							try {
								const out = await saveSkillDir(join(skillsDir, name));
								ctx.ui.notify(`engram: saved ${name} (${out.uri})`, "info");
							} catch (err) {
								ctx.ui.notify(`engram: ${name} not saved: ${errorText(err)}`, "error");
							}
						}
						return;
					}
					if (arg === "sync") {
						try {
							await syncSkills(ctx, true, true);
						} catch (err) {
							ctx.ui.notify(`engram: ${errorText(err)}`, "error");
						}
						return;
					}
					const ks = cachedSkills(skillsDir);
					const dirty = new Set(dirtySkills(skillsDir));
					ctx.ui.notify(
						ks.length
							? [
									`Skills from Engram, in ${skillsDir}:`,
									...ks.map((k) => `  ${k.name}  (${k.space}${k.author ? `, ${k.author}` : ""})${dirty.has(k.name) ? "  changed here, not saved" : ""}`),
								].join("\n")
							: `No skills from Engram yet. Add one: write ${skillsDir}/<name>/SKILL.md, or engram skill put <dir>.`,
						"info",
					);
					return;
				}
				case "sessions": {
					const out = await need().call<{ sessions: StoredSession[]; note?: string }>("list_sessions", {});
					if (!out.sessions.length) return ctx.ui.notify("engram: no stored sessions yet", "info");
					const lines = out.sessions.map((x) => {
						const where = [x.machine, x.branch].filter(Boolean).join(" ");
						const stored = x.head?.bytes ? `${x.head.entries ?? 0} entries, ${humanBytes(x.head.bytes)}` : "no transcript";
						return `${x.sessionId}\n  ${shorten(x.title ?? "", 90)}\n  ${[x.space, where, `updated ${day(x.updatedAt ?? "")}`, stored].filter(Boolean).join(" · ")}${x.cwd ? `\n  ${x.cwd}` : ""}`;
					});
					if (out.note) lines.push(out.note);
					ctx.ui.notify(lines.join("\n"), "info");
					return;
				}
				case "pull": {
					if (!arg) return ctx.ui.notify("usage: /engram pull <session id>", "warning");
					const out = await need().call<{ session: StoredSession; data: string; space?: string }>("pull_session", { session: arg });
					const path = putSession(ctx, out.session, out.data);
					ctx.ui.notify(
						`engram: ${out.session.sessionId} is at ${path}\nResume it with: pi --session ${path}` +
							(out.session.gitHead ? `\nIt was at ${out.session.branch ?? "a branch"} ${out.session.gitHead}${out.session.gitDirty ? " (uncommitted changes)" : ""}` : ""),
						"info",
					);
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
