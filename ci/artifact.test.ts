import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import test from "node:test";
import {
  HostedReport,
  reportBytes,
  validateHostedFacts,
  type HostedExpectation,
} from "./artifact.ts";
import { canonicalDigest } from "./source.ts";
import { prepareArchive } from "./upload.ts";
import type { ExecutionReport } from "./contract.ts";

const revision = "a".repeat(40),
  tree = "b".repeat(40),
  digest = "c".repeat(64);
const sourceBody = {
  version: 1 as const,
  baseline: revision,
  entries: [
    {
      path: "README.md",
      type: "file" as const,
      mode: 0o644,
      bytes: 0,
      sha256: digest,
    },
  ],
};
const source = { ...sourceBody, sha256: canonicalDigest(sourceBody) };
const target = {
  repository: "fixture/designs",
  repositoryId: "1",
  target: "main",
  workflow: ".github/workflows/check.yml",
};
const run = {
  repository: target.repository,
  repositoryId: "1",
  id: "2",
  attempt: "3",
  job: "docs",
  workflowRef: "fixture/designs/.github/workflows/check.yml@refs/heads/main",
  workflowSha: revision,
};
const pin = { commit: revision, tree, lockPath: "bun.lock", lock: digest };
const composition = { policy: pin, shared: pin, distribution: digest };
const candidate = { event: "push" as const, commit: revision, tree };
const tool = {
  name: "node",
  executable: "/tools/node",
  sha256: digest,
  versionArgs: ["--version"],
  version: "v26.8.1",
};
const report: ExecutionReport = {
  version: 1,
  provenance: { kind: "github-hosted", run },
  composition,
  candidate,
  source,
  target,
  changedPaths: null,
  paths: {
    policy: "/policy",
    shared: "/policy/.shared",
    candidate: "/candidate",
    tools: [tool],
  },
  scope: "docs",
  planned: [],
  observations: [],
  errors: [],
};
const expected: HostedExpectation = {
  run,
  target,
  composition,
  candidate,
  source,
  tools: [
    {
      name: tool.name,
      sha256: tool.sha256,
      versionArgs: tool.versionArgs,
      version: tool.version,
    },
  ],
  jobId: "4",
  jobName: "Docs",
  artifactId: "5",
  artifactName: "docs-3",
  workflowBlob: tree,
  headBranch: "main",
};
const actualRun = {
  id: 2,
  run_attempt: 3,
  event: "push",
  path: target.workflow,
  head_sha: revision,
  head_branch: "main",
  status: "completed",
  repository: { id: 1, full_name: target.repository },
};
const actualJob = {
  id: 4,
  run_id: 2,
  head_sha: revision,
  name: "Docs",
  status: "completed",
  conclusion: "success",
};
const artifact = {
  id: 5,
  name: "docs-3",
  size_in_bytes: 100,
  digest: `sha256:${digest}`,
  expired: false,
  workflow_run: { id: 2, repository_id: 1, head_sha: revision },
};
const workflow = { sha: tree, type: "file" };

test("data validation cannot mint artifact provenance", () => {
  assert.deepEqual(
    validateHostedFacts(
      expected,
      report,
      actualRun,
      actualJob,
      artifact,
      workflow,
    ),
    report,
  );
  assert.throws(
    () => HostedReport.requireVerified({ report } as HostedReport),
    /Unverified/,
  );
  assert.throws(
    () => Reflect.construct(HostedReport, [Symbol(), report, {}]),
    /Only the original/,
  );
});

test("original attempt, job, workflow, source and artifact mismatches fail independently", () => {
  for (const changed of [
    { ...actualRun, run_attempt: 4 },
    { ...actualRun, head_sha: "d".repeat(40) },
    { ...actualRun, repository: { id: 9, full_name: target.repository } },
    { ...actualRun, path: ".github/workflows/other.yml" },
  ])
    assert.throws(() =>
      validateHostedFacts(
        expected,
        report,
        changed,
        actualJob,
        artifact,
        workflow,
      ),
    );
  for (const changed of [
    { ...actualJob, id: 40 },
    { ...actualJob, name: "Untrusted" },
    { ...actualJob, conclusion: "cancelled" },
  ])
    assert.throws(() =>
      validateHostedFacts(
        expected,
        report,
        actualRun,
        changed,
        artifact,
        workflow,
      ),
    );
  for (const changed of [
    { ...artifact, id: 6 },
    { ...artifact, expired: true },
    { ...artifact, workflow_run: { ...artifact.workflow_run, id: 8 } },
  ])
    assert.throws(() =>
      validateHostedFacts(
        expected,
        report,
        actualRun,
        actualJob,
        changed,
        workflow,
      ),
    );
  assert.throws(() =>
    validateHostedFacts(expected, report, actualRun, actualJob, artifact, {
      sha: revision,
      type: "file",
    }),
  );
  assert.throws(() =>
    validateHostedFacts(
      expected,
      {
        ...report,
        provenance: {
          kind: "local-container",
          invocation: crypto.randomUUID(),
        },
      },
      actualRun,
      actualJob,
      artifact,
      workflow,
    ),
  );
  assert.throws(() =>
    validateHostedFacts(
      expected,
      {
        ...report,
        composition: { ...composition, distribution: "d".repeat(64) },
      },
      actualRun,
      actualJob,
      artifact,
      workflow,
    ),
  );
});

test("PR run head belongs to H while the fixed workflow independently belongs to B", () => {
  const head = "d".repeat(40),
    integration = "e".repeat(40);
  const pr = {
    event: "pull_request_target" as const,
    base: revision,
    head,
    integration,
    tree,
  };
  const sourceBodyPr = { ...sourceBody, baseline: integration };
  const prSource = { ...sourceBodyPr, sha256: canonicalDigest(sourceBodyPr) };
  const expectedPr = {
    ...expected,
    candidate: pr,
    headBranch: "codex/docs-change",
    source: prSource,
  };
  const reportPr = {
    ...report,
    candidate: pr,
    source: prSource,
    changedPaths: ["README.md"],
  };
  const runPr = {
    ...actualRun,
    event: "pull_request_target",
    head_sha: head,
    head_branch: expectedPr.headBranch,
  };
  const jobPr = { ...actualJob, head_sha: head };
  const artifactPr = {
    ...artifact,
    workflow_run: { ...artifact.workflow_run, head_sha: head },
  };
  assert.deepEqual(
    validateHostedFacts(
      expectedPr,
      reportPr,
      runPr,
      jobPr,
      artifactPr,
      workflow,
    ),
    reportPr,
  );
  assert.throws(() =>
    validateHostedFacts(
      expectedPr,
      reportPr,
      { ...runPr, head_sha: revision },
      jobPr,
      artifactPr,
      workflow,
    ),
  );
  assert.throws(() =>
    validateHostedFacts(
      { ...expectedPr, run: { ...run, workflowSha: head } },
      reportPr,
      runPr,
      jobPr,
      artifactPr,
      workflow,
    ),
  );
});

test("bounded report parser consumes the original upload archive and rejects truncated bytes", async (t) => {
  const directory = mkdtempSync("/tmp/designs-report-");
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  const bytes = Buffer.from(JSON.stringify(report));
  const archive = await prepareArchive(
    bytes,
    directory,
    new AbortController().signal,
  );
  assert.deepEqual(
    await reportBytes(readFileSync(archive.file), new AbortController().signal),
    bytes,
  );
  await assert.rejects(
    reportBytes(Buffer.from("not a ZIP"), new AbortController().signal),
  );
  await assert.rejects(
    reportBytes(
      Buffer.alloc(16 * 1024 * 1024 + 1),
      new AbortController().signal,
    ),
    /16 MiB/,
  );
});
