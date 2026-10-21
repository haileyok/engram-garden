// A minimal MCP client for one stdio server: engram-mcp. It speaks
// newline-delimited JSON-RPC, which is all the stdio transport is, so the
// package needs no dependencies (pi supplies only its own packages to
// extensions, and a Nix store copy can't be npm-installed).

import { type ChildProcess, spawn } from "node:child_process";

export const PROTOCOL_VERSION = "2025-06-18";
const MAX_LINE = 64 * 1024 * 1024;

export interface ToolResult {
	content: Array<{ type: string; text?: string }>;
	structuredContent?: unknown;
	isError?: boolean;
}

export interface ToolInfo {
	name: string;
	description?: string;
	inputSchema: Record<string, unknown>;
	annotations?: Record<string, unknown>;
}

export interface ServerOptions {
	command: string;
	args?: string[];
	env?: Record<string, string>;
	/** How long to wait for the server to start and answer initialize. */
	startTimeoutMs?: number;
	/** How long to wait for any one answer. */
	requestTimeoutMs?: number;
	/** The longest answer accepted, in characters (default 64 MiB). */
	maxLine?: number;
}

interface Pending {
	resolve: (v: unknown) => void;
	reject: (e: Error) => void;
	cleanup: () => void;
}

/** RpcError is an error the server returned for a request. */
export class RpcError extends Error {
	code: number;
	constructor(code: number, message: string) {
		super(message);
		this.code = code;
	}
}

/**
 * McpServer runs engram-mcp and calls its tools. It starts on the first
 * call (or start()), and starts again on the next call after the process
 * exits, so a crash costs one failed call rather than the session.
 */
export class McpServer {
	readonly opts: ServerOptions;
	instructions = "";
	serverVersion = "";
	/** The tail of the server's stderr, for diagnosing a failed start. */
	stderrTail: string[] = [];
	/** Why the last start failed, if it did. */
	lastError: string | undefined;

	private proc: ChildProcess | undefined;
	private ready: Promise<void> | undefined;
	private nextId = 1;
	private pending = new Map<number, Pending>();
	private buf = "";
	private closed = false;

	constructor(opts: ServerOptions) {
		this.opts = opts;
	}

	get running(): boolean {
		return this.proc !== undefined && this.proc.exitCode === null && !this.proc.killed;
	}

	/** start launches the server and completes the MCP handshake. */
	start(): Promise<void> {
		if (this.closed) return Promise.reject(new Error("engram-mcp was stopped"));
		if (this.ready) return this.ready;
		this.ready = this.launch().catch((err) => {
			this.lastError = errorText(err);
			this.kill();
			this.ready = undefined;
			throw err;
		});
		return this.ready;
	}

	private async launch(): Promise<void> {
		const proc = spawn(this.opts.command, this.opts.args ?? [], {
			stdio: ["pipe", "pipe", "pipe"],
			env: { ...process.env, ...this.opts.env },
		});
		this.proc = proc;
		this.buf = "";
		const exited = new Promise<never>((_, reject) => {
			proc.once("error", (err) => reject(new Error(`can't run ${this.opts.command}: ${err.message}`)));
			proc.once("exit", (code, signal) =>
				reject(new Error(`${this.opts.command} exited (${signal ?? code})${this.stderrHint()}`)),
			);
		});
		exited.catch(() => {});
		proc.once("exit", () => this.onExit(proc));
		proc.once("error", () => this.onExit(proc));
		proc.stdout!.setEncoding("utf8");
		proc.stdout!.on("data", (chunk: string) => this.onData(chunk));
		proc.stderr!.setEncoding("utf8");
		proc.stderr!.on("data", (chunk: string) => {
			for (const line of chunk.split("\n")) {
				if (!line.trim()) continue;
				this.stderrTail.push(line.length > 2000 ? `${line.slice(0, 2000)}…` : line);
				if (this.stderrTail.length > 40) this.stderrTail.shift();
			}
		});
		proc.stdin!.on("error", () => {}); // reported by exit

		const timeout = this.opts.startTimeoutMs ?? 30_000;
		const init = this.send("initialize", {
			protocolVersion: PROTOCOL_VERSION,
			capabilities: {},
			clientInfo: { name: "pi-engram", version: "0.1.0" },
		});
		const res = (await Promise.race([
			init,
			exited,
			sleep(timeout).then(() => {
				throw new Error(`engram-mcp didn't start within ${timeout / 1000}s${this.stderrHint()}`);
			}),
		])) as { instructions?: string; serverInfo?: { version?: string } };
		this.instructions = res.instructions ?? "";
		this.serverVersion = res.serverInfo?.version ?? "";
		this.notify("notifications/initialized", {});
		this.lastError = undefined;
	}

	private stderrHint(): string {
		const last = this.stderrTail.slice(-3).join(" | ");
		return last ? `: ${last}` : "";
	}

	private onExit(proc: ChildProcess) {
		if (this.proc !== proc) return;
		this.proc = undefined;
		this.ready = undefined;
		const err = new Error(`engram-mcp exited${this.stderrHint()}`);
		for (const p of this.pending.values()) {
			p.cleanup();
			p.reject(err);
		}
		this.pending.clear();
	}

	private onData(chunk: string) {
		this.buf += chunk;
		let nl: number;
		while ((nl = this.buf.indexOf("\n")) >= 0) {
			const line = this.buf.slice(0, nl).trim();
			this.buf = this.buf.slice(nl + 1);
			if (!line) continue;
			if (line.length > (this.opts.maxLine ?? MAX_LINE)) return this.tooBig();
			let msg: any;
			try {
				msg = JSON.parse(line);
			} catch {
				continue;
			}
			this.onMessage(msg);
		}
		if (this.buf.length > (this.opts.maxLine ?? MAX_LINE)) this.tooBig();
	}

	private tooBig() {
		// Not engram-mcp as we know it: stop rather than grow without end.
		this.buf = "";
		this.stderrTail.push("pi-engram: an answer too large to be engram-mcp's; stopping it");
		this.kill();
	}

	private onMessage(msg: any) {
		if (msg.method !== undefined) {
			// A request or notification from the server. engram-mcp sends
			// none we act on; answer requests so the server never waits.
			if (msg.id !== undefined) {
				if (msg.method === "ping") this.write({ jsonrpc: "2.0", id: msg.id, result: {} });
				else this.write({ jsonrpc: "2.0", id: msg.id, error: { code: -32601, message: "method not found" } });
			}
			return;
		}
		const p = this.pending.get(msg.id);
		if (!p) return;
		this.pending.delete(msg.id);
		p.cleanup();
		if (msg.error) p.reject(new RpcError(msg.error.code ?? -1, msg.error.message ?? "error"));
		else p.resolve(msg.result);
	}

	private write(msg: unknown) {
		this.proc?.stdin?.write(`${JSON.stringify(msg)}\n`);
	}

	private notify(method: string, params: unknown) {
		this.write({ jsonrpc: "2.0", method, params });
	}

	private send(method: string, params: unknown, signal?: AbortSignal): Promise<unknown> {
		if (!this.proc) return Promise.reject(new Error("engram-mcp isn't running"));
		const id = this.nextId++;
		const timeoutMs = this.opts.requestTimeoutMs ?? 120_000;
		return new Promise((resolve, reject) => {
			const give = (why: string) => {
				if (!this.pending.delete(id)) return;
				cleanup();
				this.notify("notifications/cancelled", { requestId: id, reason: why });
				reject(new Error(why));
			};
			const onAbort = () => give("aborted");
			const timer = setTimeout(() => give(`engram-mcp didn't answer ${method} within ${timeoutMs / 1000}s`), timeoutMs);
			timer.unref();
			const cleanup = () => {
				clearTimeout(timer);
				signal?.removeEventListener("abort", onAbort);
			};
			if (signal?.aborted) {
				cleanup();
				return reject(new Error("aborted"));
			}
			signal?.addEventListener("abort", onAbort, { once: true });
			this.pending.set(id, { resolve, reject, cleanup });
			this.write({ jsonrpc: "2.0", id, method, params });
		});
	}

	/** request starts the server if needed, then sends one request. */
	async request(method: string, params: unknown, signal?: AbortSignal): Promise<unknown> {
		await abortable(this.start(), signal);
		return this.send(method, params, signal);
	}

	async listTools(signal?: AbortSignal): Promise<ToolInfo[]> {
		const tools: ToolInfo[] = [];
		let cursor: string | undefined;
		do {
			const res = (await this.request("tools/list", cursor ? { cursor } : {}, signal)) as {
				tools: ToolInfo[];
				nextCursor?: string;
			};
			tools.push(...res.tools);
			cursor = res.nextCursor;
		} while (cursor);
		return tools;
	}

	callTool(name: string, args: Record<string, unknown>, signal?: AbortSignal): Promise<ToolResult> {
		return this.request("tools/call", { name, arguments: args }, signal) as Promise<ToolResult>;
	}

	/**
	 * call runs a tool and returns its structured result, throwing the
	 * server's explanation when the tool failed.
	 */
	async call<T>(name: string, args: Record<string, unknown>, signal?: AbortSignal): Promise<T> {
		const res = await this.callTool(name, args, signal);
		const text = (res.content ?? [])
			.map((c) => c.text ?? "")
			.join("\n")
			.trim();
		if (res.isError) throw new Error(text || `${name} failed`);
		if (res.structuredContent !== undefined) return res.structuredContent as T;
		try {
			return JSON.parse(text) as T;
		} catch {
			return text as T;
		}
	}

	private kill() {
		const proc = this.proc;
		if (!proc) return;
		this.proc = undefined;
		this.ready = undefined;
		const err = new Error(`engram-mcp was stopped${this.stderrHint()}`);
		for (const p of this.pending.values()) {
			p.cleanup();
			p.reject(err);
		}
		this.pending.clear();
		try {
			proc.stdin?.end();
		} catch {}
		proc.kill("SIGTERM");
		const t = setTimeout(() => proc.kill("SIGKILL"), 3000);
		t.unref();
		proc.once("exit", () => clearTimeout(t));
	}

	/** close stops the server for good. */
	close() {
		this.closed = true;
		for (const p of this.pending.values()) {
			p.cleanup();
			p.reject(new Error("engram-mcp was stopped"));
		}
		this.pending.clear();
		this.kill();
	}
}

export function errorText(err: unknown): string {
	return err instanceof Error ? err.message : String(err);
}

/** abortable settles with p, or rejects when signal aborts first (p keeps running). */
function abortable<T>(p: Promise<T>, signal?: AbortSignal): Promise<T> {
	if (!signal) return p;
	if (signal.aborted) return Promise.reject(new Error("aborted"));
	return new Promise((resolve, reject) => {
		const onAbort = () => reject(new Error("aborted"));
		signal.addEventListener("abort", onAbort, { once: true });
		p.then(
			(v) => {
				signal.removeEventListener("abort", onAbort);
				resolve(v);
			},
			(e) => {
				signal.removeEventListener("abort", onAbort);
				reject(e);
			},
		);
	});
}

function sleep(ms: number): Promise<void> {
	return new Promise((r) => setTimeout(r, ms).unref());
}
