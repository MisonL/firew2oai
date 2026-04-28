#!/usr/bin/env node

const resourceUri = "fixture://firew2oai/readme";
const resourceText = "fixture-ok: firew2oai MCP resource fixture";

function send(message) {
  process.stdout.write(`${JSON.stringify(message)}\n`);
}

function result(id, value) {
  send({ jsonrpc: "2.0", id, result: value });
}

function error(id, code, message) {
  send({ jsonrpc: "2.0", id, error: { code, message } });
}

function handleRequest(request) {
  if (request.method === "initialize") {
    result(request.id, {
      protocolVersion: request.params?.protocolVersion || "2024-11-05",
      capabilities: { resources: {} },
      serverInfo: { name: "firew2oai-resource-fixture", version: "0.1.0" },
    });
    return;
  }
  if (request.method === "resources/list") {
    result(request.id, {
      resources: [
        {
          uri: resourceUri,
          name: "firew2oai fixture resource",
          description: "A deterministic resource used by Codex built-in resource tests.",
          mimeType: "text/plain",
        },
      ],
    });
    return;
  }
  if (request.method === "resources/templates/list") {
    result(request.id, { resourceTemplates: [] });
    return;
  }
  if (request.method === "resources/read") {
    const uri = request.params?.uri;
    if (uri !== resourceUri) {
      error(request.id, -32002, `Unknown resource URI: ${uri}`);
      return;
    }
    result(request.id, {
      contents: [{ uri: resourceUri, mimeType: "text/plain", text: resourceText }],
    });
    return;
  }
  if (request.method === "tools/list") {
    result(request.id, { tools: [] });
    return;
  }
  if (request.method === "prompts/list") {
    result(request.id, { prompts: [] });
    return;
  }
  if (request.id !== undefined) {
    error(request.id, -32601, `Method not found: ${request.method}`);
  }
}

let buffer = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", (chunk) => {
  buffer += chunk;
  let newlineIndex = buffer.indexOf("\n");
  while (newlineIndex !== -1) {
    const line = buffer.slice(0, newlineIndex).trim();
    buffer = buffer.slice(newlineIndex + 1);
    newlineIndex = buffer.indexOf("\n");
    if (!line) {
      continue;
    }
    try {
      handleRequest(JSON.parse(line));
    } catch (err) {
      error(null, -32700, String(err));
    }
  }
});
