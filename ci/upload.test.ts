import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createServer, request as httpRequest } from "node:http";
import { request as httpsRequest } from "node:https";
import { createServer as createTcpServer } from "node:net";
import { once } from "node:events";
import { createHash } from "node:crypto";
import {
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import os from "node:os";
import path from "node:path";
import { Readable } from "node:stream";
import test from "node:test";
import {
  artifactClient,
  prepareArchive,
  uploadReport,
  uploadContent,
  blockBytes,
  blockIds,
  reportBlob,
} from "./upload.ts";
import type { Archive } from "./upload.ts";
import { reportRequest } from "./http.ts";

const identity = {
  workflowRunBackendId: "run",
  workflowJobRunBackendId: "job",
  name: "check-1-Linux",
};
const archive: Archive = { file: "/unused", size: 123, sha256: "a".repeat(64) };
const artifact = {
  ...identity,
  databaseId: "17",
  size: "123",
  digest: { value: `sha256:${archive.sha256}` },
};
const signal = () => new AbortController().signal;

function fixture(
  fail?: "list" | "create" | "content" | "finalize",
  matches = [artifact],
) {
  const calls: string[] = [];
  const events: unknown[] = [];
  let reads = 0;
  const client: Parameters<typeof uploadReport>[2] = {
    async ListArtifacts(request) {
      assert.deepEqual(request, identity);
      calls.push("list");
      if (fail === "list") throw Error("unavailable");
      return { artifacts: reads++ ? matches : [] };
    },
    async CreateArtifact({ expiresAt: _expiresAt, ...request }) {
      assert.deepEqual(request, { ...identity, version: 4 });
      calls.push("create");
      if (fail === "create") throw Error("lost receipt");
      return {
        ok: true,
        signedUploadUrl: "https://fixture.invalid/blob?sig=fixture-secret",
      };
    },
    async FinalizeArtifact(request) {
      assert.deepEqual(request, {
        ...identity,
        size: "123",
        hash: { value: `sha256:${"a".repeat(64)}` },
      });
      calls.push("finalize");
      if (fail === "finalize") throw Error("lost receipt");
      return { ok: true, artifactId: "17" };
    },
  };
  const put = async () => {
    calls.push("content");
    if (fail === "content") throw Error("lost receipt");
  };
  const run = () =>
    uploadReport(identity, archive, client, put, signal(), (event) =>
      events.push(event),
    );
  return { calls, events, client, put, run };
}

test("original upload uses one write per phase and verifies exact identity", async () => {
  const f = fixture();
  assert.deepEqual(await f.run(), { id: "17" });
  assert.deepEqual(f.calls, ["list", "create", "content", "finalize", "list"]);
  assert.doesNotMatch(
    JSON.stringify(f.events),
    /fixture-secret|signedUploadUrl/,
  );
});

for (const [failure, expected] of [
  ["list", ["list"]],
  ["create", ["list", "create"]],
  ["content", ["list", "create", "content"]],
] as const)
  test(`${failure} failure sends no dependent write or retry`, async () => {
    const f = fixture(failure);
    await assert.rejects(f.run());
    assert.deepEqual(f.calls, expected);
  });

test("an occupied original name never gets overwritten or recreated", async () => {
  const f = fixture();
  f.client.ListArtifacts = async () => ({ artifacts: [artifact] });
  await assert.rejects(f.run(), /already occupied/);
  assert.deepEqual(f.calls, []);
});

test("lost final receipt retains the observed original artifact without claiming settlement", async () => {
  const f = fixture("finalize");
  await assert.rejects(f.run(), /could not be verified/);
  assert.deepEqual(f.calls, ["list", "create", "content", "finalize", "list"]);
  assert.ok(
    f.events.some(
      (event) =>
        JSON.stringify(event) ===
        JSON.stringify({
          phase: "finalize",
          outcome: "unknown",
          artifactId: "17",
        }),
    ),
  );
});

for (const [name, matches] of Object.entries({
  missing: [],
  duplicate: [artifact, { ...artifact, databaseId: "18" }],
  wrongJob: [{ ...artifact, workflowJobRunBackendId: "different" }],
  wrongRun: [{ ...artifact, workflowRunBackendId: "different" }],
  wrongSize: [{ ...artifact, size: "122" }],
  wrongDigest: [{ ...artifact, digest: { value: `sha256:${"b".repeat(64)}` } }],
}))
  test(`unknown finalization rejects ${name} readback`, async () => {
    const f = fixture("finalize", matches);
    await assert.rejects(f.run(), /could not be verified/);
    assert.equal(f.calls.filter((x) => x === "finalize").length, 1);
  });

test("successful receipt cannot be replaced by another artifact ID", async () => {
  const f = fixture(undefined, [{ ...artifact, databaseId: "18" }]);
  await assert.rejects(f.run(), /could not be verified/);
});

test(
  "cancellation waits for the original content operation to settle",
  { timeout: 2000 },
  async () => {
    const f = fixture();
    const controller = new AbortController();
    let release!: () => void;
    let entered!: () => void;
    const ready = new Promise<void>((resolve) => {
      entered = resolve;
    });
    const pending = uploadReport(
      identity,
      archive,
      f.client,
      async () => {
        entered();
        await new Promise<void>((resolve) => {
          release = resolve;
        });
        throw Error("transport ended after cancellation");
      },
      controller.signal,
      () => {},
    );
    let settled = false;
    const observed = pending.catch(() => {
      settled = true;
    });
    await ready;
    controller.abort();
    await Promise.resolve();
    assert.equal(settled, false);
    release();
    await observed;
    assert.equal(settled, true);
    assert.deepEqual(f.calls, ["list", "create"]);
  },
);

for (const scenario of ["ok", "503", "truncated", "lost-body"])
  test(`fixed generated client preserves wire identity and never retries ${scenario}`, async () => {
    let requests = 0;
    const client = artifactClient(
      "https://fixture.invalid",
      "fixture-only",
      signal(),
      async (url, method, _headers, body) => {
        requests++;
        assert.equal(
          String(url),
          "https://fixture.invalid/twirp/github.actions.results.api.v1.ArtifactService/CreateArtifact",
        );
        assert.equal(method, "POST");
        assert.deepEqual(JSON.parse(String(body)), {
          workflow_run_backend_id: "run",
          workflow_job_run_backend_id: "job",
          name: "check-1-Linux",
          version: 4,
        });
        if (scenario === "lost-body") throw Error("body disconnected");
        return {
          body:
            scenario === "truncated"
              ? '{"ok":'
              : '{"ok":true,"signed_upload_url":"https://fixture.invalid/blob"}',
          status: scenario === "503" ? 503 : 200,
          headers: {},
        };
      },
    );
    const operation = client.CreateArtifact({ ...identity, version: 4 });
    if (scenario === "ok")
      assert.equal(
        (await operation).signedUploadUrl,
        "https://fixture.invalid/blob",
      );
    else await assert.rejects(operation);
    assert.equal(requests, 1);
  });

test("archive is a complete single-report ZIP before any write", async (t) => {
  const directory = mkdtempSync(path.join(os.tmpdir(), "ci-upload-zip-"));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  const bytes = Buffer.from('{"original":"report"}\n');
  const result = await prepareArchive(bytes, directory, signal());
  assert.equal(
    result.sha256,
    createHash("sha256").update(readFileSync(result.file)).digest("hex"),
  );
  const decoded = execFileSync("python3", [
    "-c",
    "import zipfile,sys; z=zipfile.ZipFile(sys.argv[1]); assert z.namelist()==['report.json']; sys.stdout.buffer.write(z.read('report.json'))",
    result.file,
  ]);
  assert.deepEqual(decoded, bytes);
});

test(
  "cancelled archive preparation finishes before any remote phase",
  { timeout: 2000 },
  async (t) => {
    const directory = mkdtempSync(path.join(os.tmpdir(), "ci-upload-abort-"));
    t.after(() => rmSync(directory, { recursive: true, force: true }));
    const controller = new AbortController();
    controller.abort();
    await assert.rejects(
      prepareArchive(Buffer.from("report"), directory, controller.signal),
    );
  },
);

for (const failure of ["cancel-during", "output-exists"])
  test(
    `archive preparation settles ${failure}`,
    { timeout: 2000 },
    async (t) => {
      const directory = mkdtempSync(
        path.join(os.tmpdir(), "ci-upload-zip-failure-"),
      );
      t.after(() => rmSync(directory, { recursive: true, force: true }));
      const controller = new AbortController();
      if (failure === "output-exists")
        writeFileSync(path.join(directory, "report.zip"), "preserved");
      const result = prepareArchive(
        Buffer.alloc(2 * 1024 * 1024, 65),
        directory,
        controller.signal,
      );
      if (failure === "cancel-during") controller.abort();
      await assert.rejects(result);
      if (failure === "output-exists")
        assert.equal(
          readFileSync(path.join(directory, "report.zip"), "utf8"),
          "preserved",
        );
    },
  );

test(
  "connection setup deadline settles a request with an unfinished DNS lookup",
  { timeout: 3000 },
  async (t) => {
    t.mock.timers.enable({ apis: ["setTimeout"] });
    let lookupStarted!: () => void;
    const ready = new Promise<void>((resolve) => {
      lookupStarted = resolve;
    });
    let assigned!: () => void;
    const socketReady = new Promise<void>((resolve) => {
      assigned = resolve;
    });
    let socketClosed = false;
    const result = reportRequest(
      new URL("http://report.invalid"),
      "POST",
      {},
      "body",
      new AbortController().signal,
      ((url: URL, options: import("node:http").RequestOptions) => {
        const req = httpRequest(url, {
          ...options,
          lookup() {
            lookupStarted();
          },
        });
        req.on("socket", (socket) => {
          socket.once("close", () => {
            socketClosed = true;
          });
          assigned();
        });
        return req;
      }) as typeof httpRequest,
    );
    const rejected = assert.rejects(result, /connection setup exceeded/);
    await ready;
    await socketReady;
    t.mock.timers.tick(60_000);
    await rejected;
    assert.equal(socketClosed, true);
  },
);

test(
  "connection setup deadline includes a stalled TLS handshake",
  { timeout: 3000 },
  async (t) => {
    t.mock.timers.enable({ apis: ["setTimeout"] });
    let received!: () => void;
    const ready = new Promise<void>((resolve) => {
      received = resolve;
    });
    const server = createTcpServer((socket) => {
      socket.once("data", received);
      socket.resume();
    });
    server.listen(0, "127.0.0.1");
    await once(server, "listening");
    t.after(() => server.close());
    const address = server.address();
    assert.ok(address && typeof address !== "string");
    let socketClosed = false;
    const result = reportRequest(
      new URL(`https://127.0.0.1:${address.port}`),
      "POST",
      {},
      "body",
      new AbortController().signal,
      ((url: URL, options: import("node:http").RequestOptions) => {
        const req = httpsRequest(url, options);
        req.on("socket", (socket) =>
          socket.once("close", () => {
            socketClosed = true;
          }),
        );
        return req;
      }) as typeof httpsRequest,
    );
    const rejected = assert.rejects(result, /connection setup exceeded/);
    await ready;
    t.mock.timers.tick(60_000);
    await rejected;
    assert.equal(socketClosed, true);
  },
);

for (const scenario of [
  "small",
  "blocks",
  "unknown",
  "cancel",
  "block-unknown",
  "commit-unknown",
])
  test(
    `fixed Azure public pipeline: ${scenario}`,
    { timeout: 3000 },
    async (t) => {
      const directory = mkdtempSync(path.join(os.tmpdir(), "ci-upload-blob-"));
      t.after(() => rmSync(directory, { recursive: true, force: true }));
      const file = path.join(directory, "archive");
      const bytes = Buffer.alloc(
        ["blocks", "block-unknown", "commit-unknown"].includes(scenario)
          ? blockBytes + 7
          : 7,
        65,
      );
      writeFileSync(file, bytes);
      const input = {
        file,
        size: bytes.length,
        sha256: createHash("sha256").update(bytes).digest("hex"),
      };
      const controller = new AbortController();
      const requests: { url: string; bytes: Buffer }[] = [];
      const blob = reportBlob(
        "https://fixture.invalid/blob?sig=fixture",
        controller.signal,
        async (url, _method, _headers, body, usedSignal) => {
          assert.equal(usedSignal, controller.signal);
          const chunks: Buffer[] = [];
          if (body instanceof Readable)
            for await (const chunk of body) chunks.push(Buffer.from(chunk));
          else if (typeof body === "string") chunks.push(Buffer.from(body));
          requests.push({ url: url.href, bytes: Buffer.concat(chunks) });
          if (
            scenario === "unknown" ||
            scenario === "block-unknown" ||
            (scenario === "commit-unknown" && requests.length === 3)
          )
            throw Object.assign(Error("response lost"), { code: "ECONNRESET" });
          if (scenario === "cancel") {
            controller.abort();
            throw Error("cancelled transport settled");
          }
          return { status: 201, headers: {}, body: "" };
        },
      );
      if (
        ["unknown", "cancel", "block-unknown", "commit-unknown"].includes(
          scenario,
        )
      ) {
        await assert.rejects(uploadContent(input, blob, controller.signal));
        assert.equal(requests.length, scenario === "commit-unknown" ? 3 : 1);
      } else {
        await uploadContent(input, blob, controller.signal);
        assert.equal(requests.length, scenario === "small" ? 1 : 3);
        assert.deepEqual(
          Buffer.concat(
            requests.slice(0, scenario === "small" ? 1 : 2).map((r) => r.bytes),
          ),
          bytes,
        );
        if (scenario === "blocks") {
          assert.deepEqual(
            requests
              .slice(0, 2)
              .map((r) => new URL(r.url).searchParams.get("blockid")),
            blockIds(input),
          );
          assert.equal(
            new URL(requests[2]!.url).searchParams.get("comp"),
            "blocklist",
          );
        }
      }
    },
  );

for (const scenario of [
  "ok",
  "redirect",
  "disconnect",
  "cancel",
  "silent",
  "body-stall",
])
  test(
    `actual HTTP transport settles its original socket: ${scenario}`,
    { timeout: 3000 },
    async (t) => {
      let count = 0;
      let accepted!: () => void;
      const ready = new Promise<void>((resolve) => {
        accepted = resolve;
      });
      const server = createServer((req, res) => {
        count++;
        req.resume();
        accepted();
        if (scenario === "cancel" || scenario === "silent") return;
        if (scenario === "body-stall") {
          res.writeHead(200, { "Content-Length": "100" });
          res.write('{"ok":');
          return;
        }
        if (scenario === "disconnect") {
          res.writeHead(200, { "Content-Length": "100" });
          res.write('{"ok":');
          setImmediate(() => res.destroy());
          return;
        }
        res.writeHead(scenario === "redirect" ? 307 : 200, {
          Location: "/different",
        });
        res.end("response");
      });
      server.listen(0, "127.0.0.1");
      await once(server, "listening");
      t.after(() => {
        server.closeAllConnections();
        server.close();
      });
      const address = server.address();
      assert.ok(address && typeof address !== "string");
      const controller = new AbortController();
      let socketClosed = false;
      const result = reportRequest(
        new URL(`http://127.0.0.1:${address.port}`),
        "POST",
        {},
        "body",
        controller.signal,
        ((...args: Parameters<typeof httpRequest>) => {
          const req = httpRequest(...args);
          if (scenario === "silent" || scenario === "body-stall") {
            const setTimeout = req.setTimeout.bind(req);
            req.setTimeout = (milliseconds, callback) => {
              assert.equal(milliseconds, 60_000);
              return setTimeout(50, callback);
            };
          }
          req.on("socket", (socket) =>
            socket.once("close", () => {
              socketClosed = true;
            }),
          );
          return req;
        }) as typeof httpRequest,
      );
      const observed = [
        "cancel",
        "disconnect",
        "silent",
        "body-stall",
      ].includes(scenario)
        ? assert.rejects(result)
        : result;
      await ready;
      if (scenario === "cancel") controller.abort();
      const response = await observed;
      if (scenario === "redirect") assert.equal(response?.status, 307);
      assert.equal(count, 1);
      assert.equal(socketClosed, true);
    },
  );
