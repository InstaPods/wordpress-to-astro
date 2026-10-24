#!/usr/bin/env node
// Runs the prebuilt wordpress-to-astro binary for this platform. npm installs
// exactly one of the optional wordpress-to-astro-<os>-<arch> packages.
"use strict";

const { spawn } = require("child_process");
const path = require("path");

const platforms = {
  "darwin arm64": "wordpress-to-astro-darwin-arm64",
  "darwin x64": "wordpress-to-astro-darwin-x64",
  "linux arm64": "wordpress-to-astro-linux-arm64",
  "linux x64": "wordpress-to-astro-linux-x64",
  "win32 arm64": "wordpress-to-astro-win32-arm64",
  "win32 x64": "wordpress-to-astro-win32-x64",
};

function binaryPath() {
  const key = `${process.platform} ${process.arch}`;
  const pkg = platforms[key];
  if (!pkg) {
    fail(`there is no prebuilt binary for ${key}.`);
  }
  const exe = process.platform === "win32" ? "wordpress-to-astro.exe" : "wordpress-to-astro";
  try {
    return require.resolve(path.posix.join(pkg, "bin", exe));
  } catch (e) {
    fail(`the package ${pkg} is not installed. It is an optional dependency, so it is skipped when installing with --no-optional or --omit=optional.`);
  }
}

function fail(why) {
  console.error(`wordpress-to-astro: ${why}
Install it another way:
  go install github.com/instapods/wordpress-to-astro@latest
or download a binary from https://github.com/InstaPods/wordpress-to-astro/releases`);
  process.exit(1);
}

const child = spawn(binaryPath(), process.argv.slice(2), { stdio: "inherit" });
// Pass Ctrl-C and friends on, and stay alive until the binary has exited, so
// it never outlives this process.
for (const sig of ["SIGINT", "SIGTERM", "SIGHUP"]) {
  process.on(sig, () => {
    try {
      child.kill(sig);
    } catch (e) {}
  });
}
child.on("error", (err) => {
  console.error(`wordpress-to-astro: ${err.message}`);
  process.exit(1);
});
child.on("exit", (code, signal) => {
  if (signal) {
    process.removeAllListeners(signal);
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code === null ? 1 : code);
});
