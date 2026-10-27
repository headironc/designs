import assert from "node:assert/strict";
import fs from "node:fs";
import { syncBuiltinESMExports } from "node:module";
import { requireSupervisor } from "./sandbox.ts";

// Only the guard is called. All context reads are controlled; no UID or process is changed.
let uid = 0;
let docker = true;
let init = "docker-init\n";
process.getuid = () => uid;
Object.defineProperty(process, "platform", { value: "linux" });
fs.existsSync = (file) => {
  assert.equal(file, "/.dockerenv");
  return docker;
};
fs.readFileSync = (file, encoding) => {
  assert.equal(file, "/proc/1/comm");
  assert.equal(encoding, "utf8");
  return init;
};
syncBuiltinESMExports();
process.env.GITHUB_ACTIONS = "true";
process.env.RUNNER_ENVIRONMENT = "github-hosted";

requireSupervisor("github-hosted");
requireSupervisor("local-container");
uid = 1000;
assert.throws(() => requireSupervisor("github-hosted"), /disposable hosted/);
assert.throws(() => requireSupervisor("local-container"), /disposable Docker/);
uid = 0;
process.env.GITHUB_ACTIONS = "false";
assert.throws(() => requireSupervisor("github-hosted"), /disposable hosted/);
process.env.GITHUB_ACTIONS = "true";
process.env.RUNNER_ENVIRONMENT = "self-hosted";
assert.throws(() => requireSupervisor("github-hosted"), /disposable hosted/);
docker = false;
assert.throws(() => requireSupervisor("local-container"), /disposable Docker/);
docker = true;
init = "systemd\n";
assert.throws(() => requireSupervisor("local-container"), /disposable Docker/);
init = "docker-init\n";
Object.defineProperty(process, "platform", { value: "darwin" });
assert.throws(() => requireSupervisor("local-container"), /disposable Docker/);
