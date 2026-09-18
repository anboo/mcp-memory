"use strict";

const assert = require("assert");
const fs = require("fs");
const os = require("os");
const path = require("path");
const { parse } = require("jsonc-parser");

const { writeMemoryEntry } = require("../lib/config");
const { assetName } = require("../lib/download");
const { target } = require("../lib/platform");

// Release asset naming must match the workflow.
assert.strictEqual(
  assetName("1.2.3", "linux", "amd64"),
  "opencode-memory-mcp_1.2.3_linux_amd64.tar.gz"
);
assert.strictEqual(
  assetName("1.2.3", "windows", "arm64"),
  "opencode-memory-mcp_1.2.3_windows_arm64.tar.gz"
);

// target resolves on supported CI platforms.
const t = target();
assert.ok(t.goos && t.goarch, "target should resolve");

const dir = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-memory-test-"));
const file = path.join(dir, "opencode.jsonc");

// A missing file is created with the memory entry.
const first = writeMemoryEntry(file);
assert.ok(first.changed, "first write should change the file");
let doc = parse(fs.readFileSync(file, "utf8"));
assert.strictEqual(doc.mcp.memory.type, "local");
assert.deepStrictEqual(doc.mcp.memory.command, ["npx", "-y", "opencode-memory-mcp"]);

// Re-running keeps the existing entry unless forced.
const second = writeMemoryEntry(file);
assert.strictEqual(second.changed, false, "second write should be a no-op");

// Comments and unrelated servers survive a forced merge.
fs.writeFileSync(
  file,
  `{
  // keep this comment
  "mcp": {
    "jira": { "type": "local", "command": ["npx", "-y", "x"] }
  }
}
`
);
const third = writeMemoryEntry(file, { force: true });
assert.ok(third.changed, "forced write should change the file");
const text = fs.readFileSync(file, "utf8");
assert.ok(text.includes("// keep this comment"), "comment should be preserved");
doc = parse(text);
assert.ok(doc.mcp.jira, "unrelated server should be preserved");
assert.ok(doc.mcp.memory, "memory entry should be present");

fs.rmSync(dir, { recursive: true, force: true });
process.stdout.write("ok\n");
