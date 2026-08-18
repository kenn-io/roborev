const basePathMetaName = "roborev-base-path";

export function getBasePath(): string {
  const raw = document
    .querySelector<HTMLMetaElement>(`meta[name="${basePathMetaName}"]`)
    ?.content.trim();
  if (raw === undefined || raw === "") return "";
  if (
    !raw.startsWith("/") ||
    raw.endsWith("/") ||
    raw.includes("?") ||
    raw.includes("#") ||
    raw.split("/").some((segment) => segment === "." || segment === "..")
  ) {
    return "";
  }
  return raw;
}

export function appPath(path: string): string {
  const internalPath =
    path === "" ? "/" : path.startsWith("/") ? path : `/${path}`;
  const basePath = getBasePath();
  if (basePath === "") return internalPath;
  return internalPath === "/" ? `${basePath}/` : `${basePath}${internalPath}`;
}

export function stripBasePath(pathname: string): string {
  const basePath = getBasePath();
  if (basePath === "") return pathname;
  if (pathname === basePath) return "/";
  const prefix = `${basePath}/`;
  return pathname.startsWith(prefix) ? pathname.slice(basePath.length) : "";
}
