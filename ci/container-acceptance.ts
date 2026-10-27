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
    failure: "process.exit(23)",
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
  if (scenario === "failure") assert.equal(result.status, 23);
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
