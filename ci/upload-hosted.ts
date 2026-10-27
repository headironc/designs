import { appendFileSync, lstatSync, readFileSync } from "node:fs";
import { mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { pathToFileURL } from "node:url";
import { getBackendIdsFromToken } from "@actions/artifact/lib/internal/shared/util.js";
import { ExecutionReportSchema } from "./contract.ts";
import {
  artifactClient,
  prepareArchive,
  reportBlob,
  uploadContent,
  uploadReport,
  type UploadIdentity,
} from "./upload.ts";

/** Invoked by the fixed P's JavaScript action, which owns the exact artifact name and report operand. */
export async function uploadHosted(
  reportFile: string,
  name: string,
  signal: AbortSignal,
): Promise<{ id: string; sha256: string }> {
  const runtime = JSON.parse(
    readFileSync(path.resolve(import.meta.dirname, "../package.json"), "utf8"),
  ) as { engines: { node: string } };
  if (
    process.versions.node !== runtime.engines.node ||
    process.env.NODE_OPTIONS ||
    process.env.NODE_PATH ||
    process.env.GITHUB_ACTIONS !== "true" ||
    process.env.RUNNER_ENVIRONMENT !== "github-hosted"
  )
    throw new Error("Upload requires the fixed hosted runtime");
  const stat = lstatSync(reportFile);
  if (
    !stat.isFile() ||
    stat.isSymbolicLink() ||
    stat.size > 16 * 1024 * 1024 ||
    stat.mode & 0o022 ||
    (stat.uid !== 0 && stat.uid !== process.getuid?.())
  )
    throw new Error("Report file is not a protected bounded input");
  const bytes = readFileSync(reportFile);
  const report = ExecutionReportSchema.parse(
    JSON.parse(bytes.toString("utf8")),
  );
  if (report.provenance.kind !== "github-hosted")
    throw new Error("Local observations cannot be uploaded as hosted evidence");
  const run = report.provenance.run;
  for (const [field, key] of Object.entries({
    repository: "GITHUB_REPOSITORY",
    repositoryId: "GITHUB_REPOSITORY_ID",
    id: "GITHUB_RUN_ID",
    attempt: "GITHUB_RUN_ATTEMPT",
    job: "GITHUB_JOB",
    workflowRef: "GITHUB_WORKFLOW_REF",
    workflowSha: "GITHUB_WORKFLOW_SHA",
  }))
    if (run[field as keyof typeof run] !== process.env[key])
      throw new Error("Report is not from the original hosted job");
  const token = process.env.ACTIONS_RUNTIME_TOKEN,
    endpoint = process.env.ACTIONS_RESULTS_URL;
  if (!token || !endpoint || !/^[A-Za-z0-9][A-Za-z0-9_.-]+$/.test(name))
    throw new Error("Missing fixed uploader inputs");
  const identity: UploadIdentity = { ...getBackendIdsFromToken(), name };
  const directory = await mkdtemp(
    path.join(os.tmpdir(), "designs-report-upload-"),
  );
  try {
    const archive = await prepareArchive(bytes, directory, signal);
    const observe = (event: object) =>
      console.log(
        JSON.stringify({
          reportUpload: {
            ...event,
            ...identity,
            repository: run.repository,
            run: run.id,
            attempt: run.attempt,
            size: archive.size,
            sha256: archive.sha256,
          },
        }),
      );
    observe({ phase: "prepared", outcome: "complete" });
    const uploaded = await uploadReport(
      identity,
      archive,
      artifactClient(endpoint, token, signal),
      async (url) => uploadContent(archive, reportBlob(url, signal), signal),
      signal,
      observe,
    );
    return { ...uploaded, sha256: archive.sha256 };
  } finally {
    // Archive preparation/content/finalization have already settled their local resources.
    // The original report and phase receipts remain with the workflow when a remote outcome is unknown.
    await rm(directory, { recursive: true, force: true });
  }
}

if (
  process.argv[1] &&
  import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href
) {
  const controller = new AbortController();
  const cancel = () => controller.abort();
  process.on("SIGTERM", cancel);
  process.on("SIGINT", cancel);
  try {
    const [report, name] = process.argv.slice(2);
    if (!report || !name || process.argv.length !== 4)
      throw new Error("Expected fixed report path and artifact name");
    const result = await uploadHosted(report, name, controller.signal);
    if (process.env.GITHUB_OUTPUT)
      appendFileSync(
        process.env.GITHUB_OUTPUT,
        `artifact-id=${result.id}\nartifact-digest=${result.sha256}\n`,
      );
  } catch {
    console.error(
      "Trusted report upload failed; preserve the original phase receipts",
    );
    process.exitCode = 1;
  } finally {
    process.off("SIGTERM", cancel);
    process.off("SIGINT", cancel);
  }
}
