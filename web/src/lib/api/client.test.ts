import { Effect, Stream } from "effect";
import { beforeEach, describe, expect, test, vi } from "vitest";

import {
  createRoborevClient,
  decodeRoborevNdjson,
  roborevJobOutputStream,
} from "./client";
import { StreamingFetch } from "../browser/streaming-fetch";
import { RoborevLogLinePayload } from "./schemas";

describe("native Roborev client", () => {
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
    const client = createRoborevClient("/", fetchMock);

    await client.GET("/api/status");

    const request = fetchMock.mock.calls[0]![0] as unknown as Request;
    expect(new URL(request.url).pathname).toBe("/api/status");
    expect(request.headers.get("X-Roborev-Web-Session")).toBe("tab");
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
