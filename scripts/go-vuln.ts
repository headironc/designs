import { execFileSync } from "node:child_process";
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
  realpathSync,
  renameSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { CommandFailure, runCommand } from "./go-check.ts";
import {
  evaluateLXD,
  inspectPackages,
  parseScan,
  runnerInputHash,
  verifyNativeStatus,
  type ScanIdentity,
} from "./go-vuln-report.ts";
import {
  listSourcePaths,
  openSourceFile,
  openSourceRoot,
  readSourceEntries,
} from "./source.ts";

const repository = path.resolve(import.meta.dirname, "..");
const scanInputs = ["scripts/go-vuln-json.go", "scripts/go-vuln-lxd.json"];
const isScanInput = (file: string) =>
  file.startsWith("runner/") || scanInputs.includes(file);
export const vulnTool = {
  version: "v1.9.0",
  goVersion: "1.27.3",
  module: "golang.org/x/vuln/cmd/govulncheck@v1.9.0",
  hash: "d36c5d591f43f085b4561a3a9db05894d343a86376f20fab7d8c4ffff38b7a01",
  cache: ".cache/designs-go-vuln",
  database: "https://vuln.go.dev",
} as const;
const digest = (bytes: Buffer) =>
  createHash("sha256").update(bytes).digest("hex");
export function describeFailure(error: unknown): string {
  return error instanceof AggregateError
    ? `${error.message}: ${error.errors.map(describeFailure).join("; ")}`
    : error instanceof Error
      ? error.message
      : String(error);
}

function sdk(bin = process.env.DESIGNS_GO_BIN): string {
  const candidates = bin
    ? [bin]
    : (process.env.PATH ?? "").split(path.delimiter).filter(Boolean);
  for (const directory of candidates) {
    const go = path.resolve(directory, "go");
    try {
      const resolved = realpathSync(go);
      const version = execFileSync(resolved, ["version"], {
        encoding: "utf8",
        timeout: 10_000,
        env: {
          PATH: "/usr/bin:/bin",
          GOENV: "off",
          GOWORK: "off",
          GOTOOLCHAIN: "local",
        },
      }).trim();
      if (version !== `go version go${vulnTool.goVersion} linux/amd64`)
        throw new Error(
          `Expected complete Go ${vulnTool.goVersion} linux/amd64 SDK: ${version}`,
        );
      return resolved;
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
    }
  }
  throw new Error(
    `Install complete Go ${vulnTool.goVersion}; set DESIGNS_GO_BIN or PATH`,
  );
}

// All ambient proxy, token, Go and runner fixture settings are excluded.
export function vulnerabilityEnvironment(
  go: string,
  cache: string,
  control: string,
  online: boolean,
): NodeJS.ProcessEnv {
  return {
    PATH: `${path.dirname(go)}:/usr/bin:/bin`,
    LC_ALL: "C",
    GOENV: "off",
    GOWORK: "off",
    GOTOOLCHAIN: "local",
    GOFLAGS: "-mod=readonly",
    GOOS: "linux",
    GOARCH: "amd64",
    GOAMD64: "v1",
    CGO_ENABLED: "1",
    GOPATH: path.join(cache, "gopath"),
    GOMODCACHE: path.join(cache, "mod"),
    GOCACHE: path.join(cache, "build"),
    GOTMPDIR: path.join(control, "tmp"),
    XDG_CONFIG_HOME: path.join(control, "config"),
    GOPROXY: online ? "https://proxy.golang.org" : "off",
    GOSUMDB: online ? "sum.golang.org" : "off",
  };
}

function sameDirectory(named: number, held: number): void {
  const a = fstatSync(named, { bigint: true }),
    b = fstatSync(held, { bigint: true });
  if (a.ino !== b.ino || a.dev !== b.dev)
    throw new Error("Source or cache directory changed");
}

function pinnedBytes(root: number, file: string): Buffer {
  const descriptor = openSourceFile(root, file, false);
  try {
    const before = fstatSync(descriptor, { bigint: true });
    const bytes = readFileSync(descriptor);
    const after = fstatSync(descriptor, { bigint: true });
    if (
      !(before.mode & 0o111n) ||
      before.ctimeNs !== after.ctimeNs ||
      before.mtimeNs !== after.mtimeNs ||
      digest(bytes) !== vulnTool.hash
    )
      throw new Error(
        "Missing executable mode or wrong/changing govulncheck checksum; run go:vuln:prepare",
      );
    return bytes;
  } finally {
    closeSync(descriptor);
  }
}

/** The CLI always uses the official database. A local OSV database is injectable by engine acceptance tests. */
export async function checkVulnerabilities(
  options: {
    root?: string;
    prepare?: boolean;
    goBin?: string;
    signal?: AbortSignal;
    database?: string;
  } = {},
): Promise<{
  findings: boolean;
  blocked: boolean;
  summary: string;
  report: string;
}> {
  if (process.platform !== "linux" || process.arch !== "x64")
    throw new Error("Go vulnerability checks require Linux amd64");
  const root = path.resolve(options.root ?? repository);
  const go = sdk(options.goBin);
  const source = openSourceRoot(root);
  const held = [source];
  const failures: unknown[] = [];
  let run: string | undefined;
  let snapshot: string | undefined;
  let control: string | undefined;
  let preserve = false;
  let findings = false;
  let blocked = false;
  let summary = "Go vulnerability dependencies prepared";
  let entries: ReturnType<typeof readSourceEntries> | undefined;
  let files: string[] = [];
  let cacheDescriptor: number | undefined;
  let runDescriptor: number | undefined;
  const cache = path.join(root, vulnTool.cache);
  const directoryFlags =
    constants.O_RDONLY | constants.O_DIRECTORY | constants.O_NOFOLLOW;
  const makeDirectory = (parent: number, name: string): number => {
    const target = `/proc/self/fd/${parent}/${name}`;
    try {
      mkdirSync(target, { mode: 0o700 });
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "EEXIST") throw error;
    }
    const descriptor = openSync(target, directoryFlags);
    held.push(descriptor);
    return descriptor;
  };
  try {
    cacheDescriptor = makeDirectory(
      makeDirectory(source, ".cache"),
      "designs-go-vuln",
    );
    for (const name of ["mod", "build", "gopath"])
      makeDirectory(cacheDescriptor, name);
    // Retain logs, but delete source/private executable only after all children are closed.
    const runPath = mkdtempSync(`/proc/self/fd/${cacheDescriptor}/run-`);
    run = path.join(cache, path.basename(runPath));
    runDescriptor = openSync(runPath, directoryFlags);
    held.push(runDescriptor);
    const childRun = `/proc/${process.pid}/fd/${runDescriptor}`;
    snapshot = path.join(childRun, "source");
    control = path.join(childRun, "control");
    mkdirSync(snapshot, { mode: 0o700 });
    mkdirSync(control, { mode: 0o700 });
    mkdirSync(path.join(control, "tmp"));
    mkdirSync(path.join(control, "bin"));
    mkdirSync(path.join(control, "config/go/telemetry"), { recursive: true });
    writeFileSync(
      path.join(control, "config/go/telemetry/mode"),
      "off 2026-10-29",
      { flag: "wx", mode: 0o600 },
    );
    files = listSourcePaths(root).filter(isScanInput);
    if (!files.includes("runner/go.mod") || !files.includes("runner/go.sum"))
      throw new Error("Missing runner module locks in Git source");
    for (const file of scanInputs)
      if (!files.includes(file))
        throw new Error(`Missing scanner input: ${file}`);
    entries = readSourceEntries(source, files);
    writeFileSync(
      path.join(childRun, "source-manifest.json"),
      JSON.stringify(
        entries.map(({ path, sha256 }) => ({ path, sha256 })),
        null,
        2,
      ) + "\n",
      { flag: "wx", mode: 0o600 },
    );
    for (const entry of entries) {
      const descriptor = openSourceFile(source, entry.path, false);
      try {
        const stat = fstatSync(descriptor, { bigint: true });
        const bytes = readFileSync(descriptor);
        if (
          stat.ino !== entry.ino ||
          stat.dev !== entry.dev ||
          digest(bytes) !== entry.sha256
        )
          throw new Error(`Source changed before copy: ${entry.path}`);
        const target = path.join(snapshot, entry.path);
        mkdirSync(path.dirname(target), { recursive: true, mode: 0o700 });
        writeFileSync(target, bytes, { flag: "wx", mode: 0o400 });
      } finally {
        closeSync(descriptor);
      }
    }
    if (
      !new RegExp(
        `^go ${vulnTool.goVersion.replaceAll(".", "\\.")}$`,
        "m",
      ).test(readFileSync(path.join(snapshot, "runner/go.mod"), "utf8"))
    )
      throw new Error(
        "Runner Go version changed; rebuild and repin the vulnerability tool",
      );
    const env = vulnerabilityEnvironment(
      go,
      `/proc/${process.pid}/fd/${cacheDescriptor}`,
      control,
      Boolean(options.prepare),
    );
    let sequence = 0;
    const command = async (
      binary: string,
      args: string[],
      extraEnv = env,
      input?: string,
    ) => {
      const log = path.join(
        childRun,
        `${String(++sequence).padStart(2, "0")}-${path.basename(binary)}.log`,
      );
      try {
        return await runCommand(binary, args, {
          cwd: path.join(snapshot!, "runner"),
          env: extraEnv,
          log,
          timeoutMs: 10 * 60_000,
          signal: options.signal,
          input,
        });
      } catch (error) {
        if (error instanceof CommandFailure && !error.groupClosed)
          preserve = true;
        throw error;
      }
    };
    const binary = path.join(control, "bin/govulncheck");
    const moduleShape = JSON.parse(await command(go, ["mod", "edit", "-json"]));
    if (moduleShape.Replace?.length)
      throw new Error(
        "Module replacements are unsupported by this isolated source scan",
      );
    if (options.prepare) {
      await command(
        go,
        ["install", "-trimpath", "-buildvcs=false", vulnTool.module],
        {
          ...env,
          GOFLAGS: "",
          CGO_ENABLED: "0",
          GOBIN: path.join(control, "bin"),
        },
      );
      await command(go, ["mod", "download"]);
    } else {
      writeFileSync(binary, pinnedBytes(cacheDescriptor, "govulncheck"), {
        flag: "wx",
        mode: 0o700,
      });
    }
    const binDirectory = openSourceRoot(path.join(run, "control/bin"));
    try {
      pinnedBytes(binDirectory, "govulncheck");
    } finally {
      closeSync(binDirectory);
    }
    const identity = await command(go, ["version", "-m", binary]);
    if (
      !identity.includes(`: go${vulnTool.goVersion}\n`) ||
      !identity.includes(`\tmod\tgolang.org/x/vuln\t${vulnTool.version}\t`) ||
      !identity.includes("\tbuild\t-trimpath=true\n")
    )
      throw new Error("Unexpected govulncheck build identity");
    if (!options.prepare) {
      await command(go, ["mod", "verify"]);
      const raw = await command(binary, [
        `-db=${options.database ?? vulnTool.database}`,
        "-scan=symbol",
        "-test",
        "-tags=integration",
        "-format=json",
        "./...",
      ]);
      const save = (name: string, value: unknown) =>
        writeFileSync(
          path.join(childRun, name),
          JSON.stringify(value, null, 2) + "\n",
          { flag: "wx", mode: 0o600 },
        );
      writeFileSync(path.join(childRun, "scan.json"), raw, {
        flag: "wx",
        mode: 0o600,
      });
      const helper = path.join(control, "bin/go-vuln-json");
      await command(go, [
        "build",
        "-trimpath",
        "-buildvcs=false",
        "-o",
        helper,
        path.join(snapshot, "scripts/go-vuln-json.go"),
      ]);
      const decode = async (mode: string, data: string) =>
        JSON.parse(await command(helper, [mode], env, data)) as unknown;
      const identity: ScanIdentity = {
        ...vulnTool,
        database: options.database ?? vulnTool.database,
      };
      const report = parseScan(await decode("scan", raw), identity);
      const packages = await decode(
        "packages",
        await command(go, [
          "list",
          "-deps",
          "-test",
          "-tags=integration",
          "-json",
          "./...",
        ]),
      );
      const graph = inspectPackages(packages, moduleShape.Module.Path);
      save("packages.json", packages);
      const expectedRoots = [...new Set(report.sbom.roots as string[])].sort();
      if (JSON.stringify(expectedRoots) !== JSON.stringify(graph.roots))
        throw new Error("Scanner SBOM roots differ from selected Go packages");
      const localDB = path.join(control, "conversion-db");
      mkdirSync(path.join(localDB, "index"), { recursive: true });
      // Convert only replays the completed scan. Its initial metadata lookup
      // stays local; the original config in scan.json is the scan authority.
      writeFileSync(
        path.join(localDB, "index/db.json"),
        JSON.stringify({
          modified: report.config.db_last_modified ?? "1970-01-01T00:00:00Z",
        }),
      );
      writeFileSync(path.join(localDB, "index/modules.json"), "[]");
      let nativeTextExitCode: 0 | 3 = 0;
      try {
        await command(
          binary,
          [
            `-db=file://${localDB}`,
            "-mode=convert",
            "-format=text",
            "-show=version,verbose",
          ],
          env,
          raw,
        );
      } catch (error) {
        if (
          error instanceof CommandFailure &&
          error.exitCode === 3 &&
          error.groupClosed
        )
          nativeTextExitCode = 3;
        else throw error;
      }
      findings = verifyNativeStatus(report, nativeTextExitCode);
      const assessment = evaluateLXD(
        report,
        JSON.parse(
          readFileSync(path.join(snapshot, "scripts/go-vuln-lxd.json"), "utf8"),
        ),
        {
          identity,
          sourceSHA256: runnerInputHash(entries),
          goSum: readFileSync(path.join(snapshot, "runner/go.sum"), "utf8"),
          graph,
        },
      );
      blocked = assessment.pending.length > 0;
      save("assessment.json", {
        nativeJSONExitCode: 0,
        nativeTextExitCode,
        sourceSHA256: runnerInputHash(entries),
        ...assessment,
        blocked,
        note: "Scan assessment; final CLI acceptance also requires unchanged inputs and successful cleanup.",
      });
      const count = new Set(report.reachable.map((finding) => finding.osv))
        .size;
      summary = `Native scan reported ${count} reachable advisories (native text exit ${nativeTextExitCode}); ${assessment.disposed.length} source-not-affected; ${assessment.pending.length} unhandled reachable advisories`;
      if (assessment.reasons.length && findings)
        summary += `; disposition invalid: ${assessment.reasons.join("; ")}`;
    }
  } catch (error) {
    failures.push(error);
  } finally {
    // Every failure retains the initial diagnostic; no success precedes source and cleanup checks.
    if (entries) {
      try {
        const current = openSourceRoot(root);
        try {
          sameDirectory(current, source);
          const currentFiles = listSourcePaths(root).filter(isScanInput);
          const actual = readSourceEntries(current, currentFiles);
          if (
            actual.length !== entries.length ||
            actual.some(
              (entry, i) =>
                entry.path !== entries![i]!.path ||
                entry.ino !== entries![i]!.ino ||
                entry.dev !== entries![i]!.dev ||
                entry.sha256 !== entries![i]!.sha256,
            )
          )
            throw new Error("Runner source changed during vulnerability check");
        } finally {
          closeSync(current);
        }
        if (snapshot) {
          const copied = openSourceRoot(path.join(run!, "source"));
          try {
            const actual = readSourceEntries(copied, files);
            if (actual.some((entry, i) => entry.sha256 !== entries![i]!.sha256))
              throw new Error(
                "Scanner changed source or module locks in its snapshot",
              );
          } finally {
            closeSync(copied);
          }
        }
      } catch (error) {
        failures.push(error);
      }
    }
    if (cacheDescriptor !== undefined && runDescriptor !== undefined) {
      try {
        const current = openSourceRoot(cache);
        try {
          sameDirectory(current, cacheDescriptor);
        } finally {
          closeSync(current);
        }
        const currentRun = openSourceRoot(run!);
        try {
          sameDirectory(currentRun, runDescriptor);
        } finally {
          closeSync(currentRun);
        }
        if (options.signal?.aborted) throw options.signal.reason;
        // Stage bytes outside control so cleanup can finish before atomic publication.
        if (options.prepare && !failures.length) {
          const installed = `/proc/self/fd/${cacheDescriptor}/govulncheck`;
          try {
            if (!lstatSync(installed).isFile())
              throw new Error("Installed tool is not a regular file");
          } catch (error) {
            if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
          }
          renameSync(
            path.join(control!, "bin/govulncheck"),
            `/proc/self/fd/${runDescriptor}/govulncheck.pending`,
          );
        }
      } catch (error) {
        failures.push(error);
      }
    }
    if (!preserve) {
      for (const directory of [snapshot, control])
        if (directory) {
          try {
            rmSync(directory, { recursive: true, force: true });
          } catch (error) {
            failures.push(error);
          }
        }
    }
    if (
      options.prepare &&
      !failures.length &&
      !options.signal?.aborted &&
      cacheDescriptor !== undefined &&
      runDescriptor !== undefined
    ) {
      try {
        renameSync(
          `/proc/self/fd/${runDescriptor}/govulncheck.pending`,
          `/proc/self/fd/${cacheDescriptor}/govulncheck`,
        );
      } catch (error) {
        failures.push(error);
      }
    }
    for (const descriptor of held.reverse()) {
      try {
        closeSync(descriptor);
      } catch (error) {
        failures.push(error);
      }
    }
    if (options.signal?.aborted && !failures.includes(options.signal.reason))
      failures.push(options.signal.reason);
  }
  if (failures.length)
    throw new AggregateError(
      failures,
      `Go vulnerability operation failed; diagnostics: ${run ?? "not created"}${preserve ? "; cleanup unknown, preserve source/control for recovery" : ""}`,
    );
  return { findings, blocked, summary, report: run! };
}

if (
  process.argv[1] &&
  path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  const controller = new AbortController();
  const cancel = () =>
    controller.abort(new Error("Go vulnerability check interrupted"));
  process.once("SIGTERM", cancel);
  process.once("SIGINT", cancel);
  const timer = setTimeout(
    () =>
      controller.abort(
        new Error("Go vulnerability operation exceeded 15 minutes"),
      ),
    15 * 60_000,
  );
  try {
    const args = process.argv.slice(2);
    if (args.length > 1 || (args.length === 1 && args[0] !== "--prepare"))
      throw new Error("Usage: bun run go:vuln:prepare | bun run go:vuln:check");
    const result = await checkVulnerabilities({
      prepare: args[0] === "--prepare",
      signal: controller.signal,
    });
    console.log(`${result.summary}; diagnostics: ${result.report}`);
    process.exitCode = result.blocked ? 1 : 0;
  } catch (error) {
    console.error(describeFailure(error));
    process.exitCode = 2;
  } finally {
    clearTimeout(timer);
    process.off("SIGTERM", cancel);
    process.off("SIGINT", cancel);
  }
}
