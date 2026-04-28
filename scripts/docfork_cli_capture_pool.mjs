#!/usr/bin/env node
import { createWriteStream, existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { spawn } from "node:child_process";
import { createInterface } from "node:readline";

const scriptDir = dirname(fileURLToPath(import.meta.url));
const repoRoot = resolve(scriptDir, "..");
const tracerPath = resolve(scriptDir, "docfork_fetch_capture.mjs");
const defaultDocforkEntry = "/Users/mison/.nvm/versions/node/v24.14.0/lib/node_modules/docfork/dist/index.js";
const defaultOutputDir = resolve(repoRoot, ".docfork-capture", new Date().toISOString().replace(/[:.]/g, "-"));

function usage() {
  return `Usage: node scripts/docfork_cli_capture_pool.mjs [options]

Options:
  --output <dir>             Output directory. Default: .docfork-capture/<timestamp>
  --docfork-entry <path>     Docfork dist/index.js path.
  --proxy <url|direct|env>   Add one proxy pool entry. Repeatable.
  --proxies <csv>            Add comma-separated proxy entries.
  --tool <search|fetch|both> Tool calls to run. Default: both.
  --timeout-ms <ms>          Per child timeout. Default: 20000.
  --help                     Show this help.

Environment:
  DOCFORK_PROXY_POOL         Comma-separated proxy entries when no --proxy is set.
  DOCFORK_CAPTURE_QUERY      search_docs query. Default: react hooks useEffect
  DOCFORK_CAPTURE_LIBRARY    search_docs library. Default: react
  DOCFORK_CAPTURE_FETCH_URL  fetch_doc URL.
`;
}

function parseArgs(argv) {
  const config = {
    outputDir: defaultOutputDir,
    docforkEntry: defaultDocforkEntry,
    proxies: [],
    tool: "both",
    timeoutMs: 20000,
  };

  for (let index = 0; index < argv.length; index += 1) {
    const arg = argv[index];
    if (arg === "--help") {
      console.log(usage());
      process.exit(0);
    }
    if (arg === "--output") {
      config.outputDir = resolve(argv[++index]);
    } else if (arg === "--docfork-entry") {
      config.docforkEntry = resolve(argv[++index]);
    } else if (arg === "--proxy") {
      config.proxies.push(argv[++index]);
    } else if (arg === "--proxies") {
      config.proxies.push(...argv[++index].split(",").map((item) => item.trim()).filter(Boolean));
    } else if (arg === "--tool") {
      config.tool = argv[++index];
    } else if (arg === "--timeout-ms") {
      config.timeoutMs = Number.parseInt(argv[++index], 10);
    } else {
      throw new Error(`Unknown argument: ${arg}`);
    }
  }

  if (!["search", "fetch", "both"].includes(config.tool)) {
    throw new Error("--tool must be search, fetch, or both");
  }
  if (!Number.isFinite(config.timeoutMs) || config.timeoutMs <= 0) {
    throw new Error("--timeout-ms must be a positive integer");
  }
  if (!existsSync(config.docforkEntry)) {
    throw new Error(`Docfork entry not found: ${config.docforkEntry}`);
  }

  if (config.proxies.length === 0 && process.env.DOCFORK_PROXY_POOL) {
    config.proxies = process.env.DOCFORK_PROXY_POOL.split(",").map((item) => item.trim()).filter(Boolean);
  }
  if (config.proxies.length === 0) {
    config.proxies = ["direct"];
  }

  return config;
}

function redactHeaders(headers) {
  const result = {};
  for (const [key, value] of Object.entries(headers || {})) {
    result[key] = /authorization|cookie|api[-_]?key|token/i.test(key) ? "[REDACTED]" : value;
  }
  return result;
}

function redactProxy(proxy) {
  if (!proxy || proxy === "direct" || proxy === "env") {
    return proxy;
  }
  try {
    const url = new URL(proxy);
    if (url.username) {
      url.username = "[REDACTED]";
    }
    if (url.password) {
      url.password = "[REDACTED]";
    }
    return url.toString();
  } catch {
    return "[INVALID_PROXY_URL]";
  }
}

function publicConfig(config) {
  return {
    ...config,
    proxies: config.proxies.map(redactProxy),
  };
}

function writeJsonl(stream, event) {
  stream.write(`${JSON.stringify(event)}\n`);
}

function makeRequest(id, method, params) {
  return { jsonrpc: "2.0", id, method, params };
}

function makeNotification(method, params) {
  return { jsonrpc: "2.0", method, params };
}

function callForTool(tool) {
  if (tool === "search") {
    return {
      name: "search_docs",
      arguments: {
        library: process.env.DOCFORK_CAPTURE_LIBRARY || "react",
        query: process.env.DOCFORK_CAPTURE_QUERY || "react hooks useEffect",
        tokens: "100",
      },
    };
  }
  return {
    name: "fetch_doc",
    arguments: {
      url: process.env.DOCFORK_CAPTURE_FETCH_URL || "https://github.com/facebook/react/tree/main/docs",
    },
  };
}

function proxyEnv(baseEnv, proxy) {
  const env = { ...baseEnv };

  if (proxy !== "env") {
    delete env.HTTP_PROXY;
    delete env.HTTPS_PROXY;
    delete env.ALL_PROXY;
    delete env.http_proxy;
    delete env.https_proxy;
    delete env.all_proxy;
  }

  if (proxy && proxy !== "direct" && proxy !== "env") {
    env.HTTP_PROXY = proxy;
    env.HTTPS_PROXY = proxy;
  }

  env.DOCFORK_CAPTURE_LOG = baseEnv.DOCFORK_CAPTURE_LOG;
  env.DOCFORK_CAPTURE_MAX_BODY_BYTES = baseEnv.DOCFORK_CAPTURE_MAX_BODY_BYTES || "50000";
  return env;
}

async function runOne(config, proxy, runDir) {
  mkdirSync(runDir, { recursive: true });
  const captureLog = resolve(runDir, "capture.jsonl");
  const mcpLog = resolve(runDir, "mcp.jsonl");
  const mcpStream = createWriteStream(mcpLog, { flags: "a" });
  const baseEnv = {
    ...process.env,
    DOCFORK_CAPTURE_LOG: captureLog,
  };
  const env = proxyEnv(baseEnv, proxy);
  const args = [
    "--use-env-proxy",
    "--import",
    tracerPath,
    config.docforkEntry,
  ];
  const child = spawn(process.execPath, args, {
    env,
    stdio: ["pipe", "pipe", "pipe"],
  });
  const pending = new Map();
  const stderrLines = [];
  const startedAt = Date.now();

  const timeout = setTimeout(() => {
    child.kill("SIGTERM");
  }, config.timeoutMs);

  child.on("exit", (code, signal) => {
    for (const [, pendingRequest] of pending) {
      pendingRequest.reject(new Error(`docfork exited before response: code=${code} signal=${signal}`));
    }
    pending.clear();
  });

  const stdout = createInterface({ input: child.stdout });
  stdout.on("line", (line) => {
    writeJsonl(mcpStream, { ts: new Date().toISOString(), direction: "stdout", line });
    let parsed;
    try {
      parsed = JSON.parse(line);
    } catch {
      return;
    }
    if (parsed.id !== undefined && pending.has(parsed.id)) {
      const pendingRequest = pending.get(parsed.id);
      pending.delete(parsed.id);
      clearTimeout(pendingRequest.timer);
      pendingRequest.resolve(parsed);
    }
  });

  const stderr = createInterface({ input: child.stderr });
  stderr.on("line", (line) => {
    stderrLines.push(line);
    writeJsonl(mcpStream, { ts: new Date().toISOString(), direction: "stderr", line });
  });

  function send(message) {
    writeJsonl(mcpStream, { ts: new Date().toISOString(), direction: "stdin", message });
    child.stdin.write(`${JSON.stringify(message)}\n`);
  }

  function request(method, params) {
    const id = request.nextId;
    request.nextId += 1;
    const message = makeRequest(id, method, params);
    const promise = new Promise((resolveMessage, rejectMessage) => {
      const timer = setTimeout(() => {
        pending.delete(id);
        rejectMessage(new Error(`timeout waiting for MCP response id=${id} method=${method}`));
      }, config.timeoutMs);
      pending.set(id, { resolve: resolveMessage, reject: rejectMessage, timer });
    });
    send(message);
    return promise;
  }
  request.nextId = 1;

  const toolResults = [];
  try {
    const initResponse = await request("initialize", {
      protocolVersion: "2024-11-05",
      capabilities: {},
      clientInfo: { name: "docfork-cli-capture-pool", version: "0.1.0" },
    });
    send(makeNotification("notifications/initialized", {}));

    const tools = [];
    if (config.tool === "search" || config.tool === "both") {
      tools.push("search");
    }
    if (config.tool === "fetch" || config.tool === "both") {
      tools.push("fetch");
    }
    for (const tool of tools) {
      const response = await request("tools/call", callForTool(tool));
      toolResults.push({
        tool,
        responsePreview: JSON.stringify(response).slice(0, 1000),
      });
    }

    child.kill("SIGTERM");
    const exit = await new Promise((resolveExit) => {
      child.on("exit", (code, signal) => resolveExit({ code, signal }));
    });
    clearTimeout(timeout);
    mcpStream.end();
    return {
      proxy: redactProxy(proxy),
      elapsedMs: Date.now() - startedAt,
      exit,
      initialize: initResponse.result ? "ok" : "unexpected",
      toolResults,
      captureLog,
      mcpLog,
      stderrPreview: stderrLines.slice(0, 20),
    };
  } catch (error) {
    child.kill("SIGTERM");
    clearTimeout(timeout);
    mcpStream.end();
    return {
      proxy: redactProxy(proxy),
      elapsedMs: Date.now() - startedAt,
      error: String(error),
      captureLog,
      mcpLog,
      stderrPreview: stderrLines.slice(0, 20),
    };
  }
}

function summarizeCapture(captureLog) {
  if (!existsSync(captureLog)) {
    return [];
  }
  return readFileSync(captureLog, "utf8")
    .split("\n")
    .filter(Boolean)
    .map((line) => JSON.parse(line))
    .map((event) => {
      if (event.kind === "fetch_request") {
        return {
          kind: event.kind,
          url: event.url,
          method: event.method,
          headers: redactHeaders(event.headers),
          body: event.body,
        };
      }
      return {
        kind: event.kind,
        url: event.url,
        status: event.status,
        statusText: event.statusText,
        headers: redactHeaders(event.headers),
        bodyPreview: event.body?.preview,
        elapsedMs: event.elapsedMs,
        error: event.error,
      };
    });
}

async function main() {
  const config = parseArgs(process.argv.slice(2));
  mkdirSync(config.outputDir, { recursive: true });
  const results = [];

  for (let index = 0; index < config.proxies.length; index += 1) {
    const proxy = config.proxies[index];
    const label = proxy === "direct" ? "direct" : `proxy-${index + 1}`;
    const runDir = resolve(config.outputDir, label);
    const result = await runOne(config, proxy, runDir);
    result.captureSummary = summarizeCapture(result.captureLog);
    results.push(result);
  }

  const summaryPath = resolve(config.outputDir, "summary.json");
  writeFileSync(summaryPath, `${JSON.stringify({ config: publicConfig(config), results }, null, 2)}\n`, "utf8");
  console.log(JSON.stringify({ outputDir: config.outputDir, summaryPath, results }, null, 2));
}

main().catch((error) => {
  console.error(error);
  process.exit(1);
});
