import { Cause, Context, Effect, Layer, Queue, Stream } from "effect";
import type { Scope } from "effect/Scope";

import {
  BrowserStreamError,
  InvalidExternalPayload,
  TransientTransportError,
} from "../api/effect-errors";
import { authenticatedFetch, type Fetch } from "../api/session";

export interface ByteReader {
  readonly closed: Promise<void>;
  cancel(reason?: unknown): Promise<void>;
  read(): Promise<ReadableStreamReadResult<Uint8Array>>;
  releaseLock(): void;
}

export class StreamingFetch extends Context.Service<
  StreamingFetch,
  { readonly fetch: Fetch }
>()("roborev/browser/StreamingFetch") {}

const liveFetch: Fetch = (input, init) => globalThis.fetch(input, init);

export const StreamingFetchLive = Layer.succeed(StreamingFetch)({
  fetch: authenticatedFetch(liveFetch),
});

export const openStreamingResponse = Effect.fn("StreamingFetch.open")(
  function* (operation: string, input: RequestInfo | URL, init?: RequestInit) {
    const transport = yield* StreamingFetch;
    const controller = yield* Effect.acquireRelease(
      Effect.sync(() => new AbortController()),
      (owned) => Effect.sync(() => owned.abort()),
    );
    return yield* Effect.tryPromise({
      try: () => transport.fetch(input, { ...init, signal: controller.signal }),
      catch: (cause) => TransientTransportError.make({ operation, cause }),
    });
  },
);

const releaseReader = (reader: ByteReader) =>
  Effect.tryPromise({
    try: () => reader.cancel(),
    catch: () => undefined,
  }).pipe(
    Effect.catchCause(() => Effect.void),
    Effect.ensuring(Effect.sync(() => reader.releaseLock())),
  );

export const byteStreamFromReader = <E, R>(
  acquire: Effect.Effect<ByteReader, E, R>,
  operation: string,
): Stream.Stream<Uint8Array, E | BrowserStreamError, Exclude<R, Scope>> =>
  Stream.callback<Uint8Array, E | BrowserStreamError, R>(
    (queue) =>
      Effect.gen(function* () {
        const reader = yield* Effect.acquireRelease(acquire, releaseReader);
        const read = (): Effect.Effect<void, BrowserStreamError> =>
          Effect.suspend(() =>
            Effect.tryPromise({
              try: () => reader.read(),
              catch: (cause) => BrowserStreamError.make({ operation, cause }),
            }).pipe(
              Effect.flatMap((result) => {
                if (result.done) {
                  Queue.endUnsafe(queue);
                  return Effect.void;
                }
                return Queue.offer(queue, result.value).pipe(
                  Effect.andThen(read()),
                );
              }),
            ),
          );
        yield* read().pipe(
          Effect.catch((failure) =>
            Effect.sync(() => {
              Queue.failCauseUnsafe(queue, Cause.fail(failure));
            }),
          ),
          Effect.forkScoped,
        );
      }),
    { bufferSize: 16, strategy: "suspend" },
  );

export const responseByteStream = (response: Response, operation: string) => {
  const acquire = Effect.try({
    try: (): ByteReader => {
      if (response.body === null) {
        throw new Error("response body is not readable");
      }
      return response.body.getReader();
    },
    catch: (cause) => InvalidExternalPayload.make({ operation, cause }),
  });
  return byteStreamFromReader(acquire, operation);
};
