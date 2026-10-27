import assert from "node:assert/strict";
import test from "node:test";
import { parseOptions, settleContainer } from "./local-docs.ts";

const id = "a".repeat(64),
  invocation = "test-invocation";
function state(running: boolean, label = invocation) {
  return JSON.stringify({
    Id: id,
    Config: { Labels: { "designs.local-docs": label } },
    State: {
      Running: running,
      Status: running ? "running" : "exited",
      ExitCode: 23,
      Error: "",
      OOMKilled: false,
    },
  });
}

test("local acceptance requires an exact local image and all explicit inputs", () => {
  const args = [
    "--image",
    `sha256:${id}`,
    "--node",
    "/tools/node",
    "--lychee",
    "/tools/lychee",
    "--prose",
    "/tools/prose",
    "--ids",
    "/tools/ids",
    "--shared-bundle",
    "/fixed/shared.bundle",
    "--shared-lock",
    "/fixed/shared.lock.json",
    "--output",
    "/tmp/new-output",
  ];
  assert.equal(parseOptions(args).output, "/tmp/new-output");
  assert.throws(() => parseOptions(["--image", "latest", ...args.slice(2)]));
  assert.throws(() => parseOptions([...args, "--image", `sha256:${id}`]));
  assert.throws(() => parseOptions(args.slice(0, -1)));
  assert.throws(() => parseOptions([...args, "--privileged", "true"]));
});

test("an already exited failed container is observed without restart or stop", async () => {
  const calls: string[][] = [];
  const result = await settleContainer(
    "original",
    invocation,
    async (args) => {
      calls.push(args);
      return state(false);
    },
    () => {},
  );
  assert.equal(result.State.ExitCode, 23);
  assert.equal(calls.length, 1);
  assert.equal(calls[0]?.[0], "inspect");
});

test("unknown stop is read back once without repeating the operation", async () => {
  const calls: string[][] = [],
    events: object[] = [];
  let observations = 0;
  await settleContainer(
    "original",
    invocation,
    async (args) => {
      calls.push(args);
      if (args[0] === "stop") throw new Error("reply lost");
      return state(observations++ === 0);
    },
    (event) => events.push(event),
  );
  assert.deepEqual(
    calls.map((args) => args[0]),
    ["inspect", "stop", "inspect"],
  );
  assert.deepEqual(calls[1], ["stop", "--time", "5", id]);
  assert.ok(
    events.some((event) =>
      JSON.stringify(event).includes('"outcome":"unknown"'),
    ),
  );
});

test("a persistent container is not reported as settled or stopped twice", async () => {
  const calls: string[][] = [];
  await assert.rejects(
    settleContainer(
      "original",
      invocation,
      async (args) => {
        calls.push(args);
        return args[0] === "stop" ? id : state(true);
      },
      () => {},
    ),
    /has not settled/,
  );
  assert.equal(calls.filter((args) => args[0] === "stop").length, 1);
});

test("name collision and unreadable original state never authorize a stop", async () => {
  for (const inspect of [
    async () => state(true, "someone-else"),
    async () => {
      throw new Error("daemon unavailable");
    },
  ]) {
    const calls: string[][] = [];
    await assert.rejects(
      settleContainer(
        "original",
        invocation,
        async (args) => {
          calls.push(args);
          return inspect();
        },
        () => {},
      ),
    );
    assert.deepEqual(
      calls.map((args) => args[0]),
      ["inspect"],
    );
  }
});
