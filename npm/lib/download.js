"use strict";

const crypto = require("crypto");
const fs = require("fs");
const os = require("os");
const path = require("path");
const tar = require("tar");

const { target, binaryName, cacheDir } = require("./platform");

const DEFAULT_REPO = "anboo/opencode-memory-mcp";
const BIN_NAME = "opencode-memory-mcp";

// releaseBase is the base URL for a version's release assets. It can be
// pointed at a mirror or a local file server for testing.
function releaseBase(version) {
  if (process.env.OPENCODE_MEMORY_BASE_URL) {
    return `${process.env.OPENCODE_MEMORY_BASE_URL.replace(/\/$/, "")}/v${version}`;
  }
  const repo = (process.env.OPENCODE_MEMORY_REPO || DEFAULT_REPO).replace(/^\/|\/$/g, "");
  return `https://github.com/${repo}/releases/download/v${version}`;
}

// assetName mirrors the naming used by .github/workflows/release.yml.
function assetName(version, goos, goarch) {
  return `opencode-memory-mcp_${version}_${goos}_${goarch}.tar.gz`;
}

function log(message) {
  process.stderr.write(`opencode-memory-mcp: ${message}\n`);
}

function sha256(file) {
  return crypto.createHash("sha256").update(fs.readFileSync(file)).digest("hex");
}

async function fetchOk(url) {
  const res = await fetch(url, { redirect: "follow" });
  if (!res.ok) {
    throw new Error(`GET ${url} failed: ${res.status} ${res.statusText}`);
  }
  return res;
}

async function downloadTo(url, dest) {
  const res = await fetchOk(url);
  const buf = Buffer.from(await res.arrayBuffer());
  fs.writeFileSync(dest, buf);
}

// verifyChecksum compares the archive against checksums.txt from the same
// release. A missing checksums file is a warning, not a failure, so older
// releases remain installable.
async function verifyChecksum(file, asset, base) {
  let text;
  try {
    text = await (await fetchOk(`${base}/checksums.txt`)).text();
  } catch (err) {
    log(`warning: could not fetch checksums.txt (${err.message}); skipping verification`);
    return;
  }
  const line = text
    .split("\n")
    .map((l) => l.trim())
    .find((l) => l && l.split(/\s+/).pop() === asset);
  if (!line) {
    log(`warning: ${asset} not listed in checksums.txt; skipping verification`);
    return;
  }
  const expected = line.split(/\s+/)[0];
  const actual = sha256(file);
  if (expected !== actual) {
    throw new Error(`checksum mismatch for ${asset}: expected ${expected}, got ${actual}`);
  }
}

// ensure returns the path to the single binary, downloading and extracting the
// release archive on the first call for a version.
async function ensure(version) {
  const { goos, goarch } = target();
  const dir = cacheDir(version);
  const bin = path.join(dir, binaryName(BIN_NAME));

  if (fs.existsSync(bin)) {
    return { dir, bin, goos, goarch };
  }

  const asset = assetName(version, goos, goarch);
  const base = releaseBase(version);
  fs.mkdirSync(dir, { recursive: true });

  const tmp = path.join(os.tmpdir(), `opencode-memory-${process.pid}-${Date.now()}-${asset}`);
  try {
    log(`downloading ${asset} (${goos}/${goarch})`);
    await downloadTo(`${base}/${asset}`, tmp);
    await verifyChecksum(tmp, asset, base);
    await tar.x({ file: tmp, cwd: dir });
  } finally {
    fs.rmSync(tmp, { force: true });
  }

  if (!fs.existsSync(bin)) {
    throw new Error(`${binaryName(BIN_NAME)} is missing from ${asset}`);
  }
  if (process.platform !== "win32") {
    fs.chmodSync(bin, 0o755);
  }

  log(`installed into ${dir}`);
  return { dir, bin, goos, goarch };
}

module.exports = { ensure, assetName, releaseBase, cacheDir, BIN_NAME };
