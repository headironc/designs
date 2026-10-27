import { createHash } from "node:crypto";
import { createRequire } from "node:module";
import { Readable, type Transform } from "node:stream";
import { pipeline } from "node:stream/promises";
import { z } from "zod";
import {
  ExecutionReportSchema,
  type ExecutionReport,
  type Candidate,
  type Composition,
  type PolicyTarget,
  type Run,
  type Tool,
} from "./contract.ts";
import { canonicalDigest, type SourceIdentity } from "./source.ts";
import { reportRequestBytes } from "./http.ts";

const unzip = createRequire(import.meta.url)("unzip-stream") as {
  Parse(): Transform;
};
const maximum = 16 * 1024 * 1024;

/** Decode one bounded data member without extracting paths or executing artifact code. */
export async function reportBytes(
  archive: Buffer,
  signal: AbortSignal,
): Promise<Buffer> {
  if (archive.length > maximum)
    throw new Error("Report archive exceeds 16 MiB");
  const parser = unzip.Parse();
  const chunks: Buffer[] = [];
  let count = 0,
    size = 0;
  const entries: Promise<void>[] = [];
  parser.on("entry", (entry: Readable & { path: string; type: string }) => {
    count++;
    const reading = (async () => {
      if (count !== 1 || entry.path !== "report.json" || entry.type !== "File")
        throw new Error("Expected exactly one regular report.json member");
      for await (const chunk of entry) {
        size += chunk.length;
        if (size > maximum) throw new Error("Expanded report exceeds 16 MiB");
        chunks.push(Buffer.from(chunk));
      }
    })();
    reading.catch((error: Error) => {
      entry.destroy();
      parser.destroy(error);
    });
    entries.push(reading);
  });
  const copying = pipeline(Readable.from(archive), parser, { signal });
  // A parser failure settles every entry before a caller can reuse the original bytes.
  const copyResult = await Promise.allSettled([copying]);
  const entryResults = await Promise.allSettled(entries);
  for (const result of [...copyResult, ...entryResults])
    if (result.status === "rejected") throw result.reason;
  if (count !== 1) throw new Error("Missing report.json");
  return Buffer.concat(chunks);
}

export type HostedExpectation = {
  run: Run;
  target: PolicyTarget;
  composition: Composition;
  candidate: Candidate;
  source: SourceIdentity;
  tools: Omit<Tool, "executable">[];
  jobId: string;
  jobName: string;
  artifactId: string;
  artifactName: string;
  workflowBlob: string;
  headBranch: string;
};

const id = z.number().int().positive().safe();
const RunRecord = z.looseObject({
  id,
  run_attempt: id,
  event: z.string(),
  path: z.string(),
  head_sha: z.string(),
  head_branch: z.string(),
  status: z.literal("completed"),
  repository: z.looseObject({ id, full_name: z.string() }),
});
const JobRecord = z.looseObject({
  id,
  run_id: id,
  head_sha: z.string(),
  name: z.string(),
  status: z.literal("completed"),
  conclusion: z.literal("success"),
});
const ArtifactRecord = z.looseObject({
  id,
  name: z.string(),
  size_in_bytes: id,
  digest: z.string().regex(/^sha256:[a-f0-9]{64}$/),
  expired: z.literal(false),
  workflow_run: z.looseObject({ id, repository_id: id, head_sha: z.string() }),
});

/** Validation alone grants no provenance. Only readHosted fetches these facts from GitHub. */
export function validateHostedFacts(
  expected: HostedExpectation,
  reportInput: unknown,
  runInput: unknown,
  jobInput: unknown,
  artifactInput: unknown,
  workflowInput: unknown,
): ExecutionReport {
  const report = ExecutionReportSchema.parse(reportInput);
  const run = RunRecord.parse(runInput),
    job = JobRecord.parse(jobInput),
    artifact = ArtifactRecord.parse(artifactInput);
  const workflow = z
    .looseObject({ sha: z.string(), type: z.literal("file") })
    .parse(workflowInput);
  const executionHead =
    expected.candidate.event === "pull_request_target"
      ? expected.candidate.head
      : expected.candidate.commit;
  const workflowBase =
    expected.candidate.event === "pull_request_target"
      ? expected.candidate.base
      : expected.candidate.commit;
  if (
    String(run.id) !== expected.run.id ||
    String(run.run_attempt) !== expected.run.attempt ||
    run.repository.full_name !== expected.target.repository ||
    String(run.repository.id) !== expected.target.repositoryId ||
    run.path !== expected.target.workflow ||
    run.head_sha !== executionHead ||
    expected.run.workflowSha !== workflowBase ||
    run.head_branch !== expected.headBranch ||
    run.event !== expected.candidate.event ||
    String(job.id) !== expected.jobId ||
    String(job.run_id) !== expected.run.id ||
    job.head_sha !== run.head_sha ||
    job.name !== expected.jobName ||
    String(artifact.id) !== expected.artifactId ||
    artifact.name !== expected.artifactName ||
    String(artifact.workflow_run.id) !== expected.run.id ||
    String(artifact.workflow_run.repository_id) !==
      expected.target.repositoryId ||
    artifact.workflow_run.head_sha !== run.head_sha ||
    workflow.sha !== expected.workflowBlob
  )
    throw new Error(
      "Platform producer, attempt, job, artifact or workflow mismatch",
    );
  if (
    report.provenance.kind !== "github-hosted" ||
    canonicalDigest(report.provenance.run) !== canonicalDigest(expected.run) ||
    canonicalDigest(report.composition) !==
      canonicalDigest(expected.composition) ||
    canonicalDigest(report.candidate) !== canonicalDigest(expected.candidate) ||
    canonicalDigest(report.target) !== canonicalDigest(expected.target) ||
    canonicalDigest(report.source) !== canonicalDigest(expected.source) ||
    canonicalDigest(
      report.paths.tools.map(({ executable: _path, ...tool }) => tool),
    ) !== canonicalDigest(expected.tools)
  )
    throw new Error(
      "Report does not belong to the designated execution inputs",
    );
  return report;
}

const originals = new WeakSet<object>();
const originalToken = Symbol("authenticated artifact");
export class HostedReport {
  readonly report: ExecutionReport;
  readonly artifact: { id: string; sha256: string; size: number };
  private constructor(
    token: symbol,
    report: ExecutionReport,
    artifact: HostedReport["artifact"],
  ) {
    if (token !== originalToken)
      throw new Error(
        "Only the original platform reader can authenticate a report",
      );
    this.report = deepFreeze(report);
    this.artifact = deepFreeze(artifact);
    originals.add(this);
    Object.freeze(this);
  }
  static requireVerified(value: HostedReport): void {
    if (!originals.has(value))
      throw new Error("Unverified artifact provenance");
  }

  static async read(
    expected: HostedExpectation,
    token: string,
    signal: AbortSignal,
  ): Promise<HostedReport> {
    if (
      !token ||
      !/^[\w.-]+\/[\w.-]+$/.test(expected.target.repository) ||
      [
        expected.run.id,
        expected.run.attempt,
        expected.jobId,
        expected.artifactId,
      ].some((value) => !/^[1-9][0-9]*$/.test(value)) ||
      !/^[a-f0-9]{40}$/.test(expected.workflowBlob)
    )
      throw new Error("Invalid independent platform expectation");
    const headers = {
      Authorization: `Bearer ${token}`,
      Accept: "application/vnd.github+json",
      "User-Agent": "designs-ci",
      "X-GitHub-Api-Version": "2026-03-10",
    };
    const root = `https://api.github.com/repos/${expected.target.repository}`;
    const json = async (endpoint: string) => {
      const response = await reportRequestBytes(
        new URL(root + endpoint),
        "GET",
        headers,
        "",
        signal,
      );
      if (response.status !== 200)
        throw new Error(`Original platform read failed (${response.status})`);
      return JSON.parse(response.body.toString("utf8")) as unknown;
    };
    const run = await json(
      `/actions/runs/${expected.run.id}/attempts/${expected.run.attempt}`,
    );
    let selected: unknown;
    for (let page = 1; ; page++) {
      const listing = z
        .looseObject({
          total_count: z.number().int().nonnegative(),
          jobs: z.array(z.looseObject({ id })),
        })
        .parse(
          await json(
            `/actions/runs/${expected.run.id}/attempts/${expected.run.attempt}/jobs?per_page=100&page=${page}`,
          ),
        );
      for (const job of listing.jobs)
        if (String(job.id) === expected.jobId) {
          if (selected) throw new Error("Duplicate original job");
          selected = job;
        }
      if (page * 100 >= listing.total_count) break;
      if (listing.jobs.length !== 100)
        throw new Error("Incomplete original job listing");
    }
    if (!selected) throw new Error("Job is absent from the original attempt");
    const artifact = ArtifactRecord.parse(
      await json(`/actions/artifacts/${expected.artifactId}`),
    );
    const workflow = await json(
      `/contents/${expected.target.workflow}?ref=${expected.run.workflowSha}`,
    );
    const redirect = await reportRequestBytes(
      new URL(`${root}/actions/artifacts/${expected.artifactId}/zip`),
      "GET",
      headers,
      "",
      signal,
    );
    if (
      redirect.status !== 302 ||
      typeof redirect.headers.location !== "string"
    )
      throw new Error("Missing original artifact download location");
    const location = new URL(redirect.headers.location);
    if (
      location.protocol !== "https:" ||
      location.username ||
      location.password ||
      !(
        location.hostname.endsWith(".blob.core.windows.net") ||
        location.hostname.endsWith(".actions.githubusercontent.com")
      )
    )
      throw new Error("Unexpected artifact storage origin");
    // The signed storage URL receives no GitHub credential. Never follow another redirect.
    const archive = await reportRequestBytes(location, "GET", {}, "", signal);
    const digest = createHash("sha256").update(archive.body).digest("hex");
    if (
      archive.status !== 200 ||
      archive.body.length !== artifact.size_in_bytes ||
      `sha256:${digest}` !== artifact.digest
    )
      throw new Error(
        "Artifact bytes differ from the original platform digest",
      );
    const report = JSON.parse(
      (await reportBytes(archive.body, signal)).toString("utf8"),
    ) as unknown;
    const verified = validateHostedFacts(
      expected,
      report,
      run,
      selected,
      artifact,
      workflow,
    );
    return new HostedReport(originalToken, verified, {
      id: expected.artifactId,
      sha256: digest,
      size: archive.body.length,
    });
  }
}

function deepFreeze<T>(value: T): T {
  if (value && typeof value === "object") {
    for (const item of Object.values(value)) deepFreeze(item);
    Object.freeze(value);
  }
  return value;
}
