import assert from "node:assert/strict";
import test from "node:test";
import { BoundInputs } from "./binding.ts";
import { VerifiedExecution, verifyChecks } from "./execution.ts";
import { successful, type Observation } from "./contract.ts";

test("JSON and direct constructors cannot claim source or execution verification", () => {
  assert.throws(
    () => Reflect.construct(BoundInputs, [Symbol(), {}, {}, {}]),
    /Only verified/,
  );
  assert.throws(
    () => Reflect.construct(VerifiedExecution, [Symbol(), {}]),
    /Only the supervisor/,
  );
  assert.throws(
    () => BoundInputs.requireVerified({} as BoundInputs),
    /Unverified/,
  );
  assert.throws(
    () => verifyChecks({} as VerifiedExecution, {} as BoundInputs, "docs", []),
    /Unverified/,
  );
});

test("exit zero does not replace start, timeout, cancellation or settlement evidence", () => {
  const complete = {
    started: true,
    settled: true,
    exitCode: 0,
    signal: null,
    error: null,
    timedOut: false,
    cancelled: false,
  } as Observation;
  assert.equal(successful(complete), true);
  for (const change of [
    { started: false },
    { settled: false },
    { exitCode: 1 },
    { signal: "SIGKILL" },
    { error: "unknown" },
    { timedOut: true },
    { cancelled: true },
  ])
    assert.equal(successful({ ...complete, ...change }), false);
});
