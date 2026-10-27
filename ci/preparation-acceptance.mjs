import assert from "node:assert/strict";
import {
  existsSync,
  mkdirSync,
  readFileSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { pathToFileURL } from "node:url";
import { requireSupervisor } from "./sandbox.ts";

// Reuse the disposable-container fixture; never invoke this on a persistent host.
requireSupervisor("local-container");
await import("./designs-acceptance.mjs");
const policy = "/work/policy",
  shared = policy + "/.shared",
  candidate = "/work/candidate";
const { BoundInputs, fileDigest, git } = await import(
  shared + "/ci/binding.ts"
);
const { VerifiedExecution, verifyChecks } = await import(
  shared + "/ci/execution.ts"
);
const pin = (directory) => ({
  commit: git(directory, ["rev-parse", "HEAD"]),
  tree: git(directory, ["rev-parse", "HEAD^{tree}"]),
  lockPath: "bun.lock",
  lock: fileDigest(directory + "/bun.lock"),
});
const bundle = "/work/shared.bundle";
const composition = {
  policy: pin(policy),
  shared: pin(shared),
  distribution: fileDigest(bundle),
};
const integration = git(candidate, ["rev-parse", "HEAD"]);
const [base, head] = git(candidate, [
  "show",
  "-s",
  "--format=%P",
  "HEAD",
]).split(" ");
const probe = JSON.parse(readFileSync("/evidence/designs-probe.json", "utf8"));
const inputs = BoundInputs.verify(
  composition,
  {
    event: "pull_request_target",
    base,
    head,
    integration,
    tree: git(candidate, ["rev-parse", "HEAD^{tree}"]),
  },
  { policy, shared, candidate, tools: probe.paths.tools },
  bundle,
  {
    repository: "headironc/designs",
    repositoryId: "1434658318",
    target: "main",
    workflow: ".github/workflows/docs.yml",
  },
);
const checks = [
  {
    name: "Read-only shared candidate input",
    argv: [
      "/work/tools/node",
      "--input-type=module",
      "-e",
      `
    import assert from 'node:assert/strict';
    import {writeFileSync,existsSync} from 'node:fs';
    import {BoundInputs} from '${candidate}/.shared/ci/binding.ts';
    assert.equal(existsSync('${candidate}/.shared/.git'),false);
    assert.throws(()=>writeFileSync('${candidate}/.shared/ci/binding.ts','changed'),{code:'EACCES'});
    assert.throws(()=>BoundInputs.requireVerified({}),/Unverified/);
  `,
    ],
    cwd: policy,
    env: {},
    timeoutMs: 5000,
  },
];
const options = (name) => {
  const reportDirectory = "/work/preparation-" + name;
  mkdirSync(reportDirectory, { mode: 0o700 });
  return {
    provenance: { kind: "local-container" },
    reportDirectory,
    signal: AbortSignal.timeout(15_000),
  };
};
const execution = await VerifiedExecution.run(inputs, "preparation", checks, {
  ...options("positive"),
  preparation: {
    copies: [{ from: shared, to: candidate + "/.shared" }],
    writable: [],
  },
});
writeFileSync(
  "/evidence/preparation-report.json",
  JSON.stringify(execution.report, null, 2) + "\n",
);
verifyChecks(execution, inputs, "preparation", checks);
assert.equal(existsSync(candidate + "/.shared/.git"), false);
assert.equal(
  fileDigest(candidate + "/.shared/ci/binding.ts"),
  fileDigest(shared + "/ci/binding.ts"),
);
const copied = await import(
  pathToFileURL(candidate + "/.shared/ci/binding.ts").href
);
assert.throws(() => copied.BoundInputs.requireVerified(inputs), /Unverified/);
mkdirSync(candidate + "/.cache", { recursive: true });
symlinkSync(candidate + "/.shared", candidate + "/.cache/shared-alias");
const rejected = [];
for (const [name, preparation] of [
  [
    "other-source",
    {
      copies: [{ from: policy + "/node_modules", to: candidate + "/.shared" }],
      writable: [],
    },
  ],
  [
    "partial-shared",
    {
      copies: [{ from: shared + "/ci", to: candidate + "/.shared/ci" }],
      writable: [],
    },
  ],
  [
    "git-metadata",
    {
      copies: [{ from: shared + "/.git", to: candidate + "/.cache/git-copy" }],
      writable: [],
    },
  ],
  [
    "writable-shared",
    { copies: [], writable: [candidate + "/.shared/output"] },
  ],
  [
    "copy-through-alias",
    {
      copies: [
        {
          from: shared + "/ci",
          to: candidate + "/.cache/shared-alias/addition",
        },
      ],
      writable: [],
    },
  ],
  [
    "writable-through-alias",
    { copies: [], writable: [candidate + "/.cache/shared-alias/output"] },
  ],
]) {
  await assert.rejects(
    VerifiedExecution.run(inputs, "preparation", checks, {
      ...options(name),
      preparation,
    }),
    /Unbound|traverses a symlink/,
  );
  rejected.push(name);
}
// A root-prepared but escaping dependency link is still invalid and is never executed.
symlinkSync("/work/tools/node", shared + "/node_modules/escape-probe");
await assert.rejects(
  VerifiedExecution.run(inputs, "preparation", checks, {
    ...options("escaping-link"),
    preparation: { copies: [], writable: [] },
  }),
  /symlink escapes/,
);
rejected.push("escaping-link");
const result = {
  kind: "local-development-probe",
  composition,
  passed: true,
  rejected,
};
writeFileSync(
  "/evidence/preparation-result.json",
  JSON.stringify(result, null, 2) + "\n",
);
console.log(JSON.stringify(result));
