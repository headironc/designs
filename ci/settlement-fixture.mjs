import assert from "node:assert/strict";
import childProcess from "node:child_process";
import { syncBuiltinESMExports } from "node:module";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";

// All process operations are replaced before loading the supervisor. No host identity is touched.
const scenario = process.argv[2];
let started = false,
  clock = 0,
  queries = 0;
const calls = [];
Object.defineProperty(performance, "now", { value: () => clock });
Atomics.wait = (_a, _i, _v, ms) => {
  clock += ms;
  return "timed-out";
};
childProcess.spawnSync = (command, args) => {
  calls.push([command, ...args]);
  clock += 10;
  let status = 1,
    stdout = "";
  if (command === "/usr/bin/pgrep") {
    if (started) queries++;
    if (
      started &&
      (scenario === "persistent" || (scenario === "delayed" && queries <= 3))
    ) {
      status = 0;
      stdout = "1234\n";
    }
    if (
      (scenario === "query-before" && !started) ||
      (scenario === "query-after" && started)
    )
      status = 2;
  } else if (command === "/usr/bin/pkill")
    status = scenario === "kill-error" && args[1] === "-U" ? 2 : 0;
  else throw Error("Unexpected process operation");
  return { status, stdout, stderr: "", signal: null };
};
childProcess.spawn = () => {
  started = true;
  const child = new EventEmitter();
  child.stdout = new PassThrough();
  child.stderr = new PassThrough();
  child.kill = () => true;
  queueMicrotask(() => {
    child.emit("spawn");
    child.emit("exit", 0, null);
    child.emit("close", 0, null);
  });
  return child;
};
syncBuiltinESMExports();
process.getuid = () => 0;
process.env.GITHUB_ACTIONS = "true";
process.env.RUNNER_ENVIRONMENT = "github-hosted";
const { runIsolated } = await import("./sandbox.ts");
const run = () =>
  runIsolated(
    "github-hosted",
    { uid: 65534, gid: 65534, group: "nogroup" },
    { argv: ["/bin/true"], cwd: "/tmp", env: {} },
    {},
    100,
    new AbortController().signal,
  );
if (scenario === "query-before") {
  await assert.rejects(run(), /cannot be verified/);
  assert.equal(started, false);
} else {
  const result = await run();
  assert.equal(result.settled, scenario === "delayed");
  if (scenario === "query-after")
    assert.match(result.error, /identity cannot be verified/);
  else if (scenario !== "delayed") assert.match(result.error, /cleanup|query/);
  assert.deepEqual(
    calls.filter((call) => call[0] === "/usr/bin/pkill"),
    [
      ["/usr/bin/pkill", "-KILL", "-u", "65534"],
      ["/usr/bin/pkill", "-KILL", "-U", "65534"],
    ],
  );
}
