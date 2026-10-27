import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import {
  appendFileSync,
  chmodSync,
  copyFileSync,
  cpSync,
  mkdirSync,
  readFileSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";
import { unpackShared } from "./bootstrap.mjs";

// A local preparation probe binds real source snapshots; final combination reuses the signed distribution.
const root = "/work",
  policy = path.join(root, "policy"),
  candidate = path.join(root, "candidate"),
  shared = path.join(policy, ".shared");
mkdirSync(root, { recursive: true });
cpSync("/source", policy, {
  recursive: true,
  verbatimSymlinks: true,
  filter: (file) => {
    const first = path.relative("/source", file).split(path.sep)[0];
    return ![".git", "node_modules", ".shared", ".cache"].includes(first);
  },
});
const git = (cwd, ...args) =>
  execFileSync("/usr/bin/git", args, {
    cwd,
    encoding: "utf8",
    env: {
      PATH: "/usr/bin:/bin",
      GIT_CONFIG_GLOBAL: "/dev/null",
      GIT_CONFIG_NOSYSTEM: "1",
    },
  }).trim();
git(policy, "init", "--quiet");
git(policy, "add", ".");
git(
  policy,
  "-c",
  "user.name=Local acceptance",
  "-c",
  "user.email=fixture@example.invalid",
  "commit",
  "--no-gpg-sign",
  "--quiet",
  "-m",
  "local source snapshot",
);
const revision = git(policy, "rev-parse", "HEAD");
const digest = (file) =>
  createHash("sha256").update(readFileSync(file)).digest("hex");
const source = {
  commit: revision,
  tree: git(policy, "rev-parse", "HEAD^{tree}"),
  lockPath: "bun.lock",
  lock: digest(path.join(policy, "bun.lock")),
};
const bundle = path.join(root, "shared.bundle");
git(policy, "bundle", "create", bundle, "HEAD");
const distribution = digest(bundle);
unpackShared(bundle, { ...source, distribution }, shared);
// Real physical copies of the already locked install. Neither source depends on an external symlink.
cpSync("/source/node_modules", path.join(policy, "node_modules"), {
  recursive: true,
  verbatimSymlinks: true,
});
cpSync("/source/node_modules", path.join(shared, "node_modules"), {
  recursive: true,
  verbatimSymlinks: true,
});
git(root, "clone", "--quiet", "--no-hardlinks", policy, candidate);
appendFileSync(path.join(candidate, "README.md"), "\n本地验收候选。\n");
git(candidate, "add", "README.md");
git(
  candidate,
  "-c",
  "user.name=Local acceptance",
  "-c",
  "user.email=fixture@example.invalid",
  "commit",
  "--quiet",
  "--no-gpg-sign",
  "-m",
  "docs fixture",
);
const head = git(candidate, "rev-parse", "HEAD"),
  tree = git(candidate, "rev-parse", "HEAD^{tree}");
const integration = git(
  candidate,
  "-c",
  "user.name=Local acceptance",
  "-c",
  "user.email=fixture@example.invalid",
  "commit-tree",
  tree,
  "-p",
  revision,
  "-p",
  head,
  "-m",
  "local integration",
);
git(candidate, "reset", "--hard", integration);
const toolsRoot = path.join(root, "tools");
mkdirSync(toolsRoot);
for (const file of ["node", "lychee"]) {
  copyFileSync(`/host-tools/${file}`, path.join(toolsRoot, file));
  chmodSync(path.join(toolsRoot, file), 0o755);
}
for (const file of ["check-prose.mjs", "check-ids.mjs"])
  copyFileSync(`/host-docs/${file}`, path.join(toolsRoot, file));
const node = path.join(toolsRoot, "node"),
  lychee = path.join(toolsRoot, "lychee"),
  prose = path.join(toolsRoot, "check-prose.mjs"),
  ids = path.join(toolsRoot, "check-ids.mjs");
assert.equal(
  digest(prose),
  "bc38f628fd431c2639206687e3ca9ec69e2524f4637445c1956df214c59bca59",
);
assert.equal(
  digest(ids),
  "a33c0559ba2cf8bf48ef49492ce312e41a1251327173710cdbe3319751ace7e0",
);
const tools = [
  {
    name: "node",
    executable: node,
    sha256: digest(node),
    versionArgs: ["--version"],
    version: "v26.8.1",
  },
  {
    name: "lychee",
    executable: lychee,
    sha256: digest(lychee),
    versionArgs: ["--version"],
    version: "lychee 0.24.2",
  },
  {
    name: "prose",
    executable: node,
    sha256: digest(node),
    versionArgs: [prose, "--version"],
    version: "3.0.0",
    files: [{ path: prose, sha256: digest(prose) }],
  },
  {
    name: "ids",
    executable: node,
    sha256: digest(node),
    versionArgs: [ids, "--version"],
    version: "1.0.0",
    files: [{ path: ids, sha256: digest(ids) }],
  },
];
const { BoundInputs } = await import(
  pathToFileURL(path.join(shared, "ci/binding.ts")).href
);
const { VerifiedExecution } = await import(
  pathToFileURL(path.join(shared, "ci/execution.ts")).href
);
const { protect } = await import(
  pathToFileURL(path.join(shared, "ci/sandbox.ts")).href
);
protect(toolsRoot);
const { designsChecks, verifyDesigns } = await import(
  pathToFileURL(path.join(policy, "policy/docs.ts")).href
);
// The repository target is real; source commits and provenance remain local test fixtures.
const target = {
  repository: "headironc/designs",
  repositoryId: "1434658318",
  target: "main",
  workflow: ".github/workflows/docs.yml",
};
const inputs = BoundInputs.verify(
  { policy: source, shared: source, distribution },
  { event: "pull_request_target", base: revision, head, integration, tree },
  { policy, shared, candidate, tools },
  bundle,
  target,
);
const reportDirectory = path.join(root, "private");
mkdirSync(reportDirectory, { mode: 0o700 });
const checks = designsChecks(inputs);
const result = await VerifiedExecution.run(inputs, "docs", checks, {
  provenance: { kind: "local-container" },
  preparation: { copies: [], writable: [] },
  reportDirectory,
  signal: AbortSignal.timeout(600_000),
});
const record = {
  kind: "local-development-probe",
  composition: inputs.composition,
  candidate: inputs.candidate,
  paths: inputs.paths,
  report: result.report,
};
writeFileSync(
  "/evidence/designs-probe.json",
  JSON.stringify(record, null, 2) + "\n",
);
verifyDesigns(result, inputs);
console.log(
  JSON.stringify({
    passed: true,
    checks: checks.map((check) => check.name),
    source: revision,
    distribution,
  }),
);
