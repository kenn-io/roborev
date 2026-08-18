import { appPath, stripBasePath } from "../base-path";

export type AppRoute =
  | { page: "reviews"; jobId?: number }
  | { page: "analytics" };

export function parseRoute(pathname: string): AppRoute {
  const normalized = pathname.replace(/\/+$/, "") || "/";
  if (normalized === "/analytics") {
    return { page: "analytics" };
  }
  const review = normalized.match(/^\/reviews\/(\d+)$/);
  if (review !== null) {
    const jobId = Number(review[1]);
    if (Number.isSafeInteger(jobId) && jobId > 0) {
      return { page: "reviews", jobId };
    }
  }
  return { page: "reviews" };
}

export function createRouter() {
  let route = $state<AppRoute>(
    parseRoute(stripBasePath(globalThis.location.pathname)),
  );

  const publishLocation = (): void => {
    route = parseRoute(stripBasePath(globalThis.location.pathname));
  };
  globalThis.addEventListener("popstate", publishLocation);

  function navigate(path: string, replace = false): void {
    const target = appPath(path);
    if (globalThis.location.pathname !== target) {
      if (replace) {
        globalThis.history.replaceState(null, "", target);
      } else {
        globalThis.history.pushState(null, "", target);
      }
    }
    publishLocation();
  }

  return {
    getRoute: () => route,
    navigateToReview: (jobId?: number, options: { replace?: boolean } = {}) =>
      navigate(
        jobId === undefined ? "/reviews" : `/reviews/${jobId}`,
        options.replace,
      ),
    navigateToAnalytics: (options: { replace?: boolean } = {}) =>
      navigate("/analytics", options.replace),
    dispose: () => globalThis.removeEventListener("popstate", publishLocation),
  };
}

export type AppRouter = ReturnType<typeof createRouter>;
