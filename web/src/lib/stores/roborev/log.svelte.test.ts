import { Effect } from "effect";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { AppRuntime, OwnedAppRuntime } from "../../runtime/runtime";
import { makeTestAppRuntime } from "../../testing/runtime";
import {
  createLogStore as createRuntimeLogStore,
  type LogStore,
} from "./log.svelte";

const encoder = new TextEncoder();
let runtime: OwnedAppRuntime | undefined;

function createLogStore(options: { readonly baseUrl: string }): LogStore {
  if (runtime === undefined)
    throw new Error("test runtime was not initialized");
  return createRuntimeLogStore({
    ...options,
    runtime,
  });
}

async function runLogEffect(
  program: ReturnType<LogStore["loadSnapshotEffect"]>,
): Promise<void> {
  if (runtime === undefined)
    throw new Error("test runtime was not initialized");
  const execution = runtime.runCommand(program, {
    operation: "test Roborev log",
    safeContext: {},
    onFailure: () => {},
  });
  const exit = await Effect.runPromise(execution.await);
  expect(exit._tag).toBe("Success");
}

function requestURL(input: RequestInfo | URL): string {
  return input instanceof Request ? input.url : String(input);
}

function requestSignal(
  input: RequestInfo | URL,
  init?: RequestInit,
): AbortSignal | undefined {
  return input instanceof Request
    ? (input.signal ?? undefined)
    : (init?.signal ?? undefined);
}

function ndjsonResponse(lines: unknown[], status = "done"): Response {
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      for (const line of lines) {
        controller.enqueue(encoder.encode(`${JSON.stringify(line)}\n`));
      }
      controller.enqueue(
        encoder.encode(`${JSON.stringify({ type: "complete", status })}\n`),
      );
      controller.close();
    },
  });
  return new Response(body, {
    status: 200,
    headers: { "Content-Type": "application/x-ndjson" },
  });
}

describe("createLogStore", () => {
  beforeEach(() => {
    runtime = makeTestAppRuntime();
  });

  afterEach(async () => {
    vi.restoreAllMocks();
    if (runtime !== undefined) await Effect.runPromise(runtime.disposeEffect);
    runtime = undefined;
  });

  it("loads completed job output snapshots from the JSON response", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(
        JSON.stringify({
          lines: [
            {
              ts: "2026-04-11T11:00:00Z",
              text: "finished review",
              line_type: "text",
            },
          ],
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
    const store = createLogStore({ baseUrl: "http://roborev.test" });

    await runLogEffect(store.loadSnapshotEffect(42, "snapshot-test"));

    expect(requestURL(fetchMock.mock.calls[0]![0])).toBe(
      "http://roborev.test/api/job/output?job_id=42",
    );
    expect(store.getLines()).toEqual([
      {
        ts: "2026-04-11T11:00:00Z",
        text: "finished review",
        lineType: "text",
      },
    ]);
  });

  it("streams live job output from the NDJSON job output endpoint", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      ndjsonResponse([
        {
          type: "line",
          ts: "2026-04-11T11:00:01Z",
          text: "running review",
          line_type: "tool",
        },
      ]),
    );
    const store = createLogStore({ baseUrl: "http://roborev.test" });

    await runLogEffect(store.startStreamingEffect(77, "stream-test"));

    expect(requestURL(fetchMock.mock.calls[0]![0])).toBe(
      "http://roborev.test/api/job/output?job_id=77&stream=1",
    );
    expect(
      requestSignal(fetchMock.mock.calls[0]![0], fetchMock.mock.calls[0]![1]),
    ).toBeInstanceOf(AbortSignal);
    expect(store.getLines()).toEqual([
      {
        ts: "2026-04-11T11:00:01Z",
        text: "running review",
        lineType: "tool",
      },
    ]);
  });

  it("reconciles the authoritative snapshot after a completed stream", async () => {
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(
        ndjsonResponse([{ type: "line", text: "first", line_type: "text" }]),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            lines: [
              { text: "first", line_type: "text" },
              { text: "record dropped by stream", line_type: "text" },
            ],
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      );
    const store = createLogStore({ baseUrl: "http://roborev.test" });

    await runLogEffect(store.startStreamingEffect(81, "complete-reconcile"));

    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(requestURL(fetchMock.mock.calls[1]![0])).toBe(
      "http://roborev.test/api/job/output?job_id=81",
    );
    expect(store.getLines().map((line) => line.text)).toEqual([
      "first",
      "record dropped by stream",
    ]);
  });

  it("reconnects a live output stream after a transient transport failure", async () => {
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockRejectedValueOnce(new TypeError("connection reset"))
      .mockResolvedValueOnce(
        ndjsonResponse([
          {
            type: "line",
            ts: "2026-04-11T11:00:03Z",
            text: "reconnected output",
            line_type: "text",
          },
        ]),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            lines: [
              {
                ts: "2026-04-11T11:00:03Z",
                text: "reconnected output",
                line_type: "text",
              },
            ],
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      );
    const store = createLogStore({ baseUrl: "http://roborev.test" });

    await runLogEffect(store.startStreamingEffect(79, "reconnect-test"));

    expect(fetchMock).toHaveBeenCalledTimes(3);
    expect(store.getLines().map((line) => line.text)).toEqual([
      "reconnected output",
    ]);
  });

  it("keeps existing output and reconnects when an attempt is requeued", async () => {
    let nextAttempt: ReadableStreamDefaultController<Uint8Array> | undefined;
    const nextAttemptResponse = new Response(
      new ReadableStream<Uint8Array>({
        start(controller) {
          nextAttempt = controller;
        },
      }),
      { status: 200 },
    );
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(
        ndjsonResponse(
          [{ type: "line", text: "first attempt", line_type: "text" }],
          "queued",
        ),
      )
      .mockResolvedValueOnce(nextAttemptResponse)
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            lines: [
              { text: "first attempt", line_type: "text" },
              { text: "second attempt", line_type: "text" },
            ],
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      );
    const store = createLogStore({ baseUrl: "http://roborev.test" });

    const streaming = runLogEffect(
      store.startStreamingEffect(82, "requeue-reconnect"),
    );
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2), {
      timeout: 2_000,
    });
    expect(store.getLines().map((line) => line.text)).toEqual([
      "first attempt",
    ]);

    nextAttempt?.enqueue(
      encoder.encode(
        `${JSON.stringify({ type: "line", text: "second attempt", line_type: "text" })}\n`,
      ),
    );
    nextAttempt?.enqueue(
      encoder.encode(
        `${JSON.stringify({ type: "complete", status: "done" })}\n`,
      ),
    );
    nextAttempt?.close();
    await streaming;

    expect(fetchMock).toHaveBeenCalledTimes(3);
    expect(store.getLines().map((line) => line.text)).toEqual([
      "first attempt",
      "second attempt",
    ]);
  });

  it("reconciles partial output before reconnecting without duplicates", async () => {
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(
        new Response('{"type":"line","text":"first","line_type":"text"}\n', {
          status: 200,
        }),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            lines: [
              { text: "first", line_type: "text" },
              { text: "second", line_type: "text" },
            ],
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      )
      .mockResolvedValueOnce(
        ndjsonResponse([
          { type: "line", text: "first", line_type: "text" },
          { type: "line", text: "second", line_type: "text" },
        ]),
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            lines: [
              { text: "first", line_type: "text" },
              { text: "second", line_type: "text" },
            ],
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      );
    const store = createLogStore({ baseUrl: "http://roborev.test" });

    await runLogEffect(store.startStreamingEffect(80, "partial-reconnect"));

    expect(fetchMock).toHaveBeenCalledTimes(4);
    expect(store.getLines().map((line) => line.text)).toEqual([
      "first",
      "second",
    ]);
  });

  it("skips malformed log records without failing later valid output", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(
        new ReadableStream<Uint8Array>({
          start(controller) {
            controller.enqueue(encoder.encode("not-json\n"));
            controller.enqueue(
              encoder.encode(
                `${JSON.stringify({ type: "line", ts: "2026-04-11T11:00:02Z", text: "valid output", line_type: "text" })}\n`,
              ),
            );
            controller.close();
          },
        }),
        { status: 200 },
      ),
    );
    const store = createLogStore({ baseUrl: "http://roborev.test" });

    await runLogEffect(store.startStreamingEffect(78, "malformed-log-test"));

    expect(store.getLines().map((line) => line.text)).toEqual(["valid output"]);
  });

  it("aborts a stale output snapshot when another job becomes authoritative", async () => {
    let staleSignal: AbortSignal | undefined;
    let calls = 0;
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockImplementation((_input, init) => {
        calls += 1;
        if (calls === 1) {
          staleSignal = requestSignal(_input, init);
          return new Promise<Response>((_resolve, reject) => {
            staleSignal?.addEventListener(
              "abort",
              () => reject(new DOMException("request aborted", "AbortError")),
              {
                once: true,
              },
            );
          });
        }
        return Promise.resolve(
          new Response(JSON.stringify({ lines: [] }), {
            status: 200,
            headers: { "Content-Type": "application/json" },
          }),
        );
      });
    const store = createLogStore({ baseUrl: "http://roborev.test" });

    store.loadSnapshot(41);
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    store.loadSnapshot(42);
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));

    expect(staleSignal).toBeDefined();
    expect(staleSignal?.aborted).toBe(true);
  });

  it("does not let stale log teardown stop a successor lease", async () => {
    const signals: AbortSignal[] = [];
    vi.spyOn(globalThis, "fetch").mockImplementation((input, init) => {
      const signal = requestSignal(input, init);
      if (signal !== undefined) signals.push(signal);
      return new Promise<Response>((_resolve, reject) => {
        signal?.addEventListener(
          "abort",
          () => reject(new DOMException("request aborted", "AbortError")),
          {
            once: true,
          },
        );
      });
    });
    const store = createLogStore({ baseUrl: "http://roborev.test" });

    const firstLease = store.startStreaming(41);
    await vi.waitFor(() => expect(signals).toHaveLength(1));
    const secondLease = store.startStreaming(42);
    await vi.waitFor(() => expect(signals).toHaveLength(2));
    if (runtime === undefined)
      throw new Error("test runtime was not initialized");
    const staleTeardown = runtime.runCommand(
      store.stopStreamingEffect(firstLease),
      {
        operation: "stop stale Roborev log lease",
        safeContext: {},
        onFailure: () => {},
      },
    );
    await Effect.runPromise(staleTeardown.await);

    expect(signals[0]?.aborted).toBe(true);
    expect(signals[1]?.aborted).toBe(false);
    expect(store.getConnectedJobId()).toBe(42);
    expect(store.isStreaming()).toBe(true);
    const successorTeardown = runtime.runCommand(
      store.stopStreamingEffect(secondLease),
      {
        operation: "stop current Roborev log lease",
        safeContext: {},
        onFailure: () => {},
      },
    );
    await Effect.runPromise(successorTeardown.await);
  });

  it("claims the newest public log lease before scheduled work begins", () => {
    const scheduledRuntime = {
      runCommand: () => ({
        interrupt: () => {},
        await: Effect.never,
        exit: new Promise(() => {}),
      }),
      runMicrotask: () => ({
        interrupt: () => {},
        await: Effect.never,
        exit: new Promise(() => {}),
      }),
    } satisfies AppRuntime;
    const store = createRuntimeLogStore({
      baseUrl: "http://roborev.test",
      runtime: scheduledRuntime,
    });

    store.startStreaming(41);
    store.startStreaming(42);

    expect(store.getConnectedJobId()).toBe(42);
    expect(store.isStreaming()).toBe(true);
  });

  it("does not start a delayed log lease after that lease was stopped", async () => {
    if (runtime === undefined)
      throw new Error("test runtime was not initialized");
    const liveRuntime = runtime;
    let runDelayedStart: (() => void) | undefined;
    const delayedRuntime: AppRuntime = {
      runMicrotask: liveRuntime.runMicrotask,
      runCommand(program, options) {
        if (
          options.operation === "stream Roborev job output" &&
          runDelayedStart === undefined
        ) {
          runDelayedStart = () => {
            liveRuntime.runCommand(program, options);
          };
          return {
            interrupt: () => {},
            await: Effect.never,
            exit: new Promise(() => {}),
          };
        }
        return liveRuntime.runCommand(program, options);
      },
    };
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValue(ndjsonResponse([]));
    const store = createRuntimeLogStore({
      baseUrl: "http://roborev.test",
      runtime: delayedRuntime,
    });

    const lease = store.startStreaming(41);
    store.stopStreaming(lease);
    await vi.waitFor(() => expect(store.isStreaming()).toBe(false));
    runDelayedStart?.();
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(fetchMock).not.toHaveBeenCalled();
    expect(store.getConnectedJobId()).toBeUndefined();
  });
});
