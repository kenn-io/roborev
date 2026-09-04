import { Effect, Option, Schema, Stream } from "effect";
import { appPath, stripBasePath } from "../base-path";
import { addComment } from "./generated/comments/comments";
import { getStatus, listReleases } from "./generated/daemon/daemon";
import { cancelJob, listJobs, rerunJob } from "./generated/jobs/jobs";
import type {
  AddCommentRequest,
  AnalyticsSnapshot,
  CancelJobOutputBody,
  CancelJobRequest,
  CloseReviewOutputBody,
  CloseReviewRequest,
  DaemonStatus,
  GetReviewParams,
  GetReviewProjectionParams,
  GetWebAnalyticsParams,
  ListBranchesParams,
  ListBranchesOutputBody,
  ListJobsOutputBody,
  ListJobsParams,
  ListReposOutputBody,
  ListReposParams,
  ReleaseNotesResponse,
  RerunJobOutputBody,
  RerunJobRequest,
  Response as ReviewResponse,
  Review,
  ReviewProjection,
} from "./generated/models";
import { listBranches, listRepos } from "./generated/repos/repos";
import { closeReview, getReview } from "./generated/reviews/reviews";
import {
  getReviewProjection,
  getWebAnalytics,
} from "./generated/web-ui/web-ui";
import type {
  RoborevRequestOptions,
  RoborevTransport,
} from "./generated-fetch";
import type { Fetch } from "./session";
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

interface SignalOptions {
  readonly signal?: AbortSignal;
}

interface QueryOptions<Query> extends SignalOptions {
  readonly params: { readonly query: Query };
}

interface OptionalQueryOptions<Query> extends SignalOptions {
  readonly params?: { readonly query: Query };
}

interface BodyOptions<Body> extends SignalOptions {
  readonly body: Body;
}

export type RoborevClientResult<Value> =
  | { readonly data: Value; readonly error?: never; readonly response?: never }
  | {
      readonly data?: never;
      readonly error: RoborevAPIError;
      readonly response: globalThis.Response;
    };

type GetArguments =
  | [path: "/api/status", options?: SignalOptions]
  | [path: "/api/releases", options?: SignalOptions]
  | [path: "/api/repos", options?: OptionalQueryOptions<ListReposParams>]
  | [path: "/api/branches", options: QueryOptions<ListBranchesParams>]
  | [path: "/api/review", options: QueryOptions<GetReviewParams>]
  | [
      path: "/api/ui/review-projection",
      options: QueryOptions<GetReviewProjectionParams>,
    ]
  | [path: "/api/jobs", options?: OptionalQueryOptions<ListJobsParams>]
  | [path: "/api/ui/analytics", options: QueryOptions<GetWebAnalyticsParams>];

type PostArguments =
  | [path: "/api/review/close", options: BodyOptions<CloseReviewRequest>]
  | [path: "/api/comment", options: BodyOptions<AddCommentRequest>]
  | [path: "/api/job/cancel", options: BodyOptions<CancelJobRequest>]
  | [path: "/api/job/rerun", options: BodyOptions<RerunJobRequest>];

export class RoborevAPIError extends Error {
  constructor(
    message: string,
    readonly detail?: string,
  ) {
    super(message);
    this.name = "RoborevAPIError";
  }
}

export interface RoborevClient {
  GET(
    path: "/api/status",
    options?: SignalOptions,
  ): Promise<RoborevClientResult<DaemonStatus>>;
  GET(
    path: "/api/releases",
    options?: SignalOptions,
  ): Promise<RoborevClientResult<ReleaseNotesResponse>>;
  GET(
    path: "/api/repos",
    options?: OptionalQueryOptions<ListReposParams>,
  ): Promise<RoborevClientResult<ListReposOutputBody>>;
  GET(
    path: "/api/branches",
    options: QueryOptions<ListBranchesParams>,
  ): Promise<RoborevClientResult<ListBranchesOutputBody>>;
  GET(
    path: "/api/review",
    options: QueryOptions<GetReviewParams>,
  ): Promise<RoborevClientResult<Review>>;
  GET(
    path: "/api/ui/review-projection",
    options: QueryOptions<GetReviewProjectionParams>,
  ): Promise<RoborevClientResult<ReviewProjection>>;
  GET(
    path: "/api/jobs",
    options?: OptionalQueryOptions<ListJobsParams>,
  ): Promise<RoborevClientResult<ListJobsOutputBody>>;
  GET(
    path: "/api/ui/analytics",
    options: QueryOptions<GetWebAnalyticsParams>,
  ): Promise<RoborevClientResult<AnalyticsSnapshot>>;
  POST(
    path: "/api/review/close",
    options: BodyOptions<CloseReviewRequest>,
  ): Promise<RoborevClientResult<CloseReviewOutputBody>>;
  POST(
    path: "/api/comment",
    options: BodyOptions<AddCommentRequest>,
  ): Promise<RoborevClientResult<ReviewResponse>>;
  POST(
    path: "/api/job/cancel",
    options: BodyOptions<CancelJobRequest>,
  ): Promise<RoborevClientResult<CancelJobOutputBody>>;
  POST(
    path: "/api/job/rerun",
    options: BodyOptions<RerunJobRequest>,
  ): Promise<RoborevClientResult<RerunJobOutputBody>>;
}

function requestOptions(
  transport: RoborevTransport,
  options?: SignalOptions,
): RoborevRequestOptions {
  return { ...options, roborevTransport: transport };
}

async function clientResult<Value>(
  request: Promise<Value>,
): Promise<RoborevClientResult<Value>> {
  try {
    return { data: await request };
  } catch (cause) {
    if (!(cause instanceof globalThis.Response)) throw cause;
    const detail = await responseDetail(cause);
    return {
      error: new RoborevAPIError(detail ?? `API ${cause.status}`, detail),
      response: cause,
    };
  }
}

async function responseDetail(
  response: globalThis.Response,
): Promise<string | undefined> {
  let body: unknown;
  try {
    body = await response.clone().json();
  } catch {
    return undefined;
  }
  if (
    typeof body === "object" &&
    body !== null &&
    "detail" in body &&
    typeof body.detail === "string"
  ) {
    return body.detail;
  }
  return undefined;
}

class GeneratedRoborevClient implements RoborevClient {
  constructor(private readonly transport: RoborevTransport) {}

  GET(
    path: "/api/status",
    options?: SignalOptions,
  ): Promise<RoborevClientResult<DaemonStatus>>;
  GET(
    path: "/api/releases",
    options?: SignalOptions,
  ): Promise<RoborevClientResult<ReleaseNotesResponse>>;
  GET(
    path: "/api/repos",
    options?: OptionalQueryOptions<ListReposParams>,
  ): Promise<RoborevClientResult<ListReposOutputBody>>;
  GET(
    path: "/api/branches",
    options: QueryOptions<ListBranchesParams>,
  ): Promise<RoborevClientResult<ListBranchesOutputBody>>;
  GET(
    path: "/api/review",
    options: QueryOptions<GetReviewParams>,
  ): Promise<RoborevClientResult<Review>>;
  GET(
    path: "/api/ui/review-projection",
    options: QueryOptions<GetReviewProjectionParams>,
  ): Promise<RoborevClientResult<ReviewProjection>>;
  GET(
    path: "/api/jobs",
    options?: OptionalQueryOptions<ListJobsParams>,
  ): Promise<RoborevClientResult<ListJobsOutputBody>>;
  GET(
    path: "/api/ui/analytics",
    options: QueryOptions<GetWebAnalyticsParams>,
  ): Promise<RoborevClientResult<AnalyticsSnapshot>>;
  GET(...args: GetArguments): Promise<RoborevClientResult<unknown>> {
    const [path, options] = args;
    switch (path) {
      case "/api/status":
        return clientResult(getStatus(requestOptions(this.transport, options)));
      case "/api/releases":
        return clientResult(
          listReleases(requestOptions(this.transport, options)),
        );
      case "/api/repos":
        return clientResult(
          listRepos(
            options?.params?.query,
            requestOptions(this.transport, options),
          ),
        );
      case "/api/branches":
        return clientResult(
          listBranches(
            options.params.query,
            requestOptions(this.transport, options),
          ),
        );
      case "/api/review":
        return clientResult(
          getReview(
            options.params.query,
            requestOptions(this.transport, options),
          ),
        );
      case "/api/ui/review-projection":
        return clientResult(
          getReviewProjection(
            options.params.query,
            requestOptions(this.transport, options),
          ),
        );
      case "/api/jobs":
        return clientResult(
          listJobs(
            options?.params?.query,
            requestOptions(this.transport, options),
          ),
        );
      case "/api/ui/analytics":
        return clientResult(
          getWebAnalytics(
            options.params.query,
            requestOptions(this.transport, options),
          ),
        );
    }
  }

  POST(
    path: "/api/review/close",
    options: BodyOptions<CloseReviewRequest>,
  ): Promise<RoborevClientResult<CloseReviewOutputBody>>;
  POST(
    path: "/api/comment",
    options: BodyOptions<AddCommentRequest>,
  ): Promise<RoborevClientResult<ReviewResponse>>;
  POST(
    path: "/api/job/cancel",
    options: BodyOptions<CancelJobRequest>,
  ): Promise<RoborevClientResult<CancelJobOutputBody>>;
  POST(
    path: "/api/job/rerun",
    options: BodyOptions<RerunJobRequest>,
  ): Promise<RoborevClientResult<RerunJobOutputBody>>;
  POST(...args: PostArguments): Promise<RoborevClientResult<unknown>> {
    const [path, options] = args;
    switch (path) {
      case "/api/review/close":
        return clientResult(
          closeReview(options.body, requestOptions(this.transport, options)),
        );
      case "/api/comment":
        return clientResult(
          addComment(options.body, requestOptions(this.transport, options)),
        );
      case "/api/job/cancel":
        return clientResult(
          cancelJob(options.body, requestOptions(this.transport, options)),
        );
      case "/api/job/rerun":
        return clientResult(
          rerunJob(options.body, requestOptions(this.transport, options)),
        );
    }
  }
}

export function createRoborevClient(
  baseUrl: string,
  fetchFn?: Fetch,
): RoborevClient {
  return new GeneratedRoborevClient({
    baseUrl,
    fetch: fetchFn ?? globalThis.fetch.bind(globalThis),
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
