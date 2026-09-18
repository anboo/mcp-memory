"use strict";

const { spawn } = require("child_process");

const pkg = require("../package.json");
const { ensure } = require("./download");
const { resolve, writeMemoryEntry } = require("./config");

const help = `mcp-memory - agent memory over the OpenCode session history

Usage: npx -y @anboo/mcp-memory [command]

Commands:
  install          Download the binary and add the "memory" MCP server to
                   your global opencode.json. Run this first.
  index [flags]    Build or update the local search index.
  serve            Run the MCP server over stdio (default when launched by an
                   MCP client). It is also the default with no arguments.
  sessions         Print a summary of the whole OpenCode database.
  session <id>     Dump the dialog of one session.
  version          Print the version.
  help             Print this help.

Environment:
  MCP_MEMORY_VERSION   Release version to download (default: this
                            package's version).
  MCP_MEMORY_REPO      GitHub repo "owner/name" to download from.
  MCP_MEMORY_BASE_URL  Full base URL for release assets (mirror).
  MCP_MEMORY_CACHE     Directory to extract the binary into.
  OPENCODE_CONFIG           opencode.json path for "install".
`;

// resolveVersion is the release version to download.
function resolveVersion() {
  const override = process.env.MCP_MEMORY_VERSION;
  if (override) {
    return override.replace(/^v/, "");
  }
  if (!/^\d+\.\d+\.\d+$/.test(pkg.version)) {
    throw new Error(
      `this is a development build (${pkg.version}); ` +
        "set MCP_MEMORY_VERSION to a released version or build from source"
    );
  }
  return pkg.version;
}

// runBinary downloads the binary if needed and runs it with inherited stdio.
function runBinary(args) {
  return ensure(resolveVersion()).then(
    ({ bin }) =>
      new Promise((resolvePromise, reject) => {
        const child = spawn(bin, args, { stdio: "inherit" });
        child.on("error", reject);
        child.on("exit", (code, signal) => {
          if (signal) {
            process.kill(process.pid, signal);
            resolvePromise(1);
            return;
          }
          resolvePromise(code == null ? 1 : code);
        });
      })
  );
}

// install downloads the binary and merges the MCP entry into opencode.json.
async function install(args) {
  const force = args.includes("--force");
  const { bin } = await ensure(resolveVersion());

  const file = resolve();
  const result = writeMemoryEntry(file, { force });

  if (result.changed) {
    console.log(`Added the "memory" MCP server to ${file}`);
  } else {
    console.log(`The "memory" MCP server is already configured in ${file}`);
    console.log("Use install --force to overwrite it.");
  }
  console.log(`Binary: ${bin}`);
  console.log("");
  console.log("Restart OpenCode, then build the index once:");
  console.log("  npx -y @anboo/mcp-memory index");
  return 0;
}

// run executes one CLI invocation and returns the process exit code.
async function run(argv) {
  const [cmd, ...rest] = argv;

  if (!cmd) {
    if (process.stdin.isTTY) {
      process.stdout.write(help);
      return 0;
    }
    return runBinary(["serve"]);
  }

  switch (cmd) {
    case "install":
      return install(rest);
    case "serve":
    case "mcp":
      return runBinary(["serve", ...rest]);
    case "index":
      return runBinary(["index", ...rest]);
    case "sessions":
    case "all":
      return runBinary(["sessions", ...rest]);
    case "session":
      return runBinary(["session", ...rest]);
    case "version":
    case "--version":
    case "-v":
      console.log(pkg.version);
      return 0;
    case "help":
    case "--help":
    case "-h":
      process.stdout.write(help);
      return 0;
    default:
      process.stderr.write(`unknown command "${cmd}"\n\n`);
      process.stderr.write(help);
      return 2;
  }
}

module.exports = { run, resolveVersion, help };
