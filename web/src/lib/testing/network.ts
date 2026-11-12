import { roborevFetch } from "../api/generated-fetch";

/** A read that loses its connection at roborevFetch's transport boundary. */
export function offlineRead<T>(): Promise<T> {
  return roborevFetch<T>("/api/test", {
    roborevTransport: {
      baseUrl: "/",
      fetch: () => Promise.reject(new TypeError("offline")),
    },
  });
}
