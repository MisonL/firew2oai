import { appendFileSync } from "node:fs";

const logPath = process.env.DOCFORK_CAPTURE_LOG;
const maxBodyBytes = Number.parseInt(process.env.DOCFORK_CAPTURE_MAX_BODY_BYTES || "20000", 10);
const captureAllFetch = process.env.DOCFORK_CAPTURE_ALL_FETCH === "1";
const originalFetch = globalThis.fetch;

if (!logPath) {
  throw new Error("DOCFORK_CAPTURE_LOG is required");
}

if (typeof originalFetch !== "function") {
  throw new Error("global fetch is not available");
}

const sensitiveHeaderNames = new Set([
  "authorization",
  "cookie",
  "set-cookie",
  "x-api-key",
  "docfork_api_key",
  "docfork-api-key",
]);

const sensitiveQueryNames = new Set([
  "api_key",
  "apikey",
  "access_token",
  "auth",
  "key",
  "token",
]);

function appendEvent(event) {
  appendFileSync(logPath, `${JSON.stringify(event)}\n`, "utf8");
}

function shouldCapture(url) {
  if (captureAllFetch) {
    return true;
  }
  try {
    return new URL(url).hostname.endsWith("docfork.com");
  } catch {
    return false;
  }
}

function redactUrl(inputUrl) {
  const url = new URL(String(inputUrl));
  for (const key of Array.from(url.searchParams.keys())) {
    if (sensitiveQueryNames.has(key.toLowerCase())) {
      url.searchParams.set(key, "[REDACTED]");
    }
  }
  return url.toString();
}

function headersToObject(headersInit) {
  const headers = new Headers(headersInit);
  const result = {};
  for (const [key, value] of headers.entries()) {
    result[key] = sensitiveHeaderNames.has(key.toLowerCase()) ? "[REDACTED]" : value;
  }
  return result;
}

function requestHeaders(input, init) {
  const merged = new Headers(input instanceof Request ? input.headers : undefined);
  if (init?.headers) {
    for (const [key, value] of new Headers(init.headers).entries()) {
      merged.set(key, value);
    }
  }
  return headersToObject(merged);
}

function requestMethod(input, init) {
  return init?.method || (input instanceof Request ? input.method : "GET");
}

function requestUrl(input) {
  if (input instanceof Request) {
    return input.url;
  }
  return String(input);
}

async function requestBodyPreview(input, init) {
  const body = init?.body;
  if (typeof body === "string") {
    return { type: "string", bytes: Buffer.byteLength(body), preview: body.slice(0, maxBodyBytes) };
  }
  if (body instanceof URLSearchParams) {
    const text = body.toString();
    return { type: "URLSearchParams", bytes: Buffer.byteLength(text), preview: text.slice(0, maxBodyBytes) };
  }
  if (body instanceof ArrayBuffer) {
    return { type: "ArrayBuffer", bytes: body.byteLength, preview: Buffer.from(body).toString("utf8", 0, maxBodyBytes) };
  }
  if (ArrayBuffer.isView(body)) {
    return { type: body.constructor.name, bytes: body.byteLength, preview: Buffer.from(body.buffer, body.byteOffset, body.byteLength).toString("utf8", 0, maxBodyBytes) };
  }
  if (input instanceof Request && input.body) {
    return { type: "RequestBodyStream", note: "not read to avoid consuming request body" };
  }
  if (body) {
    return { type: body.constructor?.name || typeof body, note: "not rendered" };
  }
  return null;
}

async function responseBodyPreview(response) {
  const clone = response.clone();
  const text = await clone.text();
  return {
    bytes: Buffer.byteLength(text),
    truncated: Buffer.byteLength(text) > maxBodyBytes,
    preview: text.slice(0, maxBodyBytes),
  };
}

globalThis.fetch = async function capturedFetch(input, init) {
  const rawUrl = requestUrl(input);
  const capture = shouldCapture(rawUrl);
  const startedAt = Date.now();
  let safeUrl = rawUrl;

  if (capture) {
    safeUrl = redactUrl(rawUrl);
    appendEvent({
      ts: new Date().toISOString(),
      pid: process.pid,
      kind: "fetch_request",
      url: safeUrl,
      method: requestMethod(input, init),
      headers: requestHeaders(input, init),
      body: await requestBodyPreview(input, init),
    });
  }

  try {
    const response = await originalFetch(input, init);
    if (capture) {
      appendEvent({
        ts: new Date().toISOString(),
        pid: process.pid,
        kind: "fetch_response",
        url: safeUrl,
        elapsedMs: Date.now() - startedAt,
        status: response.status,
        statusText: response.statusText,
        headers: headersToObject(response.headers),
        body: await responseBodyPreview(response),
      });
    }
    return response;
  } catch (error) {
    if (capture) {
      appendEvent({
        ts: new Date().toISOString(),
        pid: process.pid,
        kind: "fetch_error",
        url: safeUrl,
        elapsedMs: Date.now() - startedAt,
        error: String(error),
      });
    }
    throw error;
  }
};
