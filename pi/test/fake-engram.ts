#!/usr/bin/env node
// A stand-in for the engram CLI's `agent new` in tests: writes an identity
// file and prints it, logging each call's arguments to FAKE_ENGRAM_LOG.
// FAKE_ENGRAM_FAIL makes it fail the way engram does without a principal.

import { appendFileSync, mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";

const args = process.argv.slice(2);
if (process.env.FAKE_ENGRAM_LOG) appendFileSync(process.env.FAKE_ENGRAM_LOG, `${JSON.stringify(args)}\n`);
if (process.env.FAKE_ENGRAM_FAIL) {
	process.stdout.write(`${JSON.stringify({ error: "no principal is set up: run `engram principal login`" })}\n`);
	process.exit(1);
}
if (args[0] !== "agent" || args[1] !== "new") {
	process.stderr.write(`unexpected: ${args.join(" ")}\n`);
	process.exit(2);
}
const dir = process.env.FAKE_ENGRAM_DIR ?? "/tmp";
mkdirSync(dir, { recursive: true });
const n = Date.now().toString(36) + Math.random().toString(36).slice(2, 6);
const did = `did:plc:agent${n}`;
const path = join(dir, `${did}.json`);
writeFileSync(path, JSON.stringify({ did, password: "x", pdsHost: "https://pds.test" }));
process.stdout.write(
	`${JSON.stringify({ did, handle: `a-${n}.pds.test`, path, lounge: "at://did:plc:p/space/garden.engram.lounge/agents", fromStock: true, principal: "did:plc:p" })}\n`,
);
