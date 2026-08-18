import createClient from "openapi-fetch";
import { Effect, Option, Schema, Stream } from "effect";
import { appPath, stripBasePath } from "../base-path";
import type { paths } from "./generated";
import { authenticatedFetch, type Fetch } from "./session";
import { TransientTransportError } from "./effect-errors";
import {
  openStreamingResponse,
  responseByteStream,
} from "../browser/streaming-fetch";
import {
  RoborevEvent,
  RoborevJobOutputRecord,
  RoborevJobOutputSnapshot,
  RoborevLogLinePayload,
  RoborevStreamOpened,
} from "./schemas";

export type RoborevClient = ReturnType<typeof createClient<paths>>;

export function createRoborevClient(
  baseUrl: string,
  fetchFn?: Fetch,
): RoborevClient {
  const inner = fetchFn ?? globalThis.fetch.bind(globalThis);
  const resolvedBaseUrl = resolveBaseUrl(baseUrl);
  return createClient<paths>({
    baseUrl: new URL(resolvedBaseUrl, globalThis.location.origin).toString(),
    fetch: authenticatedFetch(inner),
  });
}

export const executeRoborevRequest = Effect.fn("RoborevClient.execute")(
  function* <A>(
    operation: string,
    request: (signal: AbortSignal) => Promise<A>,
  ) {
    return yield* Effect.tryPromise({
      try: request,
      catch: (cause) => TransientTransportError.make({ operation, cause }),
    });
  },
);

export class RoborevStreamError extends Schema.TaggedErrorClass<RoborevStreamError>()(
  "RoborevStreamError",
  {
    operation: Schema.String,
    retryable: Schema.Boolean,
    status: Schema.optionalKey(Schema.String),
    cause: Schema.Defect(),
  },
) {}

class NdjsonParser {
  private buffer = "";

  push(chunk: string): Array<string> {
    this.buffer += chunk;
    const lines: string[] = [];
    for (;;) {
      const newline = this.buffer.indexOf("\n");
      if (newline === -1) break;
      const line = this.buffer.slice(0, newline).trim();
      this.buffer = this.buffer.slice(newline + 1);
      if (line !== "") lines.push(line);
    }
    return lines;
  }

  flush(): Array<string> {
    const line = this.buffer.trim();
    this.buffer = "";
    return line === "" ? [] : [line];
  }
}

type InvalidNdjsonPolicy = "fail" | "skip";

export function decodeRoborevNdjson<
  S extends Schema.ConstraintDecoder<unknown>,
>(
  response: Response,
  schema: S,
  operation: string,
  invalidRecordPolicy: InvalidNdjsonPolicy,
): Stream.Stream<S["Type"], RoborevStreamError> {
  return Stream.suspend(() => {
    const parser = new NdjsonParser();
    const decodeLine = (
      line: string,
    ): Stream.Stream<S["Type"], RoborevStreamError> => {
      let input: unknown;
      try {
        input = JSON.parse(line);
      } catch (cause) {
        return invalidRecordPolicy === "skip"
          ? Stream.empty
          : Stream.fail(
              RoborevStreamError.make({ operation, retryable: true, cause }),
            );
      }
      const decoded = Schema.decodeUnknownOption(schema)(input);
      if (Option.isSome(decoded)) return Stream.succeed(decoded.value);
      return invalidRecordPolicy === "skip"
        ? Stream.empty
        : Stream.fail(
            RoborevStreamError.make({
              operation,
              retryable: true,
              cause: new Error(
                "Roborev NDJSON stream returned an invalid record",
              ),
            }),
          );
    };
    const decodeLines = (lines: ReadonlyArray<string>) =>
      Stream.fromIterable(lines).pipe(Stream.flatMap(decodeLine));
    const values = responseByteStream(response, operation).pipe(
      Stream.mapError((cause) =>
        RoborevStreamError.make({ operation, retryable: true, cause }),
      ),
      Stream.decodeText(),
      Stream.flatMap((chunk) => decodeLines(parser.push(chunk))),
    );
    return Stream.concat(
      values,
      Stream.suspend(() => decodeLines(parser.flush())),
    );
  });
}

function releaseResponseBody(response: Response): Effect.Effect<void> {
  const body = response.body;
  if (body === null) return Effect.void;
  return Effect.tryPromise({
    try: () => body.cancel(),
    catch: () => undefined,
  }).pipe(Effect.ignore);
}

export function roborevEventStream(
  baseUrl: string,
): Stream.Stream<
  RoborevEvent | RoborevStreamOpened,
  RoborevStreamError,
  import("../browser/streaming-fetch").StreamingFetch
> {
  const url = apiURL(baseUrl, "/api/stream/events");
  return Stream.unwrap(
    Effect.gen(function* () {
      const response = yield* Effect.acquireRelease(
        openStreamingResponse("open Roborev event stream", url, {
          headers: { Accept: "application/x-ndjson" },
        }).pipe(
          Effect.mapError((cause) =>
            RoborevStreamError.make({
              operation: "open Roborev event stream",
              retryable: true,
              cause,
            }),
          ),
        ),
        releaseResponseBody,
        { interruptible: true },
      );
      if (!response.ok) {
        return Stream.fail(
          RoborevStreamError.make({
            operation: "open Roborev event stream",
            retryable:
              response.status === 408 ||
              response.status === 429 ||
              response.status >= 500,
            cause: new Error(
              `Roborev event stream returned ${response.status}`,
            ),
          }),
        );
      }
      if (response.body === null) {
        return Stream.fail(
          RoborevStreamError.make({
            operation: "open Roborev event stream",
            retryable: false,
            cause: new Error("Roborev event stream returned no response body"),
          }),
        );
      }
      return Stream.concat(
        Stream.succeed<RoborevEvent | RoborevStreamOpened>(
          RoborevStreamOpened.make({ opened: true }),
        ),
        Stream.concat(
          decodeRoborevNdjson(
            response,
            RoborevEvent,
            "read Roborev event stream",
            "fail",
          ),
          Stream.fail(
            RoborevStreamError.make({
              operation: "read Roborev event stream",
              retryable: true,
              cause: new Error("Roborev event stream disconnected"),
            }),
          ),
        ),
      );
    }),
  );
}

function jobOutputUrl(
  baseUrl: string,
  jobID: number,
  streaming: boolean,
): string {
  const url = new URL(apiURL(baseUrl, "/api/job/output"));
  url.searchParams.set("job_id", String(jobID));
  if (streaming) url.searchParams.set("stream", "1");
  return url.toString();
}

function apiURL(baseUrl: string, path: string): string {
  const resolvedBaseUrl = resolveBaseUrl(baseUrl);
  return new URL(
    `${resolvedBaseUrl.replace(/\/$/, "")}${path}`,
    globalThis.location.origin,
  ).toString();
}

function resolveBaseUrl(baseUrl: string): string {
  if (!baseUrl.startsWith("/")) return baseUrl;
  return stripBasePath(baseUrl) === "" ? appPath(baseUrl) : baseUrl;
}

export const loadRoborevJobOutput = Effect.fn("RoborevClient.loadJobOutput")(
  function* (baseUrl: string, jobID: number) {
    return yield* Effect.scoped(
      Effect.gen(function* () {
        const response = yield* Effect.acquireRelease(
          openStreamingResponse(
            "load Roborev job output",
            jobOutputUrl(baseUrl, jobID, false),
          ).pipe(
            Effect.mapError((cause) =>
              RoborevStreamError.make({
                operation: "load Roborev job output",
                retryable: true,
                cause,
              }),
            ),
          ),
          releaseResponseBody,
          { interruptible: true },
        );
        if (!response.ok) {
          return yield* Effect.fail(
            RoborevStreamError.make({
              operation: "load Roborev job output",
              retryable:
                response.status === 408 ||
                response.status === 429 ||
                response.status >= 500,
              cause: new Error(
                `Roborev job output returned ${response.status}`,
              ),
            }),
          );
        }
        const input = yield* Effect.tryPromise({
          try: () => response.json(),
          catch: (cause) =>
            RoborevStreamError.make({
              operation: "decode Roborev job output",
              retryable: false,
              cause,
            }),
        });
        return yield* Schema.decodeUnknownEffect(RoborevJobOutputSnapshot)(
          input,
        ).pipe(
          Effect.mapError((cause) =>
            RoborevStreamError.make({
              operation: "decode Roborev job output",
              retryable: false,
              cause,
            }),
          ),
        );
      }),
    );
  },
);

export function roborevJobOutputStream(
  baseUrl: string,
  jobID: number,
): Stream.Stream<
  RoborevLogLinePayload,
  RoborevStreamError,
  import("../browser/streaming-fetch").StreamingFetch
> {
  return Stream.unwrap(
    Effect.gen(function* () {
      const response = yield* Effect.acquireRelease(
        openStreamingResponse(
          "stream Roborev job output",
          jobOutputUrl(baseUrl, jobID, true),
        ).pipe(
          Effect.mapError((cause) =>
            RoborevStreamError.make({
              operation: "stream Roborev job output",
              retryable: true,
              cause,
            }),
          ),
        ),
        releaseResponseBody,
        { interruptible: true },
      );
      if (!response.ok || response.body === null) {
        return Stream.fail(
          RoborevStreamError.make({
            operation: "stream Roborev job output",
            retryable:
              response.status === 408 ||
              response.status === 429 ||
              response.status >= 500,
            cause: new Error(
              response.body === null
                ? "Roborev job output returned no response body"
                : `Roborev job output returned ${response.status}`,
            ),
          }),
        );
      }
      return Stream.suspend(() => {
        let completed = false;
        const records = decodeRoborevNdjson(
          response,
          RoborevJobOutputRecord,
          "read Roborev job output",
          "skip",
        ).pipe(
          Stream.flatMap((record) => {
            if (record.type === "complete") {
              if (record.status === "queued") {
                return Stream.fail(
                  RoborevStreamError.make({
                    operation: "stream Roborev job output",
                    retryable: true,
                    status: record.status,
                    cause: new Error("Roborev job was requeued"),
                  }),
                );
              }
              completed = true;
              return Stream.empty;
            }
            return Stream.succeed(
              RoborevLogLinePayload.make({
                ...(record.ts === undefined ? {} : { ts: record.ts }),
                ...(record.text === undefined ? {} : { text: record.text }),
                ...(record.line_type === undefined
                  ? {}
                  : { line_type: record.line_type }),
              }),
            );
          }),
        );
        return Stream.concat(
          records,
          Stream.suspend(() =>
            completed
              ? Stream.empty
              : Stream.fail(
                  RoborevStreamError.make({
                    operation: "read Roborev job output",
                    retryable: true,
                    cause: new Error(
                      "Roborev job output disconnected before completion",
                    ),
                  }),
                ),
          ),
        );
      });
    }),
  );
}
