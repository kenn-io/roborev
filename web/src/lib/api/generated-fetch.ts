import { appPath, stripBasePath } from "../base-path";
import { authenticatedFetch, type Fetch } from "./session";

export interface RoborevTransport {
  readonly baseUrl: string;
  readonly fetch: Fetch;
}

export interface RoborevRequestOptions extends RequestInit {
  readonly roborevTransport?: RoborevTransport;
}

function resolveBaseUrl(baseUrl: string): string {
  if (!baseUrl.startsWith("/")) return baseUrl;
  return stripBasePath(baseUrl) === "" ? appPath(baseUrl) : baseUrl;
}

export function roborevFetch<T>(
  path: string,
  options?: RoborevRequestOptions,
): Promise<T>;
export async function roborevFetch(
  path: string,
  options: RoborevRequestOptions = {},
): Promise<unknown> {
  const { roborevTransport, ...init } = options;
  const baseUrl = resolveBaseUrl(roborevTransport?.baseUrl ?? "/");
  const fetchFn = roborevTransport?.fetch ?? globalThis.fetch.bind(globalThis);
  const url = new URL(
    `${baseUrl.replace(/\/$/, "")}${path}`,
    globalThis.location.origin,
  );
  const response = await authenticatedFetch(fetchFn)(new Request(url, init));
  if (!response.ok) throw response;
  if ([204, 205, 304].includes(response.status)) return undefined;

  const body = await response.text();
  if (body === "") return undefined;
  if (response.headers.get("Content-Type")?.includes("json")) {
    return JSON.parse(body);
  }
  return body;
}
