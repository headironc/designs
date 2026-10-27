import https from "node:https";
import type { IncomingHttpHeaders } from "node:http";
import { Readable } from "node:stream";
import { pipeline } from "node:stream/promises";

/** Report transport: one request, no redirects/retries, and a closed local socket on return. */
export async function reportRequestBytes(
  url: URL,
  method: string,
  headers: Record<string, string>,
  body: string | Readable,
  signal: AbortSignal,
  request = https.request,
  maxBytes = 16 * 1024 * 1024,
): Promise<{ status: number; headers: IncomingHttpHeaders; body: Buffer }> {
  signal.throwIfAborted();
  const req = request(url, { method, headers, agent: false, signal });
  // Leave time to settle the original socket before the workflow deadline.
  req.setTimeout(60_000, () => {
    req.destroy(new Error("Report connection inactive for 60 seconds"));
  });
  const closed = new Promise<void>((resolve) => req.once("close", resolve));
  // Socket inactivity starts after connection; DNS, TCP and TLS setup need their own bound.
  const connecting = setTimeout(() => {
    req.destroy(new Error("Report connection setup exceeded 60 seconds"));
  }, 60_000);
  let socketClosed = Promise.resolve();
  req.once("socket", (socket) => {
    if (url.protocol === "https:")
      socket.once("secureConnect", () => clearTimeout(connecting));
    else if (socket.connecting)
      socket.once("connect", () => clearTimeout(connecting));
    else clearTimeout(connecting);
    socketClosed = new Promise<void>((resolve) =>
      socket.once("close", () => resolve()),
    );
  });
  const response = new Promise<{
    status: number;
    headers: IncomingHttpHeaders;
    body: Buffer;
  }>((resolve, reject) => {
    req.once("error", reject);
    req.once("response", async (res) => {
      try {
        const chunks: Buffer[] = [];
        let size = 0;
        for await (const chunk of res) {
          size += chunk.length;
          if (size > maxBytes)
            throw new Error("Report response exceeds its byte bound");
          chunks.push(Buffer.from(chunk));
        }
        if (!res.complete || res.statusCode === undefined)
          throw new Error("Incomplete report response");
        resolve({
          status: res.statusCode,
          headers: res.headers,
          body: Buffer.concat(chunks),
        });
      } catch (error) {
        reject(error);
      }
    });
  });
  const writing =
    body instanceof Readable
      ? pipeline(body, req, { signal })
      : Promise.resolve(req.end(body));
  // On either failure, terminate the original request before waiting for all its resources.
  response.catch(() => req.destroy());
  writing.catch(() => req.destroy());
  try {
    const results = await Promise.allSettled([response, writing]);
    for (const result of results)
      if (result.status === "rejected") throw result.reason;
    return (results[0] as PromiseFulfilledResult<Awaited<typeof response>>)
      .value;
  } finally {
    clearTimeout(connecting);
    req.destroy();
    await closed;
    await socketClosed;
  }
}

export async function reportRequest(
  url: URL,
  method: string,
  headers: Record<string, string>,
  body: string | Readable,
  signal: AbortSignal,
  request = https.request,
): Promise<{ status: number; headers: IncomingHttpHeaders; body: string }> {
  const response = await reportRequestBytes(
    url,
    method,
    headers,
    body,
    signal,
    request,
  );
  return { ...response, body: response.body.toString("utf8") };
}
