const sessionKey = "roborev.web.session";
const csrfKey = "roborev.web.csrf";

const sessionHeader = "X-Roborev-Web-Session";
const csrfHeader = "X-Roborev-CSRF";
export const sessionLostEvent = "roborev:web-session-lost";

export type Fetch = (
  input: RequestInfo | URL,
  init?: RequestInit,
) => Promise<Response>;

export type SessionResult =
  | {
      state: "authenticated";
      expiresAt: string;
      capabilities: SessionCapabilities;
    }
  | { state: "login-required" }
  | { state: "rate-limited"; retryAfterSeconds: number }
  | { state: "error"; message: string };

export interface SessionCapabilities {
  cancelAnyJob: boolean;
  cancelReviewJob: boolean;
  rerunJob: boolean;
}

interface CredentialBody {
  session: string;
  csrf: string;
  expires_at: string;
  capabilities: {
    cancel_any_job: boolean;
    cancel_review_job: boolean;
    rerun_job: boolean;
  };
}

export function clearTabSession(): void {
  sessionStorage.removeItem(sessionKey);
  sessionStorage.removeItem(csrfKey);
}

export function sessionHeaders(): Record<string, string> {
  const session = sessionStorage.getItem(sessionKey);
  const csrf = sessionStorage.getItem(csrfKey);
  if (session === null || csrf === null) {
    return {};
  }
  return { [sessionHeader]: session, [csrfHeader]: csrf };
}

export function authenticatedFetch(
  fetchImpl: Fetch = globalThis.fetch.bind(globalThis),
): Fetch {
  return async (input, init) => {
    const request =
      input instanceof Request
        ? new Request(input, { ...init, credentials: "same-origin" })
        : new Request(new URL(input, globalThis.location.origin), {
            ...init,
            credentials: "same-origin",
          });
    const headers = new Headers(request.headers);
    const session = sessionStorage.getItem(sessionKey);
    if (session !== null) {
      headers.set(sessionHeader, session);
    }
    if (request.method !== "GET" && request.method !== "HEAD") {
      const csrf = sessionStorage.getItem(csrfKey);
      if (csrf !== null) {
        headers.set(csrfHeader, csrf);
      }
      const explicitHeaders = new Headers(
        init?.headers ?? (input instanceof Request ? input.headers : undefined),
      );
      if (
        typeof init?.body === "string" &&
        !explicitHeaders.has("Content-Type")
      ) {
        headers.set("Content-Type", "application/json");
      }
    }

    const response = await fetchImpl(new Request(request, { headers }));
    if (
      response.status === 401 &&
      session !== null &&
      sessionStorage.getItem(sessionKey) === session
    ) {
      clearTabSession();
      globalThis.dispatchEvent(new Event(sessionLostEvent));
    }
    return response;
  };
}

export async function bootstrapSession(
  fetchImpl: Fetch = fetch,
): Promise<SessionResult> {
  return exchangeCredentials(fetchImpl, "/api/ui/session/bootstrap", "{}");
}

export async function login(
  token: string,
  fetchImpl: Fetch = fetch,
): Promise<SessionResult> {
  return exchangeCredentials(
    fetchImpl,
    "/api/ui/session/login",
    JSON.stringify({ token }),
  );
}

export async function logout(fetchImpl: Fetch = fetch): Promise<void> {
  try {
    const response = await fetchImpl("/api/ui/session", {
      method: "DELETE",
      credentials: "same-origin",
      redirect: "error",
      headers: sessionHeaders(),
    });
    if (!response.ok && response.status !== 401) {
      throw new Error(`logout failed with status ${response.status}`);
    }
  } finally {
    clearTabSession();
  }
}

async function exchangeCredentials(
  fetchImpl: Fetch,
  path: string,
  body: string,
): Promise<SessionResult> {
  let response: Response;
  try {
    response = await fetchImpl(path, {
      method: "POST",
      credentials: "same-origin",
      redirect: "error",
      headers: { "Content-Type": "application/json" },
      body,
    });
  } catch (error) {
    return {
      state: "error",
      message:
        error instanceof Error ? error.message : "session request failed",
    };
  }
  if (response.status === 401) {
    clearTabSession();
    return { state: "login-required" };
  }
  if (response.status === 429) {
    const retryAfter = Number.parseInt(
      response.headers.get("Retry-After") ?? "",
      10,
    );
    return {
      state: "rate-limited",
      retryAfterSeconds:
        Number.isSafeInteger(retryAfter) && retryAfter > 0 ? retryAfter : 1,
    };
  }
  if (!response.ok) {
    return {
      state: "error",
      message: `session request failed (${response.status})`,
    };
  }

  let bodyValue: unknown;
  try {
    bodyValue = await response.json();
  } catch {
    return { state: "error", message: "session response was not valid JSON" };
  }
  if (!isCredentialBody(bodyValue)) {
    return { state: "error", message: "session response was incomplete" };
  }
  sessionStorage.setItem(sessionKey, bodyValue.session);
  sessionStorage.setItem(csrfKey, bodyValue.csrf);
  return {
    state: "authenticated",
    expiresAt: bodyValue.expires_at,
    capabilities: {
      cancelAnyJob: bodyValue.capabilities.cancel_any_job,
      cancelReviewJob: bodyValue.capabilities.cancel_review_job,
      rerunJob: bodyValue.capabilities.rerun_job,
    },
  };
}

function isCredentialBody(value: unknown): value is CredentialBody {
  if (typeof value !== "object" || value === null) {
    return false;
  }
  const candidate = value as Record<string, unknown>;
  return (
    typeof candidate.session === "string" &&
    candidate.session.length > 0 &&
    typeof candidate.csrf === "string" &&
    candidate.csrf.length > 0 &&
    typeof candidate.expires_at === "string" &&
    candidate.expires_at.length > 0 &&
    typeof candidate.capabilities === "object" &&
    candidate.capabilities !== null &&
    typeof (candidate.capabilities as Record<string, unknown>)
      .cancel_any_job === "boolean" &&
    typeof (candidate.capabilities as Record<string, unknown>)
      .cancel_review_job === "boolean" &&
    typeof (candidate.capabilities as Record<string, unknown>).rerun_job ===
      "boolean"
  );
}
