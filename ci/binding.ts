import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { lstatSync, readFileSync, realpathSync } from "node:fs";
import path from "node:path";
import {
  CandidateSchema,
  CompositionSchema,
  PathsSchema,
  RunSchema,
  TargetSchema,
  type Candidate,
  type Composition,
  type ExecutionPaths,
  type PolicyTarget,
  type Run,
  type SourcePin,
} from "./contract.ts";
import {
  canonicalDigest,
  listSourcePaths,
  readSourceIdentity,
  sourceGitEnvironment,
  type SourceIdentity,
} from "./source.ts";
import { HostedReport } from "./artifact.ts";

export function git(root: string, args: string[]): string {
  return execFileSync(
    "git",
    [
      "-c",
      `safe.directory=${root}`,
      "-c",
      "core.fsmonitor=false",
      "-c",
      "core.hooksPath=/dev/null",
      ...args,
    ],
    {
      cwd: root,
      env: sourceGitEnvironment,
      encoding: "utf8",
      timeout: 10_000,
      maxBuffer: 16 * 1024 * 1024,
    },
  ).trimEnd();
}

export function fileDigest(file: string): string {
  if (!lstatSync(file).isFile() || realpathSync(file) !== file)
    throw new Error(`Expected a regular physical file: ${file}`);
  return createHash("sha256").update(readFileSync(file)).digest("hex");
}

export function verifySource(root: string, pin: SourcePin): void {
  if (
    realpathSync(root) !== root ||
    git(root, ["rev-parse", "HEAD"]) !== pin.commit ||
    git(root, ["rev-parse", "HEAD^{tree}"]) !== pin.tree
  )
    throw new Error("Source commit/tree mismatch");
  if (
    !pin.lockPath
      .split("/")
      .every((part) => part && ![".", "..", ".git"].includes(part)) ||
    path.isAbsolute(pin.lockPath) ||
    pin.lockPath.includes("\\")
  )
    throw new Error("Invalid source lock path");
  if (fileDigest(path.join(root, pin.lockPath)) !== pin.lock)
    throw new Error("Source dependency lock mismatch");
  if (
    git(root, [
      "status",
      "--porcelain",
      "--untracked-files=all",
      "--ignore-submodules=all",
    ]) !== ""
  )
    throw new Error("Source checkout contains unbound changes");
  readSourceIdentity(root, listSourcePaths(root));
}

/** Platform values are read only by the fixed entrypoint; repository policy supplies the target. */
export function hostedRun(
  target: PolicyTarget,
  candidate: Candidate,
  policy: string,
  policyCommit: string,
  candidateRoot: string,
  environment: NodeJS.ProcessEnv,
): Run {
  const run = RunSchema.parse({
    repository: environment.GITHUB_REPOSITORY,
    repositoryId: environment.GITHUB_REPOSITORY_ID,
    id: environment.GITHUB_RUN_ID,
    attempt: environment.GITHUB_RUN_ATTEMPT,
    job: environment.GITHUB_JOB,
    workflowRef: environment.GITHUB_WORKFLOW_REF,
    workflowSha: environment.GITHUB_WORKFLOW_SHA,
  });
  const base =
    candidate.event === "pull_request_target"
      ? candidate.base
      : candidate.commit;
  if (
    run.repository !== target.repository ||
    run.repositoryId !== target.repositoryId ||
    run.workflowRef !==
      `${target.repository}/${target.workflow}@refs/heads/${target.target}` ||
    run.workflowSha !== base ||
    environment.GITHUB_EVENT_NAME !== candidate.event
  )
    throw new Error("Unexpected repository, event, target or workflow source");
  if (
    git(candidateRoot, ["rev-parse", `${base}:${target.workflow}`]) !==
    git(policy, ["rev-parse", `${policyCommit}:${target.workflow}`])
  )
    throw new Error("Workflow differs from the fixed policy source");
  return run;
}

const bindingToken = Symbol("verified inputs");
const bound = new WeakSet<object>();

function freeze<T>(value: T): T {
  if (value && typeof value === "object") {
    for (const item of Object.values(value)) freeze(item);
    Object.freeze(value);
  }
  return value;
}

export class BoundInputs {
  readonly composition: Composition;
  readonly candidate: Candidate;
  readonly paths: ExecutionPaths;
  readonly source: ReturnType<typeof readSourceIdentity>;
  readonly changedPaths: string[] | null;
  readonly target: PolicyTarget;

  private constructor(
    token: symbol,
    composition: Composition,
    candidate: Candidate,
    paths: ExecutionPaths,
    target: PolicyTarget,
    source: ReturnType<typeof readSourceIdentity>,
    changedPaths: string[] | null,
  ) {
    if (token !== bindingToken)
      throw new Error("Only verified inputs can construct a binding");
    this.composition = composition;
    this.candidate = candidate;
    this.paths = paths;
    this.target = target;
    this.source = source;
    this.changedPaths = changedPaths;
    bound.add(this);
    freeze(this);
  }

  static requireVerified(inputs: BoundInputs): void {
    if (!bound.has(inputs)) throw new Error("Unverified execution inputs");
  }

  static fromHosted(original: HostedReport): BoundInputs {
    HostedReport.requireVerified(original);
    const report = original.report;
    return new BoundInputs(
      bindingToken,
      report.composition,
      report.candidate,
      report.paths,
      report.target,
      report.source,
      report.changedPaths,
    );
  }

  verifyCandidate(directory: string): string {
    BoundInputs.requireVerified(this);
    const root = realpathSync(directory);
    const revision =
      this.candidate.event === "pull_request_target"
        ? this.candidate.integration
        : this.candidate.commit;
    if (
      git(root, ["rev-parse", "HEAD"]) !== revision ||
      git(root, ["rev-parse", "HEAD^{tree}"]) !== this.candidate.tree ||
      git(root, [
        "status",
        "--porcelain",
        "--untracked-files=all",
        "--ignore-submodules=all",
      ]) !== ""
    )
      throw new Error(
        "Local candidate does not match the authenticated execution",
      );
    const actual = readSourceIdentity(root, listSourcePaths(root));
    // Source preparation and protection can change read/write bits on either snapshot.
    // Preserve Git's executable distinction while comparing both sides symmetrically.
    const normalize = (entries: SourceIdentity["entries"]) =>
      entries.map((entry) => ({
        ...entry,
        mode: entry.mode & 0o111 ? 0o755 : 0o644,
      }));
    if (
      actual.baseline !== this.source.baseline ||
      canonicalDigest(normalize(actual.entries)) !==
        canonicalDigest(normalize(this.source.entries))
    )
      throw new Error(
        "Local candidate source differs from authenticated source members",
      );
    return root;
  }

  static verify(
    compositionInput: Composition,
    candidateInput: Candidate,
    pathsInput: ExecutionPaths,
    distribution: string,
    targetInput: PolicyTarget,
  ): BoundInputs {
    const composition = CompositionSchema.parse(compositionInput);
    const candidate = CandidateSchema.parse(candidateInput);
    const paths = PathsSchema.parse(pathsInput);
    const target = TargetSchema.parse(targetInput);
    if (paths.shared !== realpathSync(path.resolve(import.meta.dirname, "..")))
      throw new Error("The loaded S is not the bound shared source");
    if (fileDigest(distribution) !== composition.distribution)
      throw new Error("Distribution digest mismatch");
    const heads = git(paths.shared, [
      "bundle",
      "list-heads",
      distribution,
    ]).split("\n");
    if (
      heads.length !== 1 ||
      heads[0]!.split(" ")[0] !== composition.shared.commit
    )
      throw new Error("Distribution differs from the bound shared source");
    verifySource(paths.policy, composition.policy);
    verifySource(paths.shared, composition.shared);
    const runtime = JSON.parse(
      readFileSync(path.join(paths.shared, "package.json"), "utf8"),
    ) as { engines?: { node?: string } };
    const supervisor = paths.tools.find((tool) => tool.name === "node");
    if (
      process.versions.node !== runtime.engines?.node ||
      !supervisor ||
      fileDigest(realpathSync(process.execPath)) !== supervisor.sha256 ||
      process.env.NODE_OPTIONS ||
      process.env.NODE_PATH
    )
      throw new Error(
        "The actual supervisor runtime or startup environment is not pinned",
      );
    const roots = [paths.policy, paths.shared, paths.candidate];
    // P's fixed import is .shared/ci/*.ts. The nested checkout has its own Git/lock identity
    // and must be excluded from P's source; no arbitrary overlap or external symlink is allowed.
    const nestedShared = paths.shared === path.join(paths.policy, ".shared");
    if (
      nestedShared &&
      (git(paths.policy, ["ls-files", "--", ".shared"]) !== "" ||
        git(paths.policy, ["check-ignore", ".shared"]) !== ".shared")
    )
      throw new Error(
        "The fixed .shared checkout must be ignored and untracked by P",
      );
    if (
      roots.some((root) => realpathSync(root) !== root) ||
      roots.some((a, i) =>
        roots.some(
          (b, j) =>
            i !== j &&
            !(nestedShared && i === 1 && j === 0) &&
            (a === b || a.startsWith(b + path.sep)),
        ),
      )
    )
      throw new Error("Execution roots overlap or are not physical paths");
    const revision =
      candidate.event === "pull_request_target"
        ? candidate.integration
        : candidate.commit;
    if (
      git(paths.candidate, ["rev-parse", "HEAD"]) !== revision ||
      git(paths.candidate, ["rev-parse", "HEAD^{tree}"]) !== candidate.tree ||
      git(paths.candidate, [
        "status",
        "--porcelain",
        "--untracked-files=all",
        "--ignore-submodules=all",
      ]) !== "" ||
      git(paths.candidate, [
        "ls-files",
        "--others",
        "--ignored",
        "--exclude-standard",
      ]) !== ""
    )
      throw new Error("Candidate identity or source mismatch");
    if (
      candidate.event === "pull_request_target" &&
      git(paths.candidate, ["show", "-s", "--format=%P", "HEAD"]) !==
        `${candidate.base} ${candidate.head}`
    )
      throw new Error("Integration must have exact ordered base/head parents");
    if (
      new Set(paths.tools.map((tool) => tool.name)).size !== paths.tools.length
    )
      throw new Error("Duplicate tool name");
    for (const tool of paths.tools) {
      if (fileDigest(tool.executable) !== tool.sha256)
        throw new Error(`Tool bytes mismatch: ${tool.name}`);
      for (const file of tool.files ?? [])
        if (fileDigest(file.path) !== file.sha256)
          throw new Error(`Tool resource mismatch: ${tool.name}`);
      const version = execFileSync(tool.executable, tool.versionArgs, {
        env: { PATH: "/usr/bin:/bin", HOME: "/nonexistent", LANG: "C.UTF-8" },
        cwd: paths.shared,
        encoding: "utf8",
        timeout: 10_000,
      }).trim();
      if (version !== tool.version)
        throw new Error(`Tool version mismatch: ${tool.name}`);
    }
    const source = readSourceIdentity(
      paths.candidate,
      listSourcePaths(paths.candidate),
    );
    const changedPaths =
      candidate.event === "pull_request_target"
        ? git(paths.candidate, [
            "diff",
            "--no-ext-diff",
            "--no-textconv",
            // Business selection must see removals as well as rename destinations.
            "--no-renames",
            "--name-only",
            "-z",
            candidate.base,
            candidate.integration,
          ])
            .split("\0")
            .filter(Boolean)
        : null;
    return new BoundInputs(
      bindingToken,
      composition,
      candidate,
      paths,
      target,
      source,
      changedPaths,
    );
  }
}
