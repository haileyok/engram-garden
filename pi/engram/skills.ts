// Skills on disk: engram's skills are written into a cache directory that pi
// loads like any skills directory, and edits the agent makes there are read
// back into records. The format is Agent Skills: a SKILL.md (YAML
// frontmatter, then Markdown) and the files beside it.

import { createHash } from "node:crypto";
import { existsSync, mkdirSync, readdirSync, readFileSync, renameSync, rmSync, statSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join, relative, sep } from "node:path";

export interface SkillFile {
	path: string;
	content: string;
	executable?: boolean;
}

export interface Skill {
	name: string;
	description: string;
	instructions: string;
	files?: SkillFile[];
	disableModelInvocation?: boolean;
	note?: string;
	createdAt?: string;
	updatedAt?: string;
	// From list_skills:
	space?: string;
	author?: string;
	uri?: string;
}

export function skillsCacheDir(env: NodeJS.ProcessEnv = process.env): string {
	const base = env.XDG_CACHE_HOME || join(homedir(), ".cache");
	return join(base, "pi-engram", "skills");
}

const NAME = /^[a-z0-9]+(-[a-z0-9]+)*$/;
export const validSkillName = (name: string) => name.length <= 64 && NAME.test(name);

// ---- SKILL.md ----

/** yamlString writes a value as a YAML double-quoted scalar (JSON's escapes are valid there). */
const yamlString = (s: string) => JSON.stringify(s);

export function formatSkillMd(s: Skill): string {
	const head = [`name: ${s.name}`, `description: ${yamlString(s.description)}`];
	if (s.disableModelInvocation) head.push("disable-model-invocation: true");
	const body = s.instructions.replace(/^\n+/, "");
	return `---\n${head.join("\n")}\n---\n\n${body.endsWith("\n") ? body : `${body}\n`}`;
}

/** parseScalar reads one YAML scalar: double- or single-quoted (a comment may follow), or plain. */
function parseScalar(raw: string): string {
	const v = raw.trim();
	if (v.startsWith('"')) {
		// The closing quote is the first unescaped one.
		for (let i = 1; i < v.length; i++) {
			if (v[i] === "\\") i++;
			else if (v[i] === '"') return JSON.parse(v.slice(0, i + 1));
		}
		throw new Error("a double-quoted value in SKILL.md's frontmatter has no closing quote");
	}
	if (v.startsWith("'")) {
		const m = v.match(/^'((?:[^']|'')*)'/);
		if (m) return m[1].replace(/''/g, "'");
	}
	return v.replace(/\s+#.*$/, "");
}

/**
 * parseSkillMd reads the frontmatter keys a skill record keeps (name,
 * description, disable-model-invocation) and the instructions. It handles
 * plain and quoted scalars and | / > block scalars; other keys are ignored.
 */
export function parseSkillMd(text: string): Pick<Skill, "name" | "description" | "instructions" | "disableModelInvocation"> {
	const t = text.replace(/\r\n/g, "\n");
	if (!t.startsWith("---\n")) throw new Error("SKILL.md must start with YAML frontmatter between --- lines");
	// The closing line is exactly ---, as Go reads it.
	let end = t.indexOf("\n---\n", 4);
	if (end < 0 && t.endsWith("\n---") && t.length - 4 >= 4) end = t.length - 4;
	if (end < 0) throw new Error("SKILL.md's frontmatter has no closing --- line");
	const lines = t.slice(4, end).split("\n");
	const after = t.slice(end + 4);
	const body = after.startsWith("\n") ? after.slice(1) : after;
	const fm: Record<string, string> = {};
	for (let i = 0; i < lines.length; i++) {
		const m = lines[i].match(/^([A-Za-z0-9_-]+):\s*(.*)$/);
		if (!m) continue;
		const [, key, rest] = m;
		if (/^[|>][+-]?$/.test(rest.trim())) {
			const block: string[] = [];
			while (i + 1 < lines.length && (/^\s/.test(lines[i + 1]) || lines[i + 1] === "")) block.push(lines[++i]);
			const indent = Math.min(...block.filter((l) => l.trim()).map((l) => l.match(/^\s*/)![0].length));
			const texts = block.map((l) => l.slice(Number.isFinite(indent) ? indent : 0));
			// Folded (>): lines join with spaces, a blank line is a line break.
			const folded = () =>
				texts
					.join("\n")
					.trim()
					.split(/\n\s*\n/)
					.map((para) => para.split("\n").map((l) => l.trim()).join(" "))
					.join("\n");
			fm[key] = rest.trim().startsWith(">") ? folded() : texts.join("\n").trim();
		} else {
			// A plain value may go on over more lines, each indented: YAML
			// folds them into one, separated by spaces.
			let value = rest;
			if (!/^["']/.test(rest.trim())) {
				while (i + 1 < lines.length && /^\s+\S/.test(lines[i + 1]) && !/^\s*#/.test(lines[i + 1]))
					value += ` ${lines[++i].trim()}`;
			}
			fm[key] = parseScalar(value);
		}
	}
	return {
		name: (fm.name ?? "").trim(),
		description: (fm.description ?? "").trim(),
		disableModelInvocation: /^true$/i.test(fm["disable-model-invocation"] ?? ""),
		instructions: body.replace(/^\n+/, ""),
	};
}

// ---- directories ----

const skipped = (name: string) => name.startsWith(".") || name === "node_modules" || name === "__pycache__";

/** readSkillDir reads a skill directory into a record's fields. */
export function readSkillDir(dir: string): Skill {
	const s: Skill = { ...parseSkillMd(readFileSync(join(dir, "SKILL.md"), "utf8")), files: [] };
	const walk = (d: string) => {
		for (const e of readdirSync(d, { withFileTypes: true })) {
			if (skipped(e.name)) continue;
			const p = join(d, e.name);
			const rel = relative(dir, p).split(sep).join("/");
			if (e.isDirectory()) walk(p);
			else if (!e.isFile()) throw new Error(`${rel}: only regular files can be part of a skill`);
			else {
				if (rel === "SKILL.md") continue;
				const buf = readFileSync(p);
				let content: string;
				try {
					if (buf.includes(0)) throw new Error();
					content = new TextDecoder("utf-8", { fatal: true }).decode(buf);
				} catch {
					throw new Error(`${rel} isn't text; a skill's files are kept as text`);
				}
				s.files!.push({ path: rel, content, executable: (statSync(p).mode & 0o111) !== 0 });
			}
		}
	};
	walk(dir);
	s.files!.sort((a, b) => a.path.localeCompare(b.path));
	return s;
}

export function writeSkillDir(dir: string, s: Skill) {
	mkdirSync(dir, { recursive: true });
	writeFileSync(join(dir, "SKILL.md"), formatSkillMd(s));
	for (const f of s.files ?? []) {
		const p = join(dir, ...f.path.split("/"));
		const segs = f.path.split("/");
		if (!p.startsWith(dir + sep) || segs.some(skipped) || segs.includes("SKILL.md")) continue; // records are checked; belt and braces
		mkdirSync(dirname(p), { recursive: true });
		writeFileSync(p, f.content, { mode: f.executable ? 0o755 : 0o644 });
	}
}

/**
 * dirHash identifies a skill directory's contents as they are on disk, or
 * undefined when it doesn't exist or holds nothing. Everything counts,
 * dotfiles, installed dependencies and links too (by name and size), so a
 * directory with anything an agent put there is never taken for empty.
 */
export function dirHash(dir: string): string | undefined {
	if (!existsSync(dir)) return undefined;
	const h = createHash("sha256");
	const entries: Array<{ p: string; content: boolean }> = [];
	const walk = (d: string, inSkipped: boolean) => {
		for (const e of readdirSync(d, { withFileTypes: true })) {
			const p = join(d, e.name);
			const sk = inSkipped || skipped(e.name);
			if (e.isDirectory()) walk(p, sk);
			else entries.push({ p, content: e.isFile() && !sk });
		}
	};
	walk(dir, false);
	if (!entries.length) return undefined;
	for (const { p, content } of entries.sort((a, b) => (a.p < b.p ? -1 : a.p > b.p ? 1 : 0))) {
		const st = statSync(p, { throwIfNoEntry: false });
		h.update(`${relative(dir, p)}\0${st && st.mode & 0o111 ? 1 : 0}\0`);
		if (content) h.update(readFileSync(p));
		else h.update(`skipped:${st?.size ?? -1}`);
		h.update("\0");
	}
	return h.digest("hex");
}

/** What the cache's index says about a skill: where it's from, and what was written. */
export interface CachedSkill {
	name: string;
	space?: string;
	author?: string;
	uri?: string;
	/** dirHash of the directory when it last matched Engram. */
	hash?: string;
}

const INDEX = ".skills.json";

export function cachedSkills(cacheDir: string): CachedSkill[] {
	try {
		const v = JSON.parse(readFileSync(join(cacheDir, INDEX), "utf8"));
		return Array.isArray(v) ? v.filter((e) => e && typeof e.name === "string") : [];
	} catch {
		return [];
	}
}

function saveIndex(cacheDir: string, entries: CachedSkill[]) {
	const tmp = join(cacheDir, `${INDEX}.tmp-${process.pid}`);
	writeFileSync(tmp, JSON.stringify(entries, null, 1));
	renameSync(tmp, join(cacheDir, INDEX));
}

/**
 * withLock runs fn while holding the cache's lock, so two pi processes don't
 * rewrite it at once. A lock older than a minute is taken to be left over.
 */
export function withLock<T>(cacheDir: string, fn: () => T): T {
	mkdirSync(cacheDir, { recursive: true });
	const lock = join(cacheDir, ".lock");
	for (let i = 0; ; i++) {
		try {
			mkdirSync(lock);
			break;
		} catch (err: any) {
			if (err?.code !== "EEXIST") throw err;
			try {
				if (Date.now() - statSync(lock).mtimeMs > 60_000) rmSync(lock, { recursive: true, force: true });
			} catch {}
			if (i > 200) throw new Error("the skills cache is locked by another pi");
			Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 25);
		}
	}
	try {
		return fn();
	} finally {
		rmSync(lock, { recursive: true, force: true });
	}
}

/** dirtySkills lists skills in the cache changed on disk since they last matched Engram. */
export function dirtySkills(cacheDir: string): string[] {
	const index = new Map(cachedSkills(cacheDir).map((e) => [e.name, e]));
	const out: string[] = [];
	if (!existsSync(cacheDir)) return out;
	for (const e of readdirSync(cacheDir, { withFileTypes: true })) {
		if (!e.isDirectory() || !validSkillName(e.name)) continue;
		const h = dirHash(join(cacheDir, e.name));
		if (h && h !== index.get(e.name)?.hash) out.push(e.name);
	}
	return out.sort();
}

/**
 * swapIn puts the prepared directory tmp in place of cacheDir/name, if
 * what's there still hashes to expect (undefined: nothing there). It checks
 * again with the old directory moved aside, and after the swap, so an edit
 * by another pi landing meanwhile is never lost; a failed rename puts the
 * old directory back. It returns whether the swap happened.
 */
function swapIn(cacheDir: string, name: string, tmp: string, expect: string | undefined): boolean {
	const dir = join(cacheDir, name);
	const old = join(cacheDir, `.${name}.old-${process.pid}`);
	const giveUp = () => {
		rmSync(tmp, { recursive: true, force: true });
		return false;
	};
	if (dirHash(dir) !== expect) return giveUp();
	rmSync(old, { recursive: true, force: true });
	const moved = existsSync(dir);
	if (moved) {
		renameSync(dir, old);
		if (dirHash(old) !== expect) {
			renameSync(old, dir);
			return giveUp();
		}
	}
	try {
		renameSync(tmp, dir);
	} catch (err) {
		if (moved) renameSync(old, dir);
		rmSync(tmp, { recursive: true, force: true });
		throw err;
	}
	if (moved && dirHash(old) !== expect) {
		rmSync(dir, { recursive: true, force: true });
		renameSync(old, dir);
		return false;
	}
	rmSync(old, { recursive: true, force: true });
	return true;
}

export interface WriteResult {
	/** Whether any skill on disk changed. */
	changed: boolean;
	/** Skills with local changes not in Engram, left as they are. */
	kept: string[];
}

/**
 * writeSkills brings the cache in step with these skills, one directory per
 * name (the first of a name wins: list them in order of precedence). A skill
 * changed on disk since it last matched Engram is left alone and reported,
 * so an edit not saved yet is never lost.
 */
export function writeSkills(cacheDir: string, skills: Skill[]): WriteResult {
	return withLock(cacheDir, () => {
		const seen = new Set<string>();
		const keep = skills.filter((s) => validSkillName(s.name) && !seen.has(s.name) && seen.add(s.name));
		const index = new Map(cachedSkills(cacheDir).map((e) => [e.name, e]));
		const next: CachedSkill[] = [];
		const kept: string[] = [];
		let changed = false;
		const local = (name: string) => {
			const h = dirHash(join(cacheDir, name));
			return { h, dirty: h !== undefined && h !== index.get(name)?.hash };
		};
		for (const s of keep) {
			const dir = join(cacheDir, s.name);
			const meta = { name: s.name, space: s.space, author: s.author, uri: s.uri };
			const tmp = join(cacheDir, `.${s.name}.tmp-${process.pid}`);
			rmSync(tmp, { recursive: true, force: true });
			writeSkillDir(tmp, s);
			const want = dirHash(tmp)!;
			const cur = local(s.name);
			if (cur.h === want) {
				rmSync(tmp, { recursive: true, force: true });
				next.push({ ...meta, hash: want });
				continue;
			}
			if (cur.dirty || !swapIn(cacheDir, s.name, tmp, cur.h)) {
				rmSync(tmp, { recursive: true, force: true });
				kept.push(s.name);
				next.push({ ...meta, hash: index.get(s.name)?.hash });
				continue;
			}
			next.push({ ...meta, hash: want });
			changed = true;
		}
		// Skills no longer in Engram go, unless they've changed here.
		for (const e of readdirSync(cacheDir, { withFileTypes: true })) {
			if (!e.isDirectory() || !validSkillName(e.name) || seen.has(e.name)) continue;
			if (local(e.name).dirty) {
				kept.push(e.name);
				const prev = index.get(e.name);
				next.push({ name: e.name, space: prev?.space, hash: prev?.hash });
				continue;
			}
			rmSync(join(cacheDir, e.name), { recursive: true, force: true });
			changed = true;
		}
		saveIndex(cacheDir, next);
		return { changed, kept: kept.sort() };
	});
}

/**
 * markSaved records that a skill directory matches what's in Engram: the
 * contents that hashed to savedHash before saving. If the directory changed
 * since (an edit during the save), it stays unsaved.
 */
export function markSaved(cacheDir: string, name: string, meta: Omit<CachedSkill, "name" | "hash">, savedHash?: string) {
	withLock(cacheDir, () => {
		if (savedHash !== undefined && dirHash(join(cacheDir, name)) !== savedHash) return;
		const all = cachedSkills(cacheDir);
		const prev = all.find((e) => e.name === name);
		const set = Object.fromEntries(Object.entries(meta).filter(([, v]) => v !== undefined));
		const entries = all.filter((e) => e.name !== name);
		entries.push({ ...prev, ...set, name, hash: dirHash(join(cacheDir, name)) });
		saveIndex(cacheDir, entries);
	});
}

/** skillDirOf names the skill directory a path is in, if it's inside the cache. */
export function skillDirOf(path: string, cacheDir: string): string | undefined {
	const rel = relative(cacheDir, path);
	if (!rel || rel.startsWith("..") || rel.startsWith(sep) || rel.includes(`..${sep}`)) return undefined;
	const name = rel.split(sep)[0];
	return validSkillName(name) ? join(cacheDir, name) : undefined;
}

/**
 * replaceSkill writes one skill's directory, as now in Engram, and records
 * it, unless it has changes on disk not in Engram: then it's left alone and
 * replaceSkill returns false.
 */
export function replaceSkill(cacheDir: string, s: Skill): boolean {
	if (!validSkillName(s.name)) return false;
	return withLock(cacheDir, () => {
		const dir = join(cacheDir, s.name);
		const h = dirHash(dir);
		if (h !== undefined && h !== cachedSkills(cacheDir).find((e) => e.name === s.name)?.hash) return false;
		const tmp = join(cacheDir, `.${s.name}.tmp-${process.pid}`);
		rmSync(tmp, { recursive: true, force: true });
		writeSkillDir(tmp, s);
		if (!swapIn(cacheDir, s.name, tmp, h)) return false;
		const entries = cachedSkills(cacheDir).filter((e) => e.name !== s.name);
		entries.push({ name: s.name, space: s.space, author: s.author, uri: s.uri, hash: dirHash(dir) });
		saveIndex(cacheDir, entries);
		return true;
	});
}
