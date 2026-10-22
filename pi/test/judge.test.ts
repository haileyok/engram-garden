import assert from "node:assert/strict";
import { test } from "node:test";
import { consolidatePrompt, filterPrompt, lastExchange, parseDecision, parseFilter } from "../engram/judge.ts";

test("parseFilter keeps ids it was shown, once each", () => {
	const a = 'Sure:\n```json\n{"keep": [{"id": 2, "relevant": "the part"}, {"id": 9, "relevant": "x"}, {"id": 2, "relevant": "dup"}, {"id": "0"}]}\n```';
	assert.deepEqual(parseFilter(a, [0, 1, 2]), [
		{ id: 2, relevant: "the part" },
		{ id: 0, relevant: "" },
	]);
	assert.deepEqual(parseFilter('{"keep": []}', [0]), []);
	assert.throws(() => parseFilter("none of these", [0]), /no JSON/);
	assert.throws(() => parseFilter('{"memories": []}', [0]), /no keep list/);
});

test("parseDecision reads skip, new and merge, and never loses a memory", () => {
	assert.deepEqual(parseDecision('{"action": "skip", "replaces": [1]}', [0, 1]), { action: "skip", existing: [1] });
	assert.deepEqual(parseDecision('{"action": "new"}', [0]), { action: "new" });
	const text = "Attie deploys to pop1 through the Deploy Attie workflow; pop2 deploys are manual (2026-10-21).";
	assert.deepEqual(parseDecision(JSON.stringify({ action: "merge", replaces: [0, 5], text }), [0, 1]), { action: "merge", replaces: [0], text });
	// A merge of nothing it was shown, or without text, stores the memory as new.
	assert.deepEqual(parseDecision(JSON.stringify({ action: "merge", replaces: [7], text }), [0]), { action: "new" });
	assert.deepEqual(parseDecision('{"action": "merge", "replaces": [0], "text": ""}', [0]), { action: "new" });
	assert.throws(() => parseDecision('{"action": "delete"}', [0]), /no action it knows/);
});

test("prompts carry the message, the context and numbered memories", () => {
	const p = filterPrompt("ok do it", "User: deploy attie\nAgent: shall I?", [{ id: 0, text: "Attie deploys...", createdAt: "2026-10-20T00:00:00Z", space: "attie" }]);
	assert.match(p, /Recent conversation, for context:\nUser: deploy attie/);
	assert.match(p, /<memory id="0" date="2026-10-20" space="attie">\nAttie deploys/);
	assert.match(consolidatePrompt("new fact", [{ id: 3, text: "old fact" }], "2026-10-22"), /Today is 2026-10-22[\s\S]*<memory id="3">\nold fact/);
});

test("lastExchange is the agent's last reply and the message before it", () => {
	const msg = (role: string, text: string) => ({ type: "message", message: { role, content: [{ type: "text", text }] } });
	const branch = [msg("user", "old question"), msg("assistant", "old answer"), msg("user", "deploy attie please"), { type: "custom", message: undefined }, msg("assistant", "Shall I run the Deploy Attie workflow?")];
	assert.equal(lastExchange(branch), "User: deploy attie please\nAgent: Shall I run the Deploy Attie workflow?");
	assert.equal(lastExchange([]), "");
	const long = lastExchange([msg("user", "q"), msg("assistant", "x".repeat(5000))], 200);
	assert.ok(long.length < 220 && long.includes("…"));
});

test("keepsFacts and grounded check identifiers", async () => {
	const { identifiers, keepsFacts, grounded } = await import("../engram/judge.ts");
	const old = "Attie pop1 deploys use the Deploy Attie workflow (PR #1201, commit 9ea05ffce) from deploy/.github/workflows/attie.yml on 2026-10-20.";
	assert.ok(identifiers(old).has("#1201") && identifiers(old).has("9ea05ffce") && identifiers(old).has("2026-10-20"));
	assert.ok(keepsFacts(`${old} pop2 uses it too since 2026-10-21.`, old));
	assert.ok(!keepsFacts("Attie deploys use a workflow (PR #1201).", old), "lost the commit, path and date");
	assert.ok(!keepsFacts("x".repeat(30), "plain words only, nothing with digits, but quite a lot of them to keep around"), "far shorter");
	assert.ok(grounded("PR #1201 merged as 9ea05ffce", old));
	assert.ok(!grounded("PR #1302 merged", old));
});

test("keepsFacts guards long memories in proportion", async () => {
	const { keepsFacts } = await import("../engram/judge.ts");
	const old = `Attie runbook (2026-10-01). ${"A reason, a caveat, and some history worth keeping. ".repeat(100)}`;
	assert.ok(!keepsFacts(`Attie runbook (2026-10-01). ${"x".repeat(1300)}`, old));
	assert.ok(keepsFacts(`${old} Updated 2026-10-22.`, old));
});

test("keepsFacts needs the old memory's words, not just its length", async () => {
	const { keepsFacts } = await import("../engram/judge.ts");
	const old = "Hailey prefers short answers and dislikes bulleted summaries at the end of replies.";
	assert.ok(!keepsFacts("The office kitchen restocks coffee beans every Thursday morning at nine.", old));
	assert.ok(keepsFacts(`${old} She also wants file paths shown in full.`, old));
});

test("ungrounded lists identifiers the source never mentions", async () => {
	const { ungrounded } = await import("../engram/judge.ts");
	const src = "ran git log in /home/pelican/dotfiles: 1141528 Bump engram-garden; pi 1.3.0; PR 146 at github.com/x/y/pull/146";
	assert.deepEqual(ungrounded("commit 1141528 in ~/dotfiles, pi v1.3.0, PR #146", src), []);
	assert.deepEqual(ungrounded("commit 6c84547 in ~/dotfiles", src), ["6c84547"]);
});

test("ungrounded matches whole tokens and tolerates rewritten dates, # and home paths", async () => {
	const { ungrounded } = await import("../engram/judge.ts");
	assert.deepEqual(ungrounded("commit 114152", "committed 1141528"), ["114152"]);
	assert.deepEqual(ungrounded("PR #146", "see issue 1464"), ["#146"]);
	assert.deepEqual(ungrounded("Fixed issue #45.", "we fixed issue 45 today"), []);
	assert.deepEqual(ungrounded("deployed on 2026-10-20.", "git log: Date: Mon Oct 20 14:23:45 2026"), []);
	assert.deepEqual(ungrounded("deployed on 2026-10-21.", "git log: Date: Mon Oct 20 14:23:45 2026"), ["2026-10-21"]);
	assert.deepEqual(ungrounded("lives in /home/pelican/dotfiles", "cd ~/dotfiles"), []);
	assert.deepEqual(ungrounded("see github.com/x/y", "https://github.com/x/y/pull/146"), []);
});

test("ungrounded: today's date as the prompt gives it, flags, v both ways, no 'maybe' months", async () => {
	const { ungrounded } = await import("../engram/judge.ts");
	assert.deepEqual(ungrounded("The deploy finished on 2026-10-22, all green.", "Today is 2026-10-22."), []);
	assert.deepEqual(ungrounded("finished on 2026-10-22", "at 2026-10-22T01:23:45.000Z"), []);
	assert.deepEqual(ungrounded("updated with nix flake lock --update-input", "ran nix flake update engram-garden"), ["--update-input"]);
	assert.deepEqual(ungrounded("pi 1.3.0", "upgraded to v1.3.0"), []);
	assert.deepEqual(ungrounded("on 2026-05-21", "maybe 21 of them in 2026"), ["2026-05-21"]);
});

test("sameMemory spots a merge that changed nothing", async () => {
	const { sameMemory } = await import("../engram/judge.ts");
	const old = `Engram Garden prod deploy runbook (2026-10-09): build with git archive HEAD, bump compose.yaml, then docker compose up -d. ${"Verify with docker compose ps and the metrics. ".repeat(10)}`;
	assert.ok(sameMemory(`  ${old.replace(/ /g, "\n")} `, old));
	assert.ok(!sameMemory(old.replace(" and the metrics.", "."), old), "something dropped");
	assert.ok(!sameMemory(`${old} Pop2 uses it since 2026-10-21.`, old), "a new fact got in");
	assert.ok(!sameMemory(old.replace("2026-10-09", "2026-10-22"), old), "the date changed");
	assert.ok(!sameMemory(old.replace("then docker compose up -d", "then do not run docker compose up -d"), old), "a negation got in");
	assert.ok(!sameMemory("Alice manages Bob on the infra team.", "Bob manages Alice on the infra team."), "reordered");
});

test("sameMemory treats a case change as a change", async () => {
	const { sameMemory } = await import("../engram/judge.ts");
	assert.ok(!sameMemory("Config lives in /srv/app.", "Config lives in /srv/App."));
});
