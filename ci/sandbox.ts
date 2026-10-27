import { execFileSync, spawn, spawnSync } from "node:child_process";
import {
  chmodSync,
  chownSync,
  lstatSync,
  readdirSync,
  readFileSync,
  existsSync,
} from "node:fs";
import path from "node:path";

export type CiCommand = {
  argv: [string, ...string[]];
  cwd: string;
  env: Record<string, string>;
};
export type CandidateIdentity = { uid: number; gid: number; group: string };

export function protect(directory: string): void {
  const stat = lstatSync(directory);
  if (stat.isSymbolicLink()) return;
  chownSync(directory, 0, 0);
  if (stat.isDirectory()) {
    for (const name of readdirSync(directory))
      protect(path.join(directory, name));
    chmodSync(directory, 0o755);
  } else if (stat.isFile())
    chmodSync(directory, stat.mode & 0o111 ? 0o755 : 0o644);
  else throw new Error("Special CI filesystem input");
}

function candidateProcessesRemain(
  uid: number,
  deadline = performance.now() + 10_000,
): boolean {
  let remaining = false;
  // Separate queries form a union; combining both flags would require both IDs to match.
  for (const selector of ["-u", "-U"]) {
    const timeout = Math.ceil(deadline - performance.now());
    if (timeout <= 0)
      throw new Error("Candidate process query deadline exceeded");
    const result = spawnSync("/usr/bin/pgrep", [selector, String(uid)], {
      encoding: "utf8",
      timeout,
    });
    if (
      result.error ||
      result.signal ||
      ![0, 1].includes(result.status ?? -1) ||
      (result.status === 1 && result.stdout.trim()) ||
      (result.status === 0 && !result.stdout.trim())
    )
      throw new Error("Candidate execution identity cannot be verified");
    remaining ||= result.status === 0;
  }
  return remaining;
}

export function noCandidateProcesses(uid: number): void {
  if (candidateProcessesRemain(uid))
    throw new Error("Candidate execution identity is already in use");
}

export type SupervisorKind = "github-hosted" | "local-container";

export function requireSupervisor(kind: SupervisorKind): void {
  if (kind === "local-container") {
    // Local acceptance has no platform provenance. Keep UID-wide cleanup inside a PID namespace.
    if (
      process.platform !== "linux" ||
      process.getuid?.() !== 0 ||
      !existsSync("/.dockerenv") ||
      readFileSync("/proc/1/comm", "utf8").trim() !== "docker-init"
    )
      throw new Error(
        "Local supervision requires a disposable Docker container with --init",
      );
    return;
  }
  if (
    process.getuid?.() !== 0 ||
    process.env.GITHUB_ACTIONS !== "true" ||
    process.env.RUNNER_ENVIRONMENT !== "github-hosted"
  )
    throw new Error("Trusted CI requires the disposable hosted supervisor");
}

export function executionIdentity(kind: SupervisorKind): CandidateIdentity {
  requireSupervisor(kind);
  const id = (option: string) =>
    execFileSync("/usr/bin/id", [option, "nobody"], {
      encoding: "utf8",
      timeout: 10_000,
      stdio: ["ignore", "pipe", "pipe"],
    }).trim();
  const uid = Number(id("-u")),
    gid = Number(id("-g")),
    group = id("-gn");
  if (
    !Number.isSafeInteger(uid) ||
    uid <= 0 ||
    !Number.isSafeInteger(gid) ||
    gid <= 0 ||
    !/^[a-zA-Z_][a-zA-Z0-9_-]*$/.test(group)
  )
    throw new Error("Unsupported hosted candidate identity");
  noCandidateProcesses(uid);
  return { uid, gid, group };
}

function finishCandidateProcesses(uid: number): void {
  for (const selector of ["-u", "-U"]) {
    const result = spawnSync(
      "/usr/bin/pkill",
      ["-KILL", selector, String(uid)],
      {
        encoding: "utf8",
        timeout: 10_000,
      },
    );
    if (result.error || result.signal || ![0, 1].includes(result.status ?? -1))
      throw new Error("Candidate process cleanup failed");
  }
  // A successful kill request does not mean the original processes have exited and been reaped.
  const deadline = performance.now() + 5_000;
  const pause = new Int32Array(new SharedArrayBuffer(4));
  while (candidateProcessesRemain(uid, deadline)) {
    if (performance.now() >= deadline)
      throw new Error(
        "Candidate process cleanup did not settle within 5 seconds",
      );
    Atomics.wait(pause, 0, 0, 100);
  }
}

/** Only the frozen caller supplies this environment; the process never inherits Actions credentials. */
export function isolatedArguments(
  identity: CandidateIdentity,
  command: CiCommand,
  environment: Record<string, string>,
): string[] {
  const entries = Object.entries({ ...environment, ...command.env });
  if (
    command.argv.length === 0 ||
    command.argv.some((arg) => arg.includes("\0")) ||
    !path.isAbsolute(command.cwd) ||
    entries.some(
      ([key, value]) =>
        !/^[A-Z_][A-Z0-9_]*$/.test(key) ||
        value.includes("\0") ||
        /^(?:GH_|GITHUB_|ACTIONS_|RUNNER_|CLADIRON_)/.test(key),
    )
  )
    throw new Error("Unbound candidate command or credential environment");
  return [
    "-n",
    "-u",
    "nobody",
    "-g",
    identity.group,
    "--",
    "/usr/bin/env",
    "-i",
    ...entries.map(([key, value]) => `${key}=${value}`),
    ...command.argv,
  ];
}

export async function runIsolated(
  kind: SupervisorKind,
  identity: CandidateIdentity,
  command: CiCommand,
  environment: Record<string, string>,
  timeout: number,
  signal: AbortSignal,
): Promise<{
  started: boolean;
  status: number | null;
  signal: string | null;
  error: string | null;
  stderr: string;
  timedOut: boolean;
  cancelled: boolean;
  settled: boolean;
}> {
  requireSupervisor(kind);
  noCandidateProcesses(identity.uid);
  signal.throwIfAborted();
  let result: Awaited<ReturnType<typeof runIsolated>>;
  try {
    const args = isolatedArguments(identity, command, environment);
    const boundary = args.indexOf("/usr/bin/env");
    result = await new Promise((resolve) => {
      const child = spawn(args[boundary]!, args.slice(boundary + 1), {
        cwd: command.cwd,
        uid: identity.uid,
        gid: identity.gid,
        env: {},
        stdio: ["ignore", "pipe", "pipe"],
      });
      let started = false,
        timedOut = false,
        cancelled = false;
      let error: string | null = null,
        stderr = "",
        bytes = 0;
      const terminate = () => {
        child.kill("SIGKILL");
      };
      const cancel = () => {
        cancelled = true;
        terminate();
      };
      signal.addEventListener("abort", cancel, { once: true });
      if (signal.aborted) cancel();
      const timer = setTimeout(() => {
        timedOut = true;
        terminate();
      }, timeout);
      child.once("spawn", () => {
        started = true;
      });
      child.once("error", (cause) => {
        error = cause.message;
      });
      child.stdout.on("data", (chunk: Buffer) => {
        bytes += chunk.length;
        if (bytes > 32 * 1024 * 1024) {
          error = "Candidate output exceeded 32 MiB";
          terminate();
        }
      });
      child.stderr.on("data", (chunk: Buffer) => {
        bytes += chunk.length;
        stderr = (stderr + chunk.toString("utf8")).slice(-2000);
        if (bytes > 32 * 1024 * 1024) {
          error = "Candidate output exceeded 32 MiB";
          terminate();
        }
      });
      // A descendant can inherit the pipes. Kill the whole identity when the original child exits,
      // before waiting for close, rather than waiting forever for the descendant's pipe handles.
      let settlement: Error | undefined;
      child.once("exit", () => {
        try {
          finishCandidateProcesses(identity.uid);
        } catch (cause) {
          settlement =
            cause instanceof Error ? cause : new Error(String(cause));
        }
        child.stdout.destroy();
        child.stderr.destroy();
      });
      child.once("close", (status, exitSignal) => {
        clearTimeout(timer);
        signal.removeEventListener("abort", cancel);
        if (settlement)
          error = `${error ?? "Child terminated"}; ${settlement.message}`;
        resolve({
          started,
          status,
          signal: exitSignal,
          error,
          stderr,
          timedOut,
          cancelled,
          settled: !settlement,
        });
      });
    });
  } catch (cause) {
    finishCandidateProcesses(identity.uid);
    throw cause;
  }
  // The exit handler made the only kill request. A spawn error has no child to terminate.
  if (!result.started) noCandidateProcesses(identity.uid);
  return result;
}
