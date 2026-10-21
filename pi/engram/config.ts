// The extension's settings: ~/.pi/agent/engram.json, then a trusted
// project's .pi/engram.json, then PI_ENGRAM_* environment variables. Which
// account and spaces engram-mcp uses is engram's own business
// (~/.config/engram/config.json, set up with the engram CLI).

import { existsSync, readFileSync } from "node:fs";
import { homedir } from "node:os";
import { delimiter, dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

export interface Config {
	/** The engram-mcp executable: a path or a name on PATH. */
	command: string;
	args: string[];
	env: Record<string, string>;

	/** Recall memories for each prompt and add them to the conversation. */
	autoRecall: boolean;
	/** At most this many memories per prompt. */
	recallLimit: number;
	/** Leave out memories less similar than this (0-1000). */
	recallMinSimilarity: number;
	/** Also leave out memories this far below the best match. */
	recallMaxGap: number;
	/** Give up on recalling for a prompt after this long. */
	recallTimeoutMs: number;
	/** How long the first prompt may wait for engram-mcp to start (about 3s). */
	recallStartTimeoutMs: number;
	/** Prompts shorter than this ("go ahead", "yes") aren't searched. */
	recallMinPromptChars: number;
	/** Each recalled memory is shortened to this many characters. */
	recallMaxChars: number;

	/** Extract memories from the conversation in the background. */
	autoCapture: boolean;
	/** "provider/model" for extraction; the session's model if unset. */
	captureModel?: string;
	/** The space captured memories go to; the default space if unset. */
	captureSpace?: string;
	/** Tags every captured memory carries, besides the project's name. */
	captureTags: string[];
	/** Capture once this much new conversation text has built up. */
	captureMinChars: number;
	/** When the session ends, capture what's left if it's at least this long. */
	captureExitMinChars: number;
	/** How long quitting may wait for capturing (switching sessions: at most 8s). */
	captureExitTimeoutMs: number;
	/** At most this many memories per 80k characters of conversation read. */
	captureMax: number;
	/** Skip a captured memory when one this similar exists (0-1000). */
	captureDedupeSimilarity: number;
}

export const defaults: Config = {
	command: "engram-mcp",
	args: [],
	env: {},
	autoRecall: true,
	recallLimit: 5,
	recallMinSimilarity: 650,
	recallMaxGap: 80,
	recallTimeoutMs: 4000,
	recallStartTimeoutMs: 10_000,
	recallMinPromptChars: 12,
	recallMaxChars: 700,
	autoCapture: true,
	captureTags: ["auto"],
	captureMinChars: 24_000,
	captureExitMinChars: 3_000,
	captureExitTimeoutMs: 20_000,
	captureMax: 5,
	captureDedupeSimilarity: 860,
};

export function agentDir(): string {
	return process.env.PI_CODING_AGENT_DIR || join(homedir(), ".pi", "agent");
}

function readJSON(path: string): Partial<Config> {
	if (!existsSync(path)) return {};
	try {
		return JSON.parse(readFileSync(path, "utf8")) as Partial<Config>;
	} catch (err) {
		throw new Error(`${path}: ${err instanceof Error ? err.message : err}`);
	}
}

const bool = (v: string) => v === "1" || v.toLowerCase() === "true";

/** check makes sure each setting has its default's type. */
function check(cfg: Record<string, unknown>) {
	for (const [k, v] of Object.entries(cfg)) {
		if (k === "captureModel" || k === "captureSpace") {
			if (v !== undefined && typeof v !== "string") throw new Error(`${k} must be a string`);
			continue;
		}
		const want = (defaults as unknown as Record<string, unknown>)[k];
		if (want === undefined) throw new Error(`unknown setting ${k}`);
		if (Array.isArray(want)) {
			if (!Array.isArray(v) || v.some((x) => typeof x !== "string")) throw new Error(`${k} must be a list of strings`);
		} else if (typeof want === "object") {
			if (typeof v !== "object" || v === null || Array.isArray(v) || Object.values(v).some((x) => typeof x !== "string"))
				throw new Error(`${k} must be an object of strings`);
		} else if (typeof v !== typeof want) throw new Error(`${k} must be a ${typeof want}`);
	}
}

/** loadConfig merges the settings files and environment over the defaults. */
export function loadConfig(projectDir?: string, env: NodeJS.ProcessEnv = process.env): Config {
	const cfg: Config = { ...defaults, ...readJSON(join(agentDir(), "engram.json")) };
	if (projectDir) Object.assign(cfg, readJSON(join(projectDir, ".pi", "engram.json")));
	check(cfg as unknown as Record<string, unknown>);
	if (env.PI_ENGRAM_COMMAND) cfg.command = env.PI_ENGRAM_COMMAND;
	if (env.PI_ENGRAM_AUTO_RECALL) cfg.autoRecall = bool(env.PI_ENGRAM_AUTO_RECALL);
	if (env.PI_ENGRAM_AUTO_CAPTURE) cfg.autoCapture = bool(env.PI_ENGRAM_AUTO_CAPTURE);
	if (env.PI_ENGRAM_CAPTURE_MODEL) cfg.captureModel = env.PI_ENGRAM_CAPTURE_MODEL;
	cfg.command = resolveCommand(cfg.command, env);
	return cfg;
}

/**
 * resolveCommand finds engram-mcp: as given if it's a path, else on PATH,
 * else next to this package when it was installed from the flake
 * (share/pi-engram beside bin/), else in a Nix profile or GOPATH.
 */
export function resolveCommand(command: string, env: NodeJS.ProcessEnv = process.env): string {
	if (command.includes("/")) return command.startsWith("~/") ? join(homedir(), command.slice(2)) : command;
	for (const dir of (env.PATH ?? "").split(delimiter)) {
		if (dir && existsSync(join(dir, command))) return join(dir, command);
	}
	const here = dirname(fileURLToPath(import.meta.url));
	const candidates = [
		join(here, "..", "..", "..", "bin", command), // $out/share/pi-engram/engram -> $out/bin
		join(homedir(), ".nix-profile", "bin", command),
		join(env.GOPATH || join(homedir(), "go"), "bin", command),
	];
	return candidates.find((p) => existsSync(p)) ?? command;
}
