import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdirSync, writeFileSync } from "node:fs";
import path from "node:path";
import { requireSupervisor } from "./sandbox.ts";

// Only a disposable container may run the real supervisor and candidate fixtures.
requireSupervisor("local-container");
const { inputs } = await import("./designs-acceptance.mjs");
const { BoundInputs } = await import(inputs.paths.shared + "/ci/binding.ts");
const { VerifiedExecution } = await import(
  inputs.paths.shared + "/ci/execution.ts"
);
const { designsChecks, verifyDesigns } = await import(
  inputs.paths.policy + "/policy/checks.ts"
);
const { formatFiles } = await import(inputs.paths.policy + "/policy/format.ts");
const git = (cwd, ...args) =>
  execFileSync(
    "/usr/bin/git",
    [
      "-c",
      "user.name=Format fixture",
      "-c",
      "user.email=fixture@example.invalid",
      "-c",
      "commit.gpgsign=false",
      ...args,
    ],
    {
      cwd,
      encoding: "utf8",
      env: {
        PATH: "/usr/bin:/bin",
        GIT_CONFIG_GLOBAL: "/dev/null",
        GIT_CONFIG_NOSYSTEM: "1",
      },
    },
  ).trim();

const records = [];
const missingTool = BoundInputs.verify(
  inputs.composition,
  inputs.candidate,
  {
    ...inputs.paths,
    tools: inputs.paths.tools.filter((tool) => tool.name !== "go-format"),
  },
  process.argv[2],
  inputs.target,
);
assert.throws(
  () => designsChecks(missingTool),
  /Missing pinned designs format tools/,
);
for (const repaired of [false, true]) {
  const candidate = `/work/format-${repaired ? "repaired" : "invalid"}`;
  git(
    "/work",
    "clone",
    "--quiet",
    "--no-hardlinks",
    inputs.paths.candidate,
    candidate,
  );
  const file = "format-negative.ts";
  writeFileSync(path.join(candidate, file), "const value={x:1}\n");
  writeFileSync(path.join(candidate, ".prettierignore"), "*\n");
  if (repaired) await formatFiles(candidate, [file], "/work/tools/gofmt", true);
  git(candidate, "add", ".");
  git(candidate, "commit", "--quiet", "-m", "format fixture");
  const head = git(candidate, "rev-parse", "HEAD");
  const tree = git(candidate, "rev-parse", "HEAD^{tree}");
  const integration = git(
    candidate,
    "commit-tree",
    tree,
    "-p",
    inputs.candidate.base,
    "-p",
    head,
    "-m",
    "format integration",
  );
  git(candidate, "reset", "--hard", integration);
  const fixture = BoundInputs.verify(
    inputs.composition,
    {
      event: "pull_request_target",
      base: inputs.candidate.base,
      head,
      integration,
      tree,
    },
    { ...inputs.paths, candidate },
    process.argv[2],
    inputs.target,
  );
  const checks = designsChecks(fixture);
  const reportDirectory = candidate + "-report";
  mkdirSync(reportDirectory, { mode: 0o700 });
  const execution = await VerifiedExecution.run(
    fixture,
    "maintenance",
    checks,
    {
      provenance: { kind: "local-container" },
      preparation: { copies: [], writable: [] },
      reportDirectory,
      signal: AbortSignal.timeout(600_000),
    },
  );
  writeFileSync(
    `/evidence/format-${repaired ? "repaired" : "invalid"}.json`,
    JSON.stringify(execution.report, null, 2) + "\n",
  );
  const format = execution.report.observations.find(
    (item) => item.request.name === "format.sources",
  );
  assert.equal(format.started, true);
  assert.equal(format.settled, true);
  assert.equal(format.exitCode, repaired ? 0 : 1);
  if (repaired) verifyDesigns(execution, fixture);
  else assert.throws(() => verifyDesigns(execution, fixture));
  records.push({
    repaired,
    candidate: fixture.candidate,
    formatExitCode: format.exitCode,
  });
}
writeFileSync(
  "/evidence/format-result.json",
  JSON.stringify({ passed: true, records }, null, 2) + "\n",
);
console.log("Format failure and repair accepted against fixed P");
