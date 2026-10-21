import assert from "node:assert/strict";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import {
	cachedSkills,
	dirtySkills,
	dirHash as dirHashOf,
	formatSkillMd,
	markSaved,
	parseSkillMd,
	readSkillDir,
	skillDirOf,
	validSkillName,
	withLock,
	writeSkills,
} from "../engram/skills.ts";

test("SKILL.md round-trips through format and parse", () => {
	for (const s of [
		{ name: "a", description: 'Has "quotes": a colon, and a #hash', instructions: "# A\n\nDo it.\n" },
		{ name: "b", description: "- looks like a list\nand two lines", instructions: "no newline", disableModelInvocation: true },
	]) {
		const back = parseSkillMd(formatSkillMd(s));
		assert.equal(back.name, s.name);
		assert.equal(back.description, s.description);
		assert.equal(back.disableModelInvocation, !!s.disableModelInvocation);
		assert.equal(back.instructions, s.instructions.endsWith("\n") ? s.instructions : `${s.instructions}\n`);
	}
});

test("parseSkillMd reads hand-written frontmatter", () => {
	const s = parseSkillMd(`---
name: attie-deploy
description: Deploy Attie to pop1. Use for any "ship it" request. # trailing comment
license: MIT
metadata:
  owner: hailey
---

# Deploy
`);
	assert.equal(s.name, "attie-deploy");
	assert.equal(s.description, 'Deploy Attie to pop1. Use for any "ship it" request.');
	assert.equal(s.instructions, "# Deploy\n");
	const folded = parseSkillMd("---\nname: x\ndescription: >\n  one\n  two\n---\nbody\n");
	assert.equal(folded.description, "one two");
	const literal = parseSkillMd("---\nname: x\ndescription: |\n  one\n  two\n---\nbody\n");
	assert.equal(literal.description, "one\ntwo");
	assert.equal(parseSkillMd("---\nname: 'it''s'\ndescription: d\n---\n").name, "it's");
	assert.throws(() => parseSkillMd("no frontmatter"), /must start/);
	assert.throws(() => parseSkillMd("---\nname: x\n"), /no closing/);
});

test("writeSkills writes one directory per name and reports changes", () => {
	const dir = join(mkdtempSync(join(tmpdir(), "pi-engram-skills-")), "skills");
	const skills = [
		{ name: "review", description: "d", instructions: "project version", space: "attie", files: [{ path: "scripts/x.sh", content: "#!/bin/sh\n", executable: true }] },
		{ name: "review", description: "d", instructions: "global version", space: "global" },
		{ name: "Bad Name", description: "d", instructions: "i" },
	];
	assert.deepEqual(writeSkills(dir, skills), { changed: true, kept: [] });
	assert.match(readFileSync(join(dir, "review", "SKILL.md"), "utf8"), /project version/);
	assert.ok(statSync(join(dir, "review", "scripts", "x.sh")).mode & 0o100);
	assert.deepEqual(cachedSkills(dir).map((k) => [k.name, k.space]), [["review", "attie"]]);
	assert.deepEqual(writeSkills(dir, skills), { changed: false, kept: [] }, "unchanged");
	assert.deepEqual(writeSkills(dir, []).changed, true);
	assert.deepEqual(cachedSkills(dir), []);

	writeSkills(dir, skills);
	const back = readSkillDir(join(dir, "review"));
	assert.equal(back.name, "review");
	assert.deepEqual(back.files, [{ path: "scripts/x.sh", content: "#!/bin/sh\n", executable: true }]);
	writeFileSync(join(dir, "review", "bin.dat"), Buffer.from([1, 0, 2]));
	assert.throws(() => readSkillDir(join(dir, "review")), /isn't text/);
});

test("writeSkills never overwrites a skill changed on disk and not saved", () => {
	const dir = join(mkdtempSync(join(tmpdir(), "pi-engram-skills-")), "skills");
	writeSkills(dir, [{ name: "review", description: "d", instructions: "v1" }, { name: "gone", description: "d", instructions: "x" }]);
	const md = join(dir, "review", "SKILL.md");
	writeFileSync(md, readFileSync(md, "utf8").replace("v1", "my local fix"));
	writeFileSync(join(dir, "gone", "SKILL.md"), readFileSync(join(dir, "gone", "SKILL.md"), "utf8") + "\nlocal note\n");
	assert.deepEqual(dirtySkills(dir), ["gone", "review"]);

	// Engram has a newer version, and "gone" was deleted there: both stay as edited.
	const r = writeSkills(dir, [{ name: "review", description: "d", instructions: "v2 from elsewhere" }]);
	assert.deepEqual(r.kept, ["gone", "review"]);
	assert.match(readFileSync(md, "utf8"), /my local fix/);
	assert.match(readFileSync(join(dir, "gone", "SKILL.md"), "utf8"), /local note/);

	// Once saved, the edit is clean, and the next sync updates it again.
	markSaved(dir, "review", { space: "global" });
	assert.deepEqual(dirtySkills(dir), ["gone"]);
	assert.equal(cachedSkills(dir).find((k) => k.name === "review")?.space, "global");
	writeSkills(dir, [{ name: "review", description: "d", instructions: "v3" }]);
	assert.match(readFileSync(md, "utf8"), /v3/);

	// A local edit that matches what Engram now has is clean.
	writeFileSync(md, readFileSync(md, "utf8").replace("v3", "v4"));
	assert.deepEqual(writeSkills(dir, [{ name: "review", description: "d", instructions: "v4" }]).kept, ["gone"]);
	assert.ok(!dirtySkills(dir).includes("review"));
});

test("withLock keeps two writers apart and clears a stale lock", () => {
	const dir = join(mkdtempSync(join(tmpdir(), "pi-engram-lock-")), "skills");
	const order: string[] = [];
	withLock(dir, () => {
		order.push("outer");
		assert.throws(() => withLock(dir, () => order.push("inner")), /locked by another pi/);
	});
	withLock(dir, () => order.push("after"));
	assert.deepEqual(order, ["outer", "after"]);
});

test("skillDirOf finds the skill a path is in", () => {
	const c = "/c/skills";
	assert.equal(skillDirOf("/c/skills/review/SKILL.md", c), "/c/skills/review");
	assert.equal(skillDirOf("/c/skills/review/scripts/x.sh", c), "/c/skills/review");
	assert.equal(skillDirOf("/c/skills/.fingerprint", c), undefined);
	assert.equal(skillDirOf("/c/other/x", c), undefined);
	assert.equal(skillDirOf("/c/skills/../x/SKILL.md", c), undefined);
	assert.ok(validSkillName("a-b") && !validSkillName("a--b"));
});

test("parseSkillMd folds plain multi-line values and allows comments after quotes, as YAML does", () => {
	const s = parseSkillMd(`---
name: attie-deploy
description: Deploy Attie to pop1,
  which is the whole point
  of this skill.
license: "MIT" # a comment
---
x
`);
	assert.equal(s.description, "Deploy Attie to pop1, which is the whole point of this skill.");
	assert.equal(parseSkillMd('---\nname: "a-b" # c\ndescription: \'it\'\'s\' # c\n---\n').name, "a-b");
	assert.equal(parseSkillMd("---\nname: x\ndescription: 'it''s' # c\n---\n").description, "it's");
});

test("a directory without SKILL.md yet is kept by a sync, and a corrupt index doesn't break it", () => {
	const dir = join(mkdtempSync(join(tmpdir(), "pi-engram-skills-")), "skills");
	writeSkills(dir, []);
	mkdirSync(join(dir, "new-skill"));
	writeFileSync(join(dir, "new-skill", "run.sh"), "echo hi\n");
	assert.deepEqual(writeSkills(dir, []).kept, ["new-skill"]);
	assert.ok(existsSync(join(dir, "new-skill", "run.sh")));
	writeFileSync(join(dir, ".skills.json"), '{"not": "a list"}');
	assert.deepEqual(cachedSkills(dir), []);
	assert.doesNotThrow(() => writeSkills(dir, [{ name: "a", description: "d", instructions: "i" }]));
});

test("replaceSkill leaves a skill with local changes alone; symlinks aren't skill files", async () => {
	const { replaceSkill } = await import("../engram/skills.ts");
	const { symlinkSync } = await import("node:fs");
	const dir = join(mkdtempSync(join(tmpdir(), "pi-engram-skills-")), "skills");
	writeSkills(dir, [{ name: "review", description: "d", instructions: "v1" }]);
	assert.equal(replaceSkill(dir, { name: "review", description: "d", instructions: "v2" }), true);
	const md = join(dir, "review", "SKILL.md");
	writeFileSync(md, readFileSync(md, "utf8").replace("v2", "local"));
	assert.equal(replaceSkill(dir, { name: "review", description: "d", instructions: "v3" }), false);
	assert.match(readFileSync(md, "utf8"), /local/);
	assert.deepEqual(dirtySkills(dir), ["review"]);
	symlinkSync("/etc/hostname", join(dir, "review", "link"));
	assert.throws(() => readSkillDir(join(dir, "review")), /only regular files/);
	assert.equal(parseSkillMd("---\nname: x\ndescription: d\ndisable-model-invocation: True\n---\n").disableModelInvocation, true);
});

test("markSaved leaves a skill unsaved if it changed during the save", () => {
	const dir = join(mkdtempSync(join(tmpdir(), "pi-engram-skills-")), "skills");
	writeSkills(dir, [{ name: "review", description: "d", instructions: "v1" }]);
	const md = join(dir, "review", "SKILL.md");
	writeFileSync(md, readFileSync(md, "utf8").replace("v1", "v2"));
	const before = dirHashOf(join(dir, "review"));
	writeFileSync(md, readFileSync(md, "utf8").replace("v2", "v3")); // landed during the save
	markSaved(dir, "review", { space: "s" }, before);
	assert.deepEqual(dirtySkills(dir), ["review"]);
});

test("frontmatter closes on a line that is exactly ---, and invalid UTF-8 isn't text", () => {
	assert.throws(() => parseSkillMd("---\nname: x\n--- \ndescription: d\n"), /no closing/);
	const s = parseSkillMd("---\nname: x\n----: v\ndescription: d\n---\nbody\n");
	assert.equal(s.description, "d");
	assert.equal(s.instructions, "body\n");
	const dir = join(mkdtempSync(join(tmpdir(), "pi-engram-skills-")), "s");
	mkdirSync(dir);
	writeFileSync(join(dir, "SKILL.md"), "---\nname: s\ndescription: d\n---\nx\n");
	writeFileSync(join(dir, "latin1.txt"), Buffer.from([0x63, 0x61, 0x66, 0xe9]));
	assert.throws(() => readSkillDir(dir), /isn't text/);
});

test("folded values and empty frontmatter parse as Go does; dotfile-only dirs aren't empty", () => {
	assert.equal(parseSkillMd("---\nname: x\ndescription: >\n  one\n  two\n\n  three\n---\n").description, "one two\nthree");
	assert.throws(() => parseSkillMd("---\n---\nbody"), /no closing/);
	const dir = join(mkdtempSync(join(tmpdir(), "pi-engram-skills-")), "skills");
	writeSkills(dir, []);
	mkdirSync(join(dir, "new-skill", "node_modules", "left-pad"), { recursive: true });
	writeFileSync(join(dir, "new-skill", ".env"), "X=1\n");
	writeFileSync(join(dir, "new-skill", "node_modules", "left-pad", "x.js"), "x");
	assert.deepEqual(writeSkills(dir, []).kept, ["new-skill"]);
	assert.ok(existsSync(join(dir, "new-skill", ".env")));
});
