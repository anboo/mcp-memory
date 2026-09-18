#!/usr/bin/env node
"use strict";

const { run } = require("../lib/cli");

run(process.argv.slice(2)).then(
  (code) => {
    process.exitCode = code;
  },
  (err) => {
    process.stderr.write(`opencode-memory-mcp: ${err.message}\n`);
    process.exitCode = 1;
  }
);
