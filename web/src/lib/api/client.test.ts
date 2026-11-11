import { Effect, Stream } from "effect";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";

import {
  decodeRoborevNdjson,
  executeRoborevRequest,
  roborevJobOutputStream,
} from "./client";
import { retryIdempotentRead } from "./retry-policy";
import { roborevFetch } from "./generated-fetch";
import { getListJobsUrl } from "./generated/jobs/jobs";
import { getGetWebAnalyticsUrl } from "./generated/web-ui/web-ui";
import { StreamingFetch } from "../browser/streaming-fetch";
import { RoborevLogLinePayload } from "./schemas";

afterEach(() => {
  document.head.querySelector('meta[name="roborev-base-path"]')?.remove();
});

describe("native Roborev client", () => {
  test.each(["fetch", "body", "persistent"])(
    "handles a %s network failure",
    async (kind) => {
      vi.useFakeTimers();
      try {
        const fetchMock = vi.fn();
        if (kind === "fetch")
          fetchMock
            .mockRejectedValueOnce(new TypeError("offline"))
            .mockResolvedValue(Response.json({ jobs: [] }));
        else if (kind === "body") {
          const broken = new Response(
            new ReadableStream({
              start(controller) {
                controller.error(new TypeError("connection lost"));
              },
            }),
            { headers: { "Content-Type": "application/json" } },
          );
          fetchMock
            .mockResolvedValueOnce(broken)
            .mockResolvedValue(Response.json({ jobs: [] }));
        } else fetchMock.mockRejectedValue(new TypeError("offline"));
        const result = Effect.runPromise(
          executeRoborevRequest("jobs", (signal) =>
            roborevFetch("/api/jobs", {
              signal,
              roborevTransport: { baseUrl: "/", fetch: fetchMock },
            }),
          ).pipe(retryIdempotentRead, Effect.exit),
        );
        await vi.runAllTimersAsync();
        const exit = await result;
        if (kind === "persistent") expect(exit._tag).toBe("Failure");
        else
          expect(exit).toMatchObject({ _tag: "Success", value: { jobs: [] } });
        expect(fetchMock).toHaveBeenCalledTimes(kind === "persistent" ? 3 : 2);
      } finally {
        vi.useRealTimers();
      }
    },
  );

  test.each([
    new TypeError("bad access"),
    new DOMException("aborted", "AbortError"),
    new Error("invalid request"),
    "unexpected",
  ])("does not retry a non-transport failure", async (failure) => {
    const request = vi.fn().mockRejectedValue(failure);
    const result = await Effect.runPromise(
      executeRoborevRequest("jobs", request).pipe(
        retryIdempotentRead,
        Effect.exit,
      ),
    );
    expect(result._tag).toBe("Failure");
    expect(request).toHaveBeenCalledOnce();
  });

  test.each(["malformed JSON", "HTTP 503"])(
    "does not retry %s from fetch",
    async (kind) => {
      const response =
        kind === "HTTP 503"
          ? new Response(null, { status: 503 })
          : new Response("{", {
              headers: { "Content-Type": "application/json" },
            });
      const fetchMock = vi.fn().mockResolvedValue(response);
      const result = await Effect.runPromise(
        executeRoborevRequest("jobs", (signal) =>
          roborevFetch("/api/jobs", {
            signal,
            roborevTransport: { baseUrl: "/", fetch: fetchMock },
          }),
        ).pipe(retryIdempotentRead, Effect.exit),
      );
      expect(result._tag).toBe("Failure");
      expect(fetchMock).toHaveBeenCalledOnce();
    },
  );

  beforeEach(() => {
    sessionStorage.clear();
  });

  test("uses direct daemon paths through the authenticated transport", async () => {
    sessionStorage.setItem("roborev.web.session", "tab");
    sessionStorage.setItem("roborev.web.csrf", "csrf");
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      void input;
      return new Response(JSON.stringify({ version: "dev" }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    });
    await roborevFetch("/api/status", {
      roborevTransport: { baseUrl: "/", fetch: fetchMock },
    });

    const calls = fetchMock.mock.calls as unknown as Array<[RequestInfo | URL]>;
    const request = calls[0]![0] as unknown as Request;
    expect(new URL(request.url).pathname).toBe("/api/status");
    expect(request.headers.get("X-Roborev-Web-Session")).toBe("tab");
  });

  test("prefixes API requests exactly once", async () => {
    const meta = document.createElement("meta");
    meta.name = "roborev-base-path";
    meta.content = "/roborev-ci";
    document.head.append(meta);
    const fetchMock = vi.fn(
      async () =>
        new Response(JSON.stringify({ version: "dev" }), { status: 200 }),
    );
    await roborevFetch("/api/status", {
      roborevTransport: { baseUrl: "/roborev-ci/", fetch: fetchMock },
    });

    const calls = fetchMock.mock.calls as unknown as Array<[RequestInfo | URL]>;
    const request = calls[0]![0] as unknown as Request;
    expect(new URL(request.url).pathname).toBe("/roborev-ci/api/status");
  });

  test("encodes repeatable query filters as repeated keys", () => {
    const analytics = new URL(
      getGetWebAnalyticsUrl({
        project: ["project-a", "project-b"],
        source: ["codex", "claude"],
      }),
      "https://example.com",
    );
    expect(analytics.searchParams.getAll("project")).toEqual([
      "project-a",
      "project-b",
    ]);
    expect(analytics.searchParams.getAll("source")).toEqual([
      "codex",
      "claude",
    ]);

    const emptyAnalytics = new URL(
      getGetWebAnalyticsUrl({ project: [], source: [] }),
      "https://example.com",
    );
    expect(Array.from(emptyAnalytics.searchParams)).toEqual([]);

    const jobs = new URL(
      getListJobsUrl({ repo: ["/repo/a", "/repo/b"] }),
      "https://example.com",
    );
    expect(jobs.searchParams.getAll("repo")).toEqual(["/repo/a", "/repo/b"]);
  });

  test("decodes complete and trailing NDJSON records", async () => {
    const response = new Response(
      '{"ts":"one","text":"first"}\n{"ts":"two","text":"second"}',
    );

    const records = await Effect.runPromise(
      decodeRoborevNdjson(
        response,
        RoborevLogLinePayload,
        "test stream",
        "fail",
      ).pipe(Stream.runCollect),
    );

    expect(Array.from(records).map((record) => record.text)).toEqual([
      "first",
      "second",
    ]);
  });

  test("skips malformed output records when the stream policy allows it", async () => {
    const response = new Response(
      '{"text":"first"}\nnot-json\n{"text":"second"}\n',
    );

    const records = await Effect.runPromise(
      decodeRoborevNdjson(
        response,
        RoborevLogLinePayload,
        "test stream",
        "skip",
      ).pipe(Stream.runCollect),
    );

    expect(Array.from(records).map((record) => record.text)).toEqual([
      "first",
      "second",
    ]);
  });
});

describe("job output stream", () => {
  test("emits line records and requires a completion marker", async () => {
    const stream = roborevJobOutputStream("http://roborev.test", 7).pipe(
      Stream.provideService(StreamingFetch, {
        fetch: async () =>
          new Response(
            '{"type":"line","text":"one"}\n{"type":"complete","status":"done"}\n',
            { status: 200 },
          ),
      }),
      Stream.runCollect,
    );

    const records = await Effect.runPromise(stream);
    expect(Array.from(records).map((record) => record.text)).toEqual(["one"]);
  });

  test("fails retryably when output ends before its completion marker", async () => {
    const stream = roborevJobOutputStream("http://roborev.test", 7).pipe(
      Stream.provideService(StreamingFetch, {
        fetch: async () =>
          new Response('{"type":"line","text":"partial"}\n', {
            status: 200,
          }),
      }),
      Stream.runCollect,
      Effect.flip,
    );

    const failure = await Effect.runPromise(stream);
    expect(failure.retryable).toBe(true);
  });

  test("classifies transient HTTP failures as retryable", async () => {
    const stream = roborevJobOutputStream("http://roborev.test", 7).pipe(
      Stream.provideService(StreamingFetch, {
        fetch: async () => new Response(null, { status: 503 }),
      }),
      Stream.runCollect,
      Effect.flip,
    );

    const failure = await Effect.runPromise(stream);
    expect(failure.retryable).toBe(true);
  });
});
