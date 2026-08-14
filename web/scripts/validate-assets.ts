import { lstat, readFile, readdir } from "node:fs/promises";
import {
  basename,
  extname,
  join,
  posix,
  relative,
  resolve,
  sep,
} from "node:path";

export const productionDistributionMarker =
  '<meta name="roborev-web-distribution" content="production"';

interface ManifestEntry {
  file?: string;
  css?: string[];
  assets?: string[];
}

const secretNames = new Set([
  "credential",
  "credentials",
  "secret",
  "secrets",
  "token",
  "tokens",
  "password",
  "passwd",
  "id_rsa",
  "id_ed25519",
]);
const secretExtensions = new Set([".key", ".pem", ".p12", ".pfx"]);

export async function validateAssetGraph(root: string): Promise<void> {
  const resolvedRoot = resolve(root);
  await rejectUnsafeFilesystemEntries(resolvedRoot, resolvedRoot);
  const index = await readFile(join(resolvedRoot, "index.html"), "utf8");
  if (!index.includes(productionDistributionMarker)) {
    throw new Error("web index is missing the production marker");
  }

  const manifestText = await readFile(
    join(resolvedRoot, ".vite/manifest.json"),
    "utf8",
  );
  const manifest = JSON.parse(manifestText) as Record<string, ManifestEntry>;
  if (Object.keys(manifest).length === 0) {
    throw new Error("vite manifest is empty");
  }

  for (const entry of Object.values(manifest)) {
    const assets = [entry.file, ...(entry.css ?? []), ...(entry.assets ?? [])];
    for (const asset of assets) {
      if (!asset) {
        throw new Error("vite manifest entry is missing its output file");
      }
      validateManifestPath(asset);
      const fullPath = resolve(resolvedRoot, ...asset.split("/"));
      if (relative(resolvedRoot, fullPath).startsWith(`..${sep}`)) {
        throw new Error(`manifest asset escapes distribution: ${asset}`);
      }
      const info = await lstat(fullPath);
      if (info.isSymbolicLink()) {
        throw new Error(`manifest asset is a symlink: ${asset}`);
      }
      if (!info.isFile()) {
        throw new Error(`manifest asset is not a regular file: ${asset}`);
      }
    }
  }
}

function validateManifestPath(asset: string): void {
  if (
    asset === "" ||
    asset.includes("\\") ||
    asset.startsWith("/") ||
    posix.normalize(asset) !== asset ||
    asset.split("/").some((segment) => segment.startsWith("."))
  ) {
    throw new Error(`invalid manifest asset path: ${asset}`);
  }
  const name = basename(asset).toLowerCase();
  const extension = extname(name);
  const stem = name.slice(0, name.length - extension.length);
  if (secretNames.has(stem) || secretExtensions.has(extension)) {
    throw new Error(`secret-like manifest asset path: ${asset}`);
  }
}

async function rejectUnsafeFilesystemEntries(
  root: string,
  directory: string,
): Promise<void> {
  for (const entry of await readdir(directory, { withFileTypes: true })) {
    const fullPath = join(directory, entry.name);
    if (entry.isSymbolicLink()) {
      throw new Error(
        `distribution contains a symlink: ${relative(root, fullPath)}`,
      );
    }
    if (entry.isDirectory()) {
      await rejectUnsafeFilesystemEntries(root, fullPath);
      continue;
    }
    const asset = relative(root, fullPath).split(sep).join("/");
    if (!entry.isFile()) {
      throw new Error(`distribution entry is not a regular file: ${asset}`);
    }
    if (asset !== "index.html" && asset !== ".vite/manifest.json") {
      validateManifestPath(asset);
    }
  }
}

async function main(): Promise<void> {
  const [root, ...extra] = process.argv.slice(2);
  if (!root || extra.length > 0) {
    console.error("usage: validate-assets.ts <distribution>");
    process.exitCode = 2;
    return;
  }
  await validateAssetGraph(resolve(root));
}

if (import.meta.main) {
  await main();
}
