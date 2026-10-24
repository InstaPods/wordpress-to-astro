// Stages the npm packages for a release from the binaries in dist/:
//   node scripts/npm-packages.mjs v0.1.0
// writes npm/dist/<package>/ for the six platform packages and the main one.
import { cpSync, mkdirSync, readFileSync, writeFileSync, chmodSync, rmSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const version = (process.argv[2] || "").replace(/^v/, "");
if (!/^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/.test(version)) {
  console.error("usage: node scripts/npm-packages.mjs vX.Y.Z");
  process.exit(2);
}

const targets = [
  ["darwin", "arm64", "darwin-arm64"],
  ["darwin", "x64", "darwin-amd64"],
  ["linux", "arm64", "linux-arm64"],
  ["linux", "x64", "linux-amd64"],
  ["win32", "arm64", "windows-arm64"],
  ["win32", "x64", "windows-amd64"],
];

const out = join(root, "npm", "dist");
rmSync(out, { recursive: true, force: true });
const main = JSON.parse(readFileSync(join(root, "npm", "wordpress-to-astro", "package.json"), "utf8"));
main.version = version;
main.optionalDependencies = {};

for (const [os, cpu, goDir] of targets) {
  const name = `wordpress-to-astro-${os}-${cpu}`;
  const exe = os === "win32" ? "wordpress-to-astro.exe" : "wordpress-to-astro";
  const dir = join(out, name);
  mkdirSync(join(dir, "bin"), { recursive: true });
  cpSync(join(root, "dist", goDir, exe), join(dir, "bin", exe));
  if (os !== "win32") chmodSync(join(dir, "bin", exe), 0o755);
  cpSync(join(root, "LICENSE"), join(dir, "LICENSE"));
  writeFileSync(join(dir, "README.md"),
    `# ${name}\n\nThe ${os} ${cpu} binary for [wordpress-to-astro](https://github.com/InstaPods/wordpress-to-astro). Install \`wordpress-to-astro\` instead; it picks this package for you.\n`);
  writeFileSync(join(dir, "package.json"), JSON.stringify({
    name,
    version,
    description: `The ${os} ${cpu} binary for wordpress-to-astro.`,
    repository: main.repository,
    homepage: main.homepage,
    license: "MIT",
    os: [os],
    cpu: [cpu],
    files: ["bin/"],
    preferUnplugged: true,
  }, null, 2) + "\n");
  main.optionalDependencies[name] = version;
}

const mainDir = join(out, "wordpress-to-astro");
cpSync(join(root, "npm", "wordpress-to-astro"), mainDir, { recursive: true });
chmodSync(join(mainDir, "bin", "wordpress-to-astro.js"), 0o755);
cpSync(join(root, "README.md"), join(mainDir, "README.md"));
cpSync(join(root, "LICENSE"), join(mainDir, "LICENSE"));
writeFileSync(join(mainDir, "package.json"), JSON.stringify(main, null, 2) + "\n");
console.log(`staged npm packages for ${version} in npm/dist/`);
