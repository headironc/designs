import { execFile, spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import {
  closeSync,
  constants,
  fstatSync,
  lstatSync,
  mkdirSync,
  mkdtempSync,
  openSync,
  readFileSync,
  renameSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import {
  listSourcePaths,
  openSourceFile,
  openSourceRoot,
  readSourceEntries,
} from "./source.ts";

const repository = path.resolve(import.meta.dirname, "..");
export const lychee = {
  version: "0.24.2",
  url: "https://github.com/lycheeverse/lychee/releases/download/lychee-v0.24.2/lychee-x86_64-unknown-linux-musl.tar.gz",
  archiveSize: 7_705_781,
  archiveHash:
    "73657a111819a30c47c08352896796f23d64e4eb2b3ed39b6d32149241566fc5",
  member: "lychee-x86_64-unknown-linux-musl/lychee",
  binarySize: 19_732_664,
  binaryHash:
    "5c44b077feb43359fb7ad0fd60452e33e1ed8a7bfdbfe6fdad029a020252943b",
  relativePath: ".cache/designs-links/lychee",
} as const;

function hash(bytes: Buffer): string {
  return createHash("sha256").update(bytes).digest("hex");
}

function errorText(error: unknown): string {
  if (error instanceof AggregateError)
    return `${error.message}: ${error.errors.map(errorText).join("; ")}`;
  return error instanceof Error ? error.message : String(error);
}

function platform(): void {
  if (process.platform !== "linux" || process.arch !== "x64")
    throw new Error("Links tooling requires Linux amd64 with /proc/self/fd");
}

// An allowlist keeps tokens, proxies, Git settings and lychee settings out of checks.
function environment(control: string): NodeJS.ProcessEnv {
  return {
    PATH: "/usr/bin:/bin",
    XDG_CONFIG_HOME: control,
    LC_ALL: "C",
  };
}

async function command(binary: string, args: string[], control: string) {
  const result = await new Promise<{
    status: number;
    output: string;
    diagnostic: string;
  }>((resolve, reject) => {
    execFile(
      binary,
      args,
      {
        cwd: control,
        env: environment(control),
        encoding: "utf8",
        timeout: 60_000,
        killSignal: "SIGKILL",
        maxBuffer: 16 * 1024 * 1024,
      },
      (error, output, diagnostic) => {
        if (
          error &&
          (typeof error.code !== "number" || error.signal || error.killed)
        )
          reject(new Error(`Link tool failed: ${error.message}`));
        else
          resolve({
            status: typeof error?.code === "number" ? error.code : 0,
            output,
            diagnostic,
          });
      },
    );
  });
  // Warnings can describe a link or an input which the tool otherwise skips.
  if (result.diagnostic.trim())
    throw new Error(`Link tool diagnostic: ${result.diagnostic.trim()}`);
  return { status: result.status, output: result.output };
}

function binaryBytes(file: string): Buffer {
  platform();
  const root = openSourceRoot(path.dirname(path.resolve(file)));
  try {
    const descriptor = openSourceFile(root, path.basename(file), false);
    try {
      const before = fstatSync(descriptor, { bigint: true });
      if (before.size !== BigInt(lychee.binarySize) || !(before.mode & 0o111n))
        throw new Error("Missing executable mode or wrong lychee size");
      const bytes = readFileSync(descriptor);
      const after = fstatSync(descriptor, { bigint: true });
      if (
        before.mtimeNs !== after.mtimeNs ||
        before.ctimeNs !== after.ctimeNs ||
        hash(bytes) !== lychee.binaryHash
      )
        throw new Error("Wrong or changing lychee binary checksum");
      return bytes;
    } finally {
      closeSync(descriptor);
    }
  } finally {
    closeSync(root);
  }
}

async function installPrivateBinary(
  bytes: Buffer,
  control: string,
): Promise<string> {
  const binary = path.join(control, "lychee");
  writeFileSync(binary, bytes, { flag: "wx", mode: 0o700 });
  const version = await command(binary, ["--version"], control);
  if (
    version.status !== 0 ||
    version.output.trim() !== `lychee ${lychee.version}`
  )
    throw new Error(`Expected lychee ${lychee.version}`);
  return binary;
}

/** Explicit online preparation; publish the verified binary with one atomic rename. */
export async function prepareLinks(
  root = repository,
  request: typeof fetch = fetch,
): Promise<void> {
  platform();
  const source = openSourceRoot(root);
  const held: number[] = [source];
  let parent = source;
  let temporary: string | undefined;
  let staging: number | undefined;
  let failure: unknown;
  try {
    // Create through held directory descriptors, refusing symlinked cache ancestors.
    for (const part of [".cache", "designs-links"]) {
      const target = `/proc/self/fd/${parent}/${part}`;
      try {
        mkdirSync(target, { mode: 0o700 });
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code !== "EEXIST") throw error;
      }
      parent = openSync(
        target,
        constants.O_RDONLY | constants.O_DIRECTORY | constants.O_NOFOLLOW,
      );
      held.push(parent);
    }
    temporary = mkdtempSync(`/proc/self/fd/${parent}/prepare-`);
    staging = openSync(
      temporary,
      constants.O_RDONLY | constants.O_DIRECTORY | constants.O_NOFOLLOW,
    );
    held.push(staging);
    const response = await request(lychee.url, {
      signal: AbortSignal.timeout(120_000),
    });
    if (!response.ok || !response.body)
      throw new Error(`Lychee download failed: HTTP ${response.status}`);
    const chunks: Buffer[] = [];
    let size = 0;
    const reader = response.body.getReader();
    let readFailure: unknown;
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        size += value.byteLength;
        if (size > lychee.archiveSize)
          throw new Error("Lychee archive exceeds pinned size");
        chunks.push(Buffer.from(value));
      }
    } catch (error) {
      readFailure = error;
      throw error;
    } finally {
      try {
        await reader.cancel();
      } catch (error) {
        if (readFailure)
          throw new AggregateError(
            [readFailure, error],
            "Lychee download and cancellation failed",
          );
        throw error;
      }
    }
    const archive = Buffer.concat(chunks);
    if (size !== lychee.archiveSize || hash(archive) !== lychee.archiveHash)
      throw new Error("Wrong lychee archive size or checksum");
    const stagePath = `/proc/self/fd/${staging}`;
    const archivePath = path.join(stagePath, "lychee.tar.gz");
    writeFileSync(archivePath, archive, { flag: "wx", mode: 0o600 });
    // Extract one pinned member to bytes, never unpack archive paths into the filesystem.
    const extracted = spawnSync(
      "/usr/bin/tar",
      [
        "-xzOf",
        archivePath.replace("/proc/self/fd/", `/proc/${process.pid}/fd/`),
        lychee.member,
      ],
      {
        cwd: root,
        env: environment(root),
        timeout: 30_000,
        maxBuffer: lychee.binarySize + 1024,
      },
    );
    if (
      extracted.error ||
      extracted.status !== 0 ||
      extracted.stderr.length ||
      extracted.stdout.length !== lychee.binarySize ||
      hash(extracted.stdout) !== lychee.binaryHash
    )
      throw new Error("Cannot extract the pinned lychee binary");
    // /proc/self/fd is only for the parent: children do not inherit these descriptors.
    await installPrivateBinary(
      extracted.stdout,
      stagePath.replace("/proc/self/fd/", `/proc/${process.pid}/fd/`),
    );
    // Rebind the named install directory before publishing through the held parent.
    const current = openSourceRoot(
      path.join(path.resolve(root), ".cache", "designs-links"),
    );
    try {
      const named = fstatSync(current, { bigint: true });
      const original = fstatSync(parent, { bigint: true });
      if (named.ino !== original.ino || named.dev !== original.dev)
        throw new Error("Tool directory changed during preparation");
    } finally {
      closeSync(current);
    }
    try {
      if (!lstatSync(`/proc/self/fd/${parent}/lychee`).isFile())
        throw new Error("Installed lychee must be a regular file");
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
    }
    const namedStage = lstatSync(temporary, { bigint: true });
    const originalStage = fstatSync(staging, { bigint: true });
    if (
      !namedStage.isDirectory() ||
      namedStage.ino !== originalStage.ino ||
      namedStage.dev !== originalStage.dev
    )
      throw new Error("Tool staging directory changed during preparation");
    renameSync(
      path.join(stagePath, "lychee"),
      `/proc/self/fd/${parent}/lychee`,
    );
    binaryBytes(path.join(root, lychee.relativePath));
  } catch (error) {
    failure = error;
    throw error;
  } finally {
    const cleanupErrors: unknown[] = [];
    if (temporary) {
      try {
        if (staging !== undefined) {
          const named = lstatSync(temporary, { bigint: true });
          const original = fstatSync(staging, { bigint: true });
          if (
            !named.isDirectory() ||
            named.ino !== original.ino ||
            named.dev !== original.dev
          )
            throw new Error(
              "Tool staging path changed; preserve it for recovery",
            );
        }
        rmSync(temporary, { recursive: true, force: true });
      } catch (error) {
        cleanupErrors.push(error);
      }
    }
    for (const descriptor of held.reverse()) {
      try {
        closeSync(descriptor);
      } catch (error) {
        cleanupErrors.push(error);
      }
    }
    if (cleanupErrors.length)
      throw new AggregateError(
        failure ? [failure, ...cleanupErrors] : cleanupErrors,
        "Lychee preparation cleanup failed; original error retained",
      );
  }
}

function lines(output: string): string[] {
  return output.trimEnd() ? output.trimEnd().split("\n") : [];
}

function literalGlob(file: string): string {
  if (/[\x00-\x1f\x7f]/.test(file))
    throw new Error(
      `Unsupported control character in source path: ${JSON.stringify(file)}`,
    );
  return file.replace(
    /[\[\]*?]/g,
    (character) =>
      ({ "[": "[[]", "]": "[]]", "*": "[*]", "?": "[?]" })[character]!,
  );
}

function localTarget(url: string, snapshot: string): string | undefined {
  if (!url.startsWith("file:")) return;
  const target = fileURLToPath(new URL(url));
  const relative = path.relative(snapshot, target);
  if (
    relative === ".." ||
    relative.startsWith(`..${path.sep}`) ||
    path.isAbsolute(relative)
  )
    return target;
}

export interface LinkFailure {
  source: string;
  line?: number;
  target: string;
  reason: string;
}

/** Check one immutable Git source snapshot; success also requires unchanged live source. */
export async function checkLinks(
  root = repository,
  tool = path.join(repository, lychee.relativePath),
) {
  const bytes = binaryBytes(tool);
  const source = openSourceRoot(root);
  let temporary: string | undefined;
  let failure: unknown;
  try {
    temporary = mkdtempSync(path.join(os.tmpdir(), "designs-links-"));
    const snapshot = path.join(temporary, "source");
    const control = path.join(temporary, "control");
    mkdirSync(snapshot, { mode: 0o700 });
    mkdirSync(control, { mode: 0o700 });
    const files = listSourcePaths(root);
    const entries = readSourceEntries(source, files);
    const documents = files.filter((file) => file.endsWith(".md"));
    if (!documents.length) throw new Error("No Markdown source documents");
    for (const entry of entries) {
      const descriptor = openSourceFile(source, entry.path, false);
      try {
        const opened = fstatSync(descriptor, { bigint: true });
        const content = readFileSync(descriptor);
        if (
          opened.ino !== entry.ino ||
          opened.dev !== entry.dev ||
          hash(content) !== entry.sha256
        )
          throw new Error(`Source changed before copying: ${entry.path}`);
        const target = path.join(snapshot, entry.path);
        mkdirSync(path.dirname(target), { recursive: true, mode: 0o700 });
        writeFileSync(target, content, { flag: "wx", mode: 0o600 });
      } finally {
        closeSync(descriptor);
      }
    }
    const copied = openSourceRoot(snapshot);
    try {
      const actual = readSourceEntries(copied, files);
      if (
        actual.some(
          (entry, index) =>
            entry.path !== entries[index]!.path ||
            entry.sha256 !== entries[index]!.sha256,
        )
      )
        throw new Error("Snapshot differs from source entries");
    } finally {
      closeSync(copied);
    }
    const binary = await installPrivateBinary(bytes, control);
    const config = path.join(control, "lychee.toml");
    writeFileSync(config, "", { flag: "wx", mode: 0o600 });
    const args = [
      "--config",
      config,
      "--offline",
      "--include-fragments=anchor-only",
      "--no-progress",
      "--cache=false",
      "--no-ignore",
      "--hidden",
      "--root-dir",
      snapshot,
    ];
    const inputFile = path.join(control, "inputs");
    const inputs = documents.map((file) => path.join(snapshot, file));
    writeFileSync(inputFile, inputs.map(literalGlob).join("\n") + "\n", {
      flag: "wx",
      mode: 0o600,
    });
    const invoke = (mode: string[]) =>
      command(binary, [...args, ...mode, "--files-from", inputFile], control);
    const consumed = await invoke(["--dump-inputs"]);
    if (
      consumed.status !== 0 ||
      JSON.stringify(lines(consumed.output).sort()) !==
        JSON.stringify([...inputs].sort())
    )
      throw new Error("Lychee did not consume the exact Markdown source set");
    const dump = await invoke(["--dump"]);
    if (dump.status !== 0) throw new Error("Lychee link extraction failed");
    const outside = lines(dump.output).filter((url) =>
      localTarget(url, snapshot),
    );
    const failures: LinkFailure[] = [];
    if (outside.length) {
      // Attribute only boundary violations, using the same parser one document at a time.
      for (const [index, input] of inputs.entries()) {
        const parsed = await command(
          binary,
          [...args, "--dump", "--", literalGlob(input)],
          control,
        );
        if (parsed.status !== 0)
          throw new Error("Lychee link attribution failed");
        for (const url of lines(parsed.output)) {
          if (localTarget(url, snapshot))
            failures.push({
              source: documents[index]!,
              target: url,
              reason: "Link escapes the repository source snapshot",
            });
        }
      }
      if (!failures.length)
        throw new Error("Cannot attribute out-of-root links");
    } else {
      const checked = await invoke(["--format", "json"]);
      const report = JSON.parse(checked.output);
      for (const field of [
        "total",
        "successful",
        "errors",
        "excludes",
        "unknown",
        "unsupported",
        "timeouts",
        "cached",
      ]) {
        if (!Number.isSafeInteger(report?.[field]) || report[field] < 0)
          throw new Error(`Invalid lychee report: ${field}`);
      }
      if (
        report.cached ||
        report.unknown ||
        report.unsupported ||
        report.timeouts ||
        !report.error_map ||
        typeof report.error_map !== "object"
      )
        throw new Error("Incomplete lychee report");
      for (const [input, records] of Object.entries(report.error_map)) {
        if (
          !inputs.includes(input) ||
          !Array.isArray(records) ||
          !records.length
        )
          throw new Error("Unknown source in lychee report");
        for (const record of records) {
          if (
            typeof record?.url !== "string" ||
            typeof record?.status?.text !== "string" ||
            !Number.isSafeInteger(record?.span?.line) ||
            record.span.line < 1
          )
            throw new Error("Invalid lychee failure record");
          failures.push({
            source: path.relative(snapshot, input),
            line: record.span.line,
            target: record.url.replaceAll(snapshot, "<repository>"),
            reason: record.status.details ?? record.status.text,
          });
        }
      }
      if (
        report.errors > 0 !== failures.length > 0 ||
        (checked.status === 0) !== (failures.length === 0) ||
        ![0, 2].includes(checked.status)
      )
        throw new Error("Lychee exit and report disagree");
    }
    const current = openSourceRoot(root);
    try {
      const original = fstatSync(source, { bigint: true });
      const named = fstatSync(current, { bigint: true });
      if (
        original.ino !== named.ino ||
        original.dev !== named.dev ||
        JSON.stringify([...listSourcePaths(root)].sort()) !==
          JSON.stringify([...files].sort())
      )
        throw new Error("Source set or root changed during link checking");
      const actual = readSourceEntries(current, files);
      if (
        actual.some(
          (entry, index) =>
            entry.path !== entries[index]!.path ||
            entry.ino !== entries[index]!.ino ||
            entry.dev !== entries[index]!.dev ||
            entry.sha256 !== entries[index]!.sha256,
        )
      )
        throw new Error("Source changed during link checking");
    } finally {
      closeSync(current);
    }
    return { documents: documents.length, failures };
  } catch (error) {
    failure = error;
    throw error;
  } finally {
    const cleanupErrors: unknown[] = [];
    try {
      closeSync(source);
    } catch (error) {
      cleanupErrors.push(error);
    }
    if (temporary) {
      try {
        rmSync(temporary, { recursive: true, force: true });
      } catch (error) {
        cleanupErrors.push(error);
      }
    }
    if (cleanupErrors.length)
      throw new AggregateError(
        failure ? [failure, ...cleanupErrors] : cleanupErrors,
        "Link check cleanup failed; original error retained",
      );
  }
}

if (
  process.argv[1] &&
  path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  try {
    const args = process.argv.slice(2);
    if (args.length === 1 && args[0] === "--prepare") {
      await prepareLinks();
      console.log(
        `Prepared lychee ${lychee.version} at ${lychee.relativePath}`,
      );
    } else {
      if (args.length)
        throw new Error("Usage: node scripts/links.ts [--prepare]");
      const result = await checkLinks();
      for (const failure of result.failures)
        console.error(
          `${failure.source}${failure.line ? `:${failure.line}` : ""}: ${failure.target}: ${failure.reason}`,
        );
      console.log(
        `Checked ${result.documents} Markdown documents for internal links; external URLs excluded (offline)`,
      );
      if (result.failures.length) process.exitCode = 1;
    }
  } catch (error) {
    console.error(
      `${errorText(error)}; run bun run links:prepare for missing or invalid tools`,
    );
    process.exitCode = 2;
  }
}
