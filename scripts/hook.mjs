#!/usr/bin/env node
// Cross-platform hook launcher for Linux / macOS / Windows (works under both sh -c and cmd /c).
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const event = process.argv[2] || "";
const fallback =
  event === "pre-tool-use"
    ? '{"decision":"allow"}\n'
    : event === "stop"
      ? '{"decision":"stop"}\n'
      : "{}\n";

const pluginRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const binDir = path.join(pluginRoot, "bin");
const ext = process.platform === "win32" ? ".exe" : "";
const goOS = process.platform === "win32" ? "windows" : process.platform;
const goArch = process.arch === "x64" ? "amd64" : process.arch === "ia32" ? "386" : process.arch;

const candidates = [
  path.join(binDir, `bswarm-${goOS}-${goArch}${ext}`),
  path.join(binDir, `bswarm${ext}`),
  `bswarm${ext}`,
];

let stdinBuf = Buffer.alloc(0);
try {
  stdinBuf = fs.readFileSync(0);
} catch {
  // empty stdin
}

for (const c of candidates) {
  if (c.includes(path.sep)) {
    try {
      if (!fs.statSync(c).isFile()) continue;
      if (process.platform !== "win32") fs.accessSync(c, fs.constants.X_OK);
    } catch {
      continue;
    }
  }
  try {
    const res = spawnSync(c, ["hook", ...process.argv.slice(2)], {
      input: stdinBuf,
      stdio: ["pipe", "pipe", "ignore"],
      timeout: 3500,
      windowsHide: true,
    });
    if (!res.error && res.status === 0 && res.stdout && res.stdout.length > 0) {
      process.stdout.write(res.stdout);
      process.exit(0);
    }
  } catch {
    // try next candidate
  }
}

process.stdout.write(fallback);
