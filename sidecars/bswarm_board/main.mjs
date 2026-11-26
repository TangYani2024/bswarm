#!/usr/bin/env node
// Cross-platform sidecar launcher for bswarm_board (invoked via Desktop's bundled agy-node or system node).
import { spawn } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const sidecarDir = path.dirname(fileURLToPath(import.meta.url));
const pluginRoot = path.resolve(sidecarDir, "..", "..");
const binDir = path.join(pluginRoot, "bin");
const ext = process.platform === "win32" ? ".exe" : "";
const goOS = process.platform === "win32" ? "windows" : process.platform;
const goArch = process.arch === "x64" ? "amd64" : process.arch === "ia32" ? "386" : process.arch;

const candidates = [
  path.join(binDir, `bswarm-${goOS}-${goArch}${ext}`),
  path.join(binDir, `bswarm${ext}`),
  `bswarm${ext}`,
];

let chosen = "";
for (const c of candidates) {
  if (c.includes(path.sep)) {
    try {
      if (!fs.statSync(c).isFile()) continue;
      if (process.platform !== "win32") fs.accessSync(c, fs.constants.X_OK);
      chosen = c;
      break;
    } catch {
      continue;
    }
  } else {
    chosen = c;
    break;
  }
}

const child = spawn(chosen, ["ui-sidecar", ...process.argv.slice(2)], {
  stdio: "inherit",
  env: process.env,
  windowsHide: true,
});

for (const sig of ["SIGINT", "SIGTERM"]) {
  process.on(sig, () => {
    try {
      child.kill(sig);
    } catch {
      // ignore
    }
  });
}

child.on("error", (err) => {
  console.error(`[bswarm_board] failed to start ${chosen}: ${err.message}`);
  process.exit(1);
});

child.on("exit", (code, signal) => {
  if (signal) process.exit(1);
  process.exit(code ?? 0);
});
