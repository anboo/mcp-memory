"use strict";

const os = require("os");
const path = require("path");

// Mapping from Node's process.platform/process.arch to the Go GOOS/GOARCH
// names used in release asset filenames.
const GOOS = { linux: "linux", darwin: "darwin", win32: "windows" };
const GOARCH = { x64: "amd64", arm64: "arm64" };

// target resolves the release platform for the current process.
function target() {
  const goos = GOOS[process.platform];
  const goarch = GOARCH[process.arch];
  if (!goos) {
    throw new Error(
      `mcp-memory: unsupported platform "${process.platform}"; ` +
        "build from source instead: https://github.com/anboo/mcp-memory"
    );
  }
  if (!goarch) {
    throw new Error(
      `mcp-memory: unsupported architecture "${process.arch}"; ` +
        "build from source instead: https://github.com/anboo/mcp-memory"
    );
  }
  return { goos, goarch };
}

// binaryName appends the Windows executable suffix when needed.
function binaryName(cmd) {
  return process.platform === "win32" ? `${cmd}.exe` : cmd;
}

// cacheDir is where the extracted binaries live. One directory per version,
// so upgrades never race with a running server.
function cacheDir(version) {
  if (process.env.MCP_MEMORY_CACHE) {
    return path.resolve(process.env.MCP_MEMORY_CACHE);
  }
  if (process.platform === "win32") {
    const base = process.env.LOCALAPPDATA || path.join(os.homedir(), "AppData", "Local");
    return path.join(base, "mcp-memory", version);
  }
  return path.join(os.homedir(), ".cache", "mcp-memory", version);
}

module.exports = { target, binaryName, cacheDir };
