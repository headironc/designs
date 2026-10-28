import { createHash } from "node:crypto";
import { createReadStream, createWriteStream } from "node:fs";
import { chmod, stat } from "node:fs/promises";
import path from "node:path";
import { pipeline, finished } from "node:stream/promises";
import archiver from "archiver";
import { BlockBlobClient, newPipeline } from "@azure/storage-blob";
import { HttpHeaders } from "@azure/core-http";
import { Readable } from "node:stream";
import { ArtifactServiceClientJSON } from "@actions/artifact/lib/generated/results/api/v1/artifact.twirp-client.js";
import { getExpiration } from "@actions/artifact/lib/internal/upload/retention.js";
import type { ListArtifactsResponse_MonolithArtifact } from "@actions/artifact/lib/generated/results/api/v1/artifact.js";
import { reportRequest } from "./http.ts";

export type UploadIdentity = {
  workflowRunBackendId: string;
  workflowJobRunBackendId: string;
  name: string;
};
export type Archive = { file: string; size: number; sha256: string };
type Phase = "prepared" | "create" | "content" | "finalize" | "verified";
type Observation = {
  phase: Phase;
  outcome: "started" | "unknown" | "complete";
  artifactId?: string;
};
type Client = Pick<
  ArtifactServiceClientJSON,
  "CreateArtifact" | "FinalizeArtifact" | "ListArtifacts"
>;

/** Generated codecs own the wire format. This transport owns one HTTP request and its body. */
export function artifactClient(
  endpoint: string,
  token: string,
  signal: AbortSignal,
  send = reportRequest,
): Client {
  const base = new URL(endpoint);
  if (
    base.protocol !== "https:" ||
    base.username ||
    base.password ||
    base.search ||
    base.hash
  )
    throw new Error("Invalid Actions results endpoint");
  return new ArtifactServiceClientJSON({
    async request(service, method, contentType, data) {
      if (
        service !== "github.actions.results.api.v1.ArtifactService" ||
        !["CreateArtifact", "FinalizeArtifact", "ListArtifacts"].includes(
          method,
        ) ||
        contentType !== "application/json"
      )
        throw new Error("Unsupported report operation");
      const response = await send(
        new URL(`/twirp/${service}/${method}`, base),
        "POST",
        { Authorization: `Bearer ${token}`, "Content-Type": contentType },
        JSON.stringify(data),
        signal,
      );
      if (response.status !== 200)
        throw new Error(`Actions report request failed (${response.status})`);
      return JSON.parse(response.body) as object;
    },
  });
}

export function reportBlob(
  url: string,
  signal: AbortSignal,
  send = reportRequest,
) {
  return new BlockBlobClient(
    url,
    newPipeline(undefined, {
      retryOptions: { maxTries: 1 },
      httpClient: {
        async sendRequest(request) {
          const body =
            typeof request.body === "function" ? request.body() : request.body;
          if (!(body instanceof Readable) && typeof body !== "string")
            throw new Error("Unsupported report blob body");
          const response = await send(
            new URL(request.url),
            request.method,
            request.headers.rawHeaders(),
            body,
            signal,
          );
          const headers: Record<string, string> = {};
          for (const [key, value] of Object.entries(response.headers))
            if (value !== undefined)
              headers[key] = Array.isArray(value) ? value.join(",") : value;
          return {
            request,
            status: response.status,
            headers: new HttpHeaders(headers),
            bodyAsText: response.body,
          };
        },
      },
    }),
  );
}

export async function prepareArchive(
  report: Buffer,
  directory: string,
  signal: AbortSignal,
): Promise<Archive> {
  signal.throwIfAborted();
  const file = path.join(directory, "report.zip");
  const zip = archiver("zip", { zlib: { level: 6 } });
  const output = createWriteStream(file, { flags: "wx", mode: 0o600 });
  zip.on("warning", (error) => output.destroy(error));
  zip.on("error", (error) => output.destroy(error));
  const copying = finished(output);
  copying.catch(() => {
    zip.unpipe(output);
    zip.resume();
  });
  // Archiver's finalize promise does not settle when its outer stream is destroyed.
  // Finish this one in-memory entry even after cancellation; no remote write has begun.
  zip.pipe(output);
  zip.append(report, {
    name: "report.json",
    date: new Date("1980-01-01T00:00:00Z"),
    mode: 0o600,
  });
  const results = await Promise.allSettled([copying, zip.finalize()]);
  for (const result of results)
    if (result.status === "rejected") throw result.reason;
  signal.throwIfAborted();
  await chmod(file, 0o400);
  const hash = createHash("sha256");
  await pipeline(createReadStream(file), hash, { signal });
  return { file, size: (await stat(file)).size, sha256: hash.digest("hex") };
}

// The existing SDK's 8 MiB upload block size; Azure permits at most 50,000 blocks.
export const blockBytes = 8 * 1024 * 1024;
export function blockIds(archive: Archive): string[] {
  const count = Math.ceil(archive.size / blockBytes);
  if (
    !Number.isSafeInteger(archive.size) ||
    archive.size <= 0 ||
    count > 50_000 ||
    !/^[a-f0-9]{64}$/.test(archive.sha256)
  )
    throw new Error("Report archive exceeds the block blob protocol bounds");
  return Array.from({ length: count }, (_, index) => {
    const id = Buffer.alloc(36);
    Buffer.from(archive.sha256, "hex").copy(id);
    id.writeUInt32BE(index, 32);
    return id.toString("base64");
  });
}

type Blob = Pick<BlockBlobClient, "upload" | "stageBlock" | "commitBlockList">;
export async function uploadContent(
  archive: Archive,
  blob: Blob,
  signal: AbortSignal,
): Promise<void> {
  const ids = blockIds(archive);
  for (let index = 0; index < ids.length; index++) {
    signal.throwIfAborted();
    const start = index * blockBytes;
    const size = Math.min(blockBytes, archive.size - start);
    const input = createReadStream(archive.file, {
      start,
      end: start + size - 1,
    });
    // Attach completion observation before the client can consume or reject the stream.
    const closed = finished(input).catch(() => undefined);
    let consumed = false;
    const body = () => {
      if (consumed) throw new Error("Report content must not be retransmitted");
      consumed = true;
      return input;
    };
    try {
      if (ids.length === 1) {
        await blob.upload(body, size, {
          abortSignal: signal,
          blobHTTPHeaders: { blobContentType: "application/zip" },
        });
      } else {
        await blob.stageBlock(ids[index]!, body, size, { abortSignal: signal });
      }
    } finally {
      input.destroy();
      await closed;
    }
  }
  if (ids.length > 1) {
    signal.throwIfAborted();
    await blob.commitBlockList(ids, {
      abortSignal: signal,
      blobHTTPHeaders: { blobContentType: "application/zip" },
    });
  }
}

function matching(
  artifacts: ListArtifactsResponse_MonolithArtifact[],
  identity: UploadIdentity,
) {
  // Keep every match, including duplicates. Never use getArtifact's newest-ID behavior.
  return artifacts.filter((artifact) => artifact.name === identity.name);
}

export async function uploadReport(
  identity: UploadIdentity,
  archive: Archive,
  client: Client,
  put: (url: string) => Promise<void>,
  signal: AbortSignal,
  observe: (event: Observation) => void,
): Promise<{ id: string }> {
  blockIds(archive);
  const list = async () =>
    matching((await client.ListArtifacts(identity)).artifacts, identity);
  signal.throwIfAborted();
  if ((await list()).length !== 0)
    throw new Error(
      "The original report name is already occupied; no upload sent",
    );
  let phase: Phase = "prepared";
  let receipt: string | undefined;
  try {
    signal.throwIfAborted();
    phase = "create";
    observe({ phase, outcome: "started" });
    const expiresAt = getExpiration(7);
    const created = await client.CreateArtifact({
      ...identity,
      version: 4,
      ...(expiresAt ? { expiresAt } : {}),
    });
    if (!created.ok || !created.signedUploadUrl)
      throw new Error("Missing report creation receipt");
    const signed = new URL(created.signedUploadUrl);
    if (signed.protocol !== "https:" || signed.username || signed.password)
      throw new Error("Invalid blob upload URL");
    signal.throwIfAborted();
    phase = "content";
    observe({ phase, outcome: "started" });
    await put(signed.href);
    signal.throwIfAborted();
    phase = "finalize";
    observe({ phase, outcome: "started" });
    const result = await client.FinalizeArtifact({
      ...identity,
      size: String(archive.size),
      hash: { value: `sha256:${archive.sha256}` },
    });
    if (!result.ok || !/^[1-9][0-9]*$/.test(result.artifactId))
      throw new Error("Missing report finalization receipt");
    receipt = result.artifactId;
  } catch {
    observe({ phase, outcome: "unknown" });
    // Earlier failures cannot be repaired by finalizing or creating another artifact.
    if (phase !== "finalize" || signal.aborted)
      throw new Error(
        `Report upload outcome unknown at ${phase}; preserve the original run and attempt`,
      );
  }
  try {
    const artifacts = await list();
    const artifact = artifacts[0];
    if (
      artifacts.length !== 1 ||
      !artifact ||
      artifact.workflowRunBackendId !== identity.workflowRunBackendId ||
      artifact.workflowJobRunBackendId !== identity.workflowJobRunBackendId ||
      !/^[1-9][0-9]*$/.test(artifact.databaseId) ||
      (receipt !== undefined && artifact.databaseId !== receipt) ||
      artifact.size !== String(archive.size) ||
      artifact.digest?.value !== `sha256:${archive.sha256}`
    )
      throw new Error(
        "Report artifact is missing, ambiguous or does not match the original upload",
      );
    signal.throwIfAborted();
    if (receipt === undefined) {
      observe({
        phase: "finalize",
        outcome: "unknown",
        artifactId: artifact.databaseId,
      });
      throw new Error(
        "An observed artifact does not settle the original unknown finalization",
      );
    }
    observe({
      phase: "verified",
      outcome: "complete",
      artifactId: artifact.databaseId,
    });
    return { id: artifact.databaseId };
  } catch {
    observe({ phase: "finalize", outcome: "unknown" });
    throw new Error(
      "Report finalization could not be verified; preserve the original run and attempt",
    );
  }
}
