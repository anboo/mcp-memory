"use strict";

const fs = require("fs");
const os = require("os");
const path = require("path");
const { parse, modify, applyEdits } = require("jsonc-parser");

// MEMORY_ENTRY is the MCP server block written into opencode.json. It uses
// npx so the config never contains an absolute path to a binary.
const MEMORY_ENTRY = {
  type: "local",
  command: ["npx", "-y", "opencode-memory-mcp"],
  enabled: true,
  timeout: 20000,
};

// configDir is the OpenCode global config directory.
function configDir() {
  if (process.env.OPENCODE_CONFIG_DIR) {
    return process.env.OPENCODE_CONFIG_DIR;
  }
  const xdg = process.env.XDG_CONFIG_HOME;
  const base = xdg ? xdg : path.join(os.homedir(), ".config");
  return path.join(base, "opencode");
}

// candidatePaths lists the files to consider, preferring the one the user
// already maintains.
function candidatePaths() {
  if (process.env.OPENCODE_CONFIG) {
    return [process.env.OPENCODE_CONFIG];
  }
  const dir = configDir();
  return [path.join(dir, "opencode.jsonc"), path.join(dir, "opencode.json")];
}

// resolve returns the config file to edit: an existing one, otherwise the
// default opencode.json.
function resolve() {
  const list = candidatePaths();
  for (const p of list) {
    if (fs.existsSync(p)) {
      return p;
    }
  }
  return list[list.length - 1];
}

function defaultText() {
  return JSON.stringify({ $schema: "https://opencode.ai/config.json" }, null, 2) + "\n";
}

// writeMemoryEntry merges mcp.memory into the config file, preserving
// comments and formatting elsewhere. When the entry already exists it is kept
// unless force is set.
function writeMemoryEntry(file, { force = false } = {}) {
  const exists = fs.existsSync(file);
  const text = exists ? fs.readFileSync(file, "utf8") : defaultText();

  const errors = [];
  const doc = parse(text, errors, { allowTrailingComma: true });
  if (errors.length > 0) {
    throw new Error(`cannot parse ${file}; edit it by hand or fix the JSON first`);
  }

  const existing = doc && doc.mcp && doc.mcp.memory;
  if (existing && !force) {
    return { file, changed: false, existing };
  }

  const edits = modify(text, ["mcp", "memory"], MEMORY_ENTRY, {
    formattingOptions: { insertSpaces: true, tabSize: 2, eol: "\n" },
  });
  const out = applyEdits(text, edits);
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, out);
  return { file, changed: true, existing: existing || null };
}

module.exports = { MEMORY_ENTRY, configDir, candidatePaths, resolve, writeMemoryEntry };
