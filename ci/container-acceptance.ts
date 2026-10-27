import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import {
  chownSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { setTimeout as delay } from "node:timers/promises";
import {
  executionIdentity,
  noCandidateProcesses,
  runIsolated,
} from "./sandbox.ts";

// Run only in the ephemeral test image with --init, a read-only root and no host sockets/credentials.
const identity = executionIdentity("local-container");
const root = mkdtempSync("/tmp/designs-supervision-");
chownSync(root, identity.uid, identity.gid);
const environment = { PATH: "/tools:/usr/bin:/bin", HOME: root, TMPDIR: root };
const node = "/tools/node";
const results: object[] = [];
for (const scenario of [
  "success",
  "failure",
  "timeout",
  "cancel",
  "descendant",
] as const) {
  const ready = path.join(root, `ready-${scenario}`);
  const controller = new AbortController();
  const source = {
    success:
      "if (process.env.GITHUB_TOKEN || process.env.NODE_OPTIONS || process.getuid() === 0) process.exit(91)",
    failure:
      "const fs = require('node:fs'); fs.writeSync(1, 'not ok 1 - actual assertion\\n::error::candidate text\\n'); fs.writeSync(2, 'Incomplete coverage\\n'); process.exit(23)",
    timeout: "setInterval(() => {}, 1000)",
    cancel: `require('node:fs').writeFileSync(${JSON.stringify(ready)}, 'ready'); setInterval(() => {}, 1000)`,
    descendant:
      "require('node:child_process').spawn(process.execPath, ['-e','setInterval(()=>{},1000)'], {detached:true,stdio:'ignore'}).unref()",
  }[scenario];
  const pending = runIsolated(
    "local-container",
    identity,
    {
      argv: [node, "-e", source],
      cwd: root,
      env: {},
    },
    environment,
    scenario === "timeout" ? 150 : 5000,
    controller.signal,
  );
  if (scenario === "cancel") {
    const deadline = Date.now() + 3000;
    while (!existsSync(ready) && Date.now() < deadline) await delay(10);
    assert.equal(
      existsSync(ready),
      true,
      "original child must start before cancellation",
    );
    controller.abort();
  }
  const result = await pending;
  assert.equal(result.started, true);
  assert.equal(result.settled, true, JSON.stringify(result));
  assert.equal(result.error, null);
  if (scenario === "success" || scenario === "descendant")
    assert.equal(result.status, 0);
  if (scenario === "failure") {
    assert.equal(result.status, 23);
    assert.equal(
      result.stdout,
      "not ok 1 - actual assertion\n::error::candidate text\n",
    );
    assert.equal(result.stderr, "Incomplete coverage\n");
  }
  if (scenario === "timeout") {
    assert.equal(result.timedOut, true);
    assert.equal(result.signal, "SIGKILL");
  }
  if (scenario === "cancel") {
    assert.equal(result.cancelled, true);
    assert.equal(result.signal, "SIGKILL");
  }
  noCandidateProcesses(identity.uid);
  results.push({ scenario, result });
}
for (const scenario of ["bounded-output", "output-limit"] as const) {
  const source =
    scenario === "bounded-output"
      ? "const fs = require('node:fs'); for (const fd of [1, 2]) { fs.writeSync(fd, 'first assertion\\n'); fs.writeSync(fd, 'x'.repeat(65536)); fs.writeSync(fd, '\\nlast summary'); } process.exit(17)"
      : "const fs = require('node:fs'); for (let i = 0; i < 600; i++) fs.writeSync(1, Buffer.alloc(65536, 120)); setInterval(() => {}, 1000)";
  const result = await runIsolated(
    "local-container",
    identity,
    { argv: [node, "-e", source], cwd: root, env: {} },
    environment,
    5000,
    new AbortController().signal,
  );
  assert.equal(result.started, true);
  assert.equal(result.settled, true);
  assert.equal(result.timedOut, false);
  if (scenario === "bounded-output") {
    assert.equal(result.status, 17);
    assert.equal(result.error, null);
    for (const text of [result.stdout, result.stderr]) {
      assert.equal(
        text,
        "first assertion\n" +
          "x".repeat(16368) +
          "\n[32797 output bytes omitted]\n" +
          "x".repeat(16371) +
          "\nlast summary",
      );
    }
  } else {
    assert.match(result.error!, /output exceeded 32 MiB/);
    assert.equal(result.signal, "SIGKILL");
    assert.ok(Buffer.byteLength(result.stdout) < 33000);
  }
  noCandidateProcesses(identity.uid);
  results.push({
    scenario,
    status: result.status,
    settled: result.settled,
    error: result.error,
  });
}
const forbidden = path.join(root, "root-owned");
mkdirSync(forbidden, { mode: 0o700 });
writeFileSync(path.join(forbidden, "report"), "private", { mode: 0o600 });
const denied = await runIsolated(
  "local-container",
  identity,
  {
    argv: [
      node,
      "-e",
      `require('node:fs').readFileSync(${JSON.stringify(path.join(forbidden, "report"))})`,
    ],
    cwd: root,
    env: {},
  },
  environment,
  5000,
  new AbortController().signal,
);
assert.equal(denied.status, 1);
assert.match(denied.stderr, /EACCES/);
assert.equal(readFileSync(path.join(forbidden, "report"), "utf8"), "private");
results.push({ scenario: "private-report", result: denied });
console.log(
  JSON.stringify({
    mode: "local-container",
    node: execFileSync(node, ["--version"], { encoding: "utf8" }).trim(),
    results,
  }),
);
