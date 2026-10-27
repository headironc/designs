import { randomUUID } from "node:crypto";
import {
  chmodSync,
  chownSync,
  cpSync,
  lstatSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  realpathSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { BoundInputs, fileDigest, hostedRun } from "./binding.ts";
import {
  CheckSchema,
  ExecutionReportSchema,
  successful,
  type Check,
  type ExecutionReport,
  type Observation,
} from "./contract.ts";
import {
  canonicalDigest,
  listSourcePaths,
  readSourceIdentity,
} from "./source.ts";
import {
  executionIdentity,
  protect,
  requireSupervisor,
  runIsolated,
} from "./sandbox.ts";
import { HostedReport } from "./artifact.ts";

export type Preparation = {
  copies: { from: string; to: string }[];
  writable: string[];
};

function below(file: string, root: string): boolean {
  return file.startsWith(root + path.sep) && path.resolve(file) === file;
}

function candidateOutput(file: string, root: string): boolean {
  return (
    below(file, root) &&
    ![".git", ".shared"].includes(path.relative(root, file).split(path.sep)[0]!)
  );
}

function protectedParents(file: string): void {
  let current = path.dirname(file);
  while (true) {
    const stat = lstatSync(current);
    if (stat.uid !== 0 || (stat.mode & 0o022 && !(stat.mode & 0o1000)))
      throw new Error(
        "A candidate can replace a protected input through its parent",
      );
    const parent = path.dirname(current);
    if (parent === current) break;
    current = parent;
  }
}

function protectedTree(root: string, directory = root): void {
  const stat = lstatSync(directory);
  if (stat.isSymbolicLink()) {
    const target = realpathSync(directory);
    if (!below(target, root))
      throw new Error("Dependency symlink escapes its protected root");
    return;
  }
  if (stat.uid !== 0 || stat.mode & 0o022)
    throw new Error("Source or dependency is writable by the candidate");
  if (stat.isDirectory())
    for (const name of readdirSync(directory))
      protectedTree(root, path.join(directory, name));
  else if (!stat.isFile()) throw new Error("Special protected input");
}

function validateChecks(inputs: BoundInputs, requests: Check[]): Check[] {
  const checks = requests.map((request) => CheckSchema.parse(request));
  if (
    checks.length === 0 ||
    new Set(checks.map((check) => check.name)).size !== checks.length
  )
    throw new Error("Empty or duplicate check obligations");
  const { policy, candidate, tools } = inputs.paths;
  for (const check of checks) {
    if (
      ![policy, candidate].includes(check.cwd) ||
      !tools.some((tool) => tool.executable === check.argv[0])
    )
      throw new Error("Unbound check runtime or working directory");
    for (const [key, value] of Object.entries(check.env)) {
      if (
        !/^[A-Z_][A-Z0-9_]*$/.test(key) ||
        value.includes("\0") ||
        /^(?:GH_|GITHUB_|ACTIONS_|RUNNER_|CLADIRON_|LD_|DYLD_)/.test(key) ||
        [
          "HOME",
          "TMPDIR",
          "TMP",
          "TEMP",
          "PATH",
          "NODE_OPTIONS",
          "BUN_OPTIONS",
          "ENV",
          "BASH_ENV",
        ].includes(key) ||
        key.startsWith("GIT_CONFIG")
      )
        throw new Error(
          "Policy environment overrides the supervisor or startup boundary",
        );
    }
    if (check.argv.some((arg) => arg.includes("\0")))
      throw new Error("NUL in command input");
  }
  return checks;
}

// This WeakSet is the live supervisor's provenance. Deserialized booleans cannot mint acceptance.
const observed = new WeakSet<object>();
const supervisorToken = Symbol("supervisor result");
export class VerifiedExecution {
  readonly report: ExecutionReport;
  private constructor(token: symbol, report: ExecutionReport) {
    if (token !== supervisorToken)
      throw new Error("Only the supervisor can construct an execution");
    this.report = freeze(ExecutionReportSchema.parse(report));
    observed.add(this);
    Object.freeze(this);
  }

  static fromHosted(original: HostedReport): VerifiedExecution {
    HostedReport.requireVerified(original);
    return new VerifiedExecution(supervisorToken, original.report);
  }

  static async run(
    inputs: BoundInputs,
    scope: string,
    requests: Check[],
    options: {
      provenance: { kind: "local-container" } | { kind: "github-hosted" };
      preparation: Preparation;
      reportDirectory: string;
      signal: AbortSignal;
    },
  ): Promise<VerifiedExecution> {
    BoundInputs.requireVerified(inputs);
    requireSupervisor(options.provenance.kind);
    const checks = validateChecks(inputs, requests);
    const identity = executionIdentity(options.provenance.kind);
    const { policy, shared, candidate, tools } = inputs.paths;
    const provenance: ExecutionReport["provenance"] =
      options.provenance.kind === "local-container"
        ? { kind: "local-container", invocation: randomUUID() }
        : {
            kind: "github-hosted",
            run: hostedRun(
              inputs.target,
              inputs.candidate,
              policy,
              inputs.composition.policy.commit,
              candidate,
              process.env,
            ),
          };
    const directory = realpathSync(options.reportDirectory);
    const stat = lstatSync(directory);
    if (
      stat.uid !== 0 ||
      stat.mode & 0o077 ||
      [policy, shared, candidate].some(
        (root) => directory === root || below(directory, root),
      )
    )
      throw new Error(
        "Report directory must be supervisor-private and outside all inputs",
      );
    for (const copy of options.preparation.copies) {
      if (
        (!below(copy.from, policy) && !below(copy.from, shared)) ||
        !candidateOutput(copy.to, candidate)
      )
        throw new Error("Unbound dependency copy");
      cpSync(copy.from, copy.to, {
        recursive: true,
        errorOnExist: true,
        force: false,
        verbatimSymlinks: true,
      });
    }
    for (const root of [policy, shared, candidate]) {
      protectedParents(root);
      protect(root);
      protectedTree(root);
    }
    for (const tool of tools) {
      for (const file of [
        { path: tool.executable, sha256: tool.sha256 },
        ...(tool.files ?? []),
      ]) {
        protectedParents(file.path);
        if (
          lstatSync(file.path).uid !== 0 ||
          lstatSync(file.path).mode & 0o022 ||
          fileDigest(file.path) !== file.sha256 ||
          below(file.path, candidate)
        )
          throw new Error(
            `Runtime/tool is mutable or candidate-owned: ${file.path} (uid=${lstatSync(file.path).uid}, mode=${(lstatSync(file.path).mode & 0o777).toString(8)})`,
          );
      }
    }
    for (const output of options.preparation.writable) {
      if (!candidateOutput(output, candidate))
        throw new Error("Unbound writable candidate output");
      mkdirSync(output, { mode: 0o700 });
      chownSync(output, identity.uid, identity.gid);
    }
    const before = readSourceIdentity(candidate, listSourcePaths(candidate));
    if (
      before.entries.length !== inputs.source.entries.length ||
      before.entries.some((entry, index) => {
        const original = inputs.source.entries[index]!;
        return (
          entry.path !== original.path ||
          entry.sha256 !== original.sha256 ||
          entry.bytes !== original.bytes
        );
      })
    )
      throw new Error("Preparation changed candidate source inputs");
    const report: ExecutionReport = {
      version: 1,
      provenance,
      composition: inputs.composition,
      candidate: inputs.candidate,
      source: before,
      target: inputs.target,
      changedPaths: inputs.changedPaths,
      paths: inputs.paths,
      scope,
      planned: checks.map((check) => check.name),
      observations: [],
      errors: [],
    };
    const rootInputs = [policy, shared].map((root) =>
      readSourceIdentity(root, listSourcePaths(root)),
    );
    for (const check of checks) {
      if (options.signal.aborted) {
        report.errors.push("Execution cancelled before the next check");
        break;
      }
      // Candidate code never gets the private report directory or a prior check's HOME.
      const scratch = mkdtempSync("/tmp/designs-ci-child-");
      chownSync(scratch, identity.uid, identity.gid);
      chmodSync(scratch, 0o700);
      const environment = {
        PATH: [
          ...new Set(tools.map((tool) => path.dirname(tool.executable))),
          "/usr/bin",
          "/bin",
        ].join(":"),
        HOME: scratch,
        TMPDIR: scratch,
        LANG: "C.UTF-8",
        GIT_CONFIG_GLOBAL: "/dev/null",
        GIT_CONFIG_NOSYSTEM: "1",
        ...check.env,
      };
      const observation: Observation = {
        request: check,
        environment,
        scratch,
        uid: identity.uid,
        gid: identity.gid,
        startedAt: Date.now(),
        finishedAt: 0,
        started: false,
        exitCode: null,
        signal: null,
        timedOut: false,
        cancelled: false,
        settled: false,
        error: null,
        diagnostic: "",
      };
      try {
        const result = await runIsolated(
          options.provenance.kind,
          identity,
          { ...check, env: {} },
          environment,
          check.timeoutMs,
          options.signal,
        );
        Object.assign(observation, {
          started: result.started,
          exitCode: result.status,
          signal: result.signal,
          timedOut: result.timedOut,
          cancelled: result.cancelled,
          settled: result.settled,
          error: result.error,
          diagnostic: result.stderr,
        });
        if (result.settled) rmSync(scratch, { recursive: true, force: true });
      } catch (cause) {
        observation.error =
          cause instanceof Error ? cause.message : String(cause);
        observation.cancelled = options.signal.aborted;
      }
      observation.finishedAt = Date.now();
      report.observations.push(observation);
      // An unsettled identity cannot be reused. Its original scratch stays available for recovery.
      if (!observation.settled || observation.cancelled) break;
    }
    if (
      readSourceIdentity(candidate, listSourcePaths(candidate)).sha256 !==
      before.sha256
    )
      report.errors.push("Candidate source changed during execution");
    for (const [index, root] of [policy, shared].entries()) {
      if (
        readSourceIdentity(root, listSourcePaths(root)).sha256 !==
        rootInputs[index]!.sha256
      )
        report.errors.push("Fixed source changed during execution");
    }
    for (const tool of tools)
      for (const file of [
        { path: tool.executable, sha256: tool.sha256 },
        ...(tool.files ?? []),
      ])
        if (fileDigest(file.path) !== file.sha256)
          report.errors.push("Tool changed during execution");
    const verified = new VerifiedExecution(supervisorToken, report);
    writeFileSync(
      path.join(directory, "report.json"),
      JSON.stringify(verified.report) + "\n",
      { mode: 0o600, flag: "wx" },
    );
    return verified;
  }
}

function freeze<T>(value: T): T {
  if (value && typeof value === "object") {
    for (const item of Object.values(value)) freeze(item);
    Object.freeze(value);
  }
  return value;
}

/** P reconstructs obligations from independently bound inputs, never from report.planned. */
export function verifyChecks(
  execution: VerifiedExecution,
  inputs: BoundInputs,
  scope: string,
  expected: Check[],
): void {
  if (!observed.has(execution))
    throw new Error("Unverified execution provenance");
  BoundInputs.requireVerified(inputs);
  const report = execution.report;
  const checks = validateChecks(inputs, expected);
  if (
    canonicalDigest(report.composition) !==
      canonicalDigest(inputs.composition) ||
    canonicalDigest(report.candidate) !== canonicalDigest(inputs.candidate) ||
    canonicalDigest(report.paths) !== canonicalDigest(inputs.paths) ||
    canonicalDigest(report.target) !== canonicalDigest(inputs.target) ||
    canonicalDigest(report.changedPaths) !==
      canonicalDigest(inputs.changedPaths) ||
    report.scope !== scope ||
    report.errors.length ||
    report.observations.length !== checks.length
  )
    throw new Error("Execution identity, scope or completeness mismatch");
  for (const [index, check] of checks.entries()) {
    const observation = report.observations[index]!;
    if (
      canonicalDigest(observation.request) !== canonicalDigest(check) ||
      !successful(observation)
    )
      throw new Error(
        `Required business check failed or differs: ${check.name}`,
      );
  }
}
