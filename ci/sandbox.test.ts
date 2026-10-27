import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { existsSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { isolatedArguments } from "./sandbox.ts";

test("the actual env boundary strips inherited secrets and startup code", (t) => {
  const root = mkdtempSync(path.join(os.tmpdir(), "designs-env-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const marker = path.join(root, "marker");
  const preload = path.join(root, "preload.cjs");
  writeFileSync(
    preload,
    `require('node:fs').writeFileSync(${JSON.stringify(marker)}, 'loaded')`,
  );
  const argv = isolatedArguments(
    { uid: 65534, gid: 65534, group: "nogroup" },
    {
      argv: [
        process.execPath,
        "-e",
        "if (process.env.GITHUB_TOKEN || process.env.NODE_OPTIONS) process.exit(2); process.exit(19)",
      ],
      cwd: root,
      env: {},
    },
    { PATH: "/usr/bin:/bin", HOME: root },
  );
  const boundary = argv.indexOf("/usr/bin/env");
  const result = spawnSync(argv[boundary]!, argv.slice(boundary + 1), {
    cwd: root,
    env: { GITHUB_TOKEN: "fixture", NODE_OPTIONS: `--require=${preload}` },
    timeout: 10_000,
  });
  assert.equal(result.status, 19);
  assert.equal(existsSync(marker), false);
});

test("supervisor guards enforce every prerequisite in controlled process contexts", () => {
  const result = spawnSync(
    process.execPath,
    [path.join(import.meta.dirname, "supervisor-fixture.mjs")],
    { encoding: "utf8", timeout: 10_000 },
  );
  assert.equal(result.status, 0, result.stderr);
});

for (const scenario of [
  "delayed",
  "persistent",
  "query-before",
  "query-after",
  "kill-error",
  "split-output",
])
  test(`original identity settlement remains explicit: ${scenario}`, () => {
    const result = spawnSync(
      process.execPath,
      [path.join(import.meta.dirname, "settlement-fixture.mjs"), scenario],
      { encoding: "utf8", timeout: 10_000 },
    );
    assert.equal(result.status, 0, result.stderr);
  });
