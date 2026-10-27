import { z } from "zod";
import { SourceIdentitySchema } from "./source.ts";

export const sha256 = z.string().regex(/^[a-f0-9]{64}$/);
export const commit = z.string().regex(/^[a-f0-9]{40}$/);
const absolute = z
  .string()
  .startsWith("/")
  .refine((s) => !s.includes("\0"));
export const SourcePinSchema = z.strictObject({
  commit,
  tree: commit,
  lockPath: z.string().min(1),
  lock: sha256,
});
export type SourcePin = z.infer<typeof SourcePinSchema>;
export const CompositionSchema = z.strictObject({
  policy: SourcePinSchema,
  shared: SourcePinSchema,
  distribution: sha256,
});
export type Composition = z.infer<typeof CompositionSchema>;
export const CandidateSchema = z.discriminatedUnion("event", [
  z.strictObject({
    event: z.literal("pull_request_target"),
    base: commit,
    head: commit,
    integration: commit,
    tree: commit,
  }),
  z.strictObject({ event: z.literal("push"), commit, tree: commit }),
]);
export type Candidate = z.infer<typeof CandidateSchema>;
export const TargetSchema = z.strictObject({
  repository: z.string().regex(/^[\w.-]+\/[\w.-]+$/),
  repositoryId: z.string().regex(/^[1-9][0-9]*$/),
  target: z.string().regex(/^[\w./-]+$/),
  workflow: z.string().regex(/^\.github\/workflows\/[\w.-]+\.ya?ml$/),
});
export type PolicyTarget = z.infer<typeof TargetSchema>;
export const RunSchema = z.strictObject({
  repository: z.string().regex(/^[\w.-]+\/[\w.-]+$/),
  repositoryId: z.string().regex(/^[1-9][0-9]*$/),
  id: z.string().regex(/^[1-9][0-9]*$/),
  attempt: z.string().regex(/^[1-9][0-9]*$/),
  job: z.string().min(1),
  workflowRef: z.string().min(1),
  workflowSha: commit,
});
export type Run = z.infer<typeof RunSchema>;
export const CheckSchema = z.strictObject({
  name: z.string().min(1),
  argv: z.tuple([absolute]).rest(z.string()),
  cwd: absolute,
  env: z.record(z.string(), z.string()),
  timeoutMs: z.number().int().positive().max(600_000),
});
export type Check = z.infer<typeof CheckSchema>;
export const ToolSchema = z.strictObject({
  name: z.string().min(1),
  executable: absolute,
  sha256,
  versionArgs: z.array(z.string()),
  version: z.string().min(1),
  files: z.array(z.strictObject({ path: absolute, sha256 })).optional(),
});
export type Tool = z.infer<typeof ToolSchema>;
export const PathsSchema = z.strictObject({
  policy: absolute,
  shared: absolute,
  candidate: absolute,
  tools: z.array(ToolSchema).min(1),
});
export type ExecutionPaths = z.infer<typeof PathsSchema>;
export const ObservationSchema = z.strictObject({
  request: CheckSchema,
  environment: z.record(z.string(), z.string()),
  scratch: absolute,
  uid: z.number().int().positive(),
  gid: z.number().int().positive(),
  startedAt: z.number().nonnegative(),
  finishedAt: z.number().nonnegative(),
  started: z.boolean(),
  exitCode: z.number().int().nullable(),
  signal: z.string().nullable(),
  timedOut: z.boolean(),
  cancelled: z.boolean(),
  settled: z.boolean(),
  error: z.string().nullable(),
  diagnostic: z.string(),
});
export type Observation = z.infer<typeof ObservationSchema>;
export const ExecutionReportSchema = z.strictObject({
  version: z.literal(1),
  provenance: z.discriminatedUnion("kind", [
    z.strictObject({
      kind: z.literal("local-container"),
      invocation: z.string().uuid(),
    }),
    z.strictObject({ kind: z.literal("github-hosted"), run: RunSchema }),
  ]),
  composition: CompositionSchema,
  candidate: CandidateSchema,
  target: TargetSchema,
  source: SourceIdentitySchema,
  changedPaths: z.array(z.string()).nullable(),
  paths: PathsSchema,
  scope: z.string().min(1),
  planned: z.array(z.string()),
  observations: z.array(ObservationSchema),
  errors: z.array(z.string()),
});
export type ExecutionReport = z.infer<typeof ExecutionReportSchema>;

export function successful(observation: Observation): boolean {
  return (
    observation.started &&
    observation.settled &&
    observation.exitCode === 0 &&
    observation.signal === null &&
    observation.error === null &&
    !observation.timedOut &&
    !observation.cancelled
  );
}
