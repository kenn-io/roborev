import { assert, it } from "@effect/vitest";
import { Deferred, Effect, Fiber, Option, Ref } from "effect";
import { TestClock } from "effect/testing";
import { TransientTransportError } from "../../api/effect-errors";
import { StreamingFetch } from "../../browser/streaming-fetch";
import {
  makeRoborevWorkflow,
  RoborevMutationBlocked,
  RoborevMutationOutcomeUnknown,
  roborevMutationFailureMessage,
} from "./workflow";

it("describes whether a fenced Roborev action was sent", () => {
  assert.strictEqual(
    roborevMutationFailureMessage(
      "Failed to add comment",
      RoborevMutationBlocked.make({
        key: "review:42",
        operation: "add Roborev comment",
        resolution: "applied",
      }),
    ),
    "The previous add Roborev comment was applied. This action was not sent.",
  );
  assert.strictEqual(
    roborevMutationFailureMessage(
      "Failed to add comment",
      RoborevMutationOutcomeUnknown.make({
        key: "review:42",
        operation: "add Roborev comment",
        cause: new Error("response lost"),
        reconciliationCause: new Error("authority unavailable"),
      }),
    ),
    "Could not confirm whether the add Roborev comment was applied. Matching actions remain blocked until Roborev authority is available.",
  );
});

it.effect("runs accepted Roborev mutations once in submission order", () =>
  Effect.scoped(
    Effect.gen(function* () {
      const workflow = yield* makeRoborevWorkflow;
      const firstStarted = yield* Deferred.make<void>();
      const releaseFirst = yield* Deferred.make<void>();
      const observed = yield* Ref.make<ReadonlyArray<string>>([]);

      const first = yield* workflow
        .mutate({
          key: "job:1",
          operation: "cancel Roborev job",
          mutation: Deferred.succeed(firstStarted, undefined).pipe(
            Effect.andThen(
              Ref.update(observed, (events) => [...events, "cancel"]),
            ),
            Effect.andThen(Deferred.await(releaseFirst)),
          ),
          reconcile: () => Effect.succeed(Option.some(undefined)),
        })
        .pipe(Effect.forkChild);
      yield* Deferred.await(firstStarted);

      const second = yield* workflow
        .mutate({
          key: "job:2",
          operation: "rerun Roborev job",
          mutation: Ref.update(observed, (events) => [...events, "rerun"]),
          reconcile: () => Effect.succeed(Option.some(undefined)),
        })
        .pipe(Effect.forkChild);
      yield* Effect.yieldNow;

      assert.deepStrictEqual(yield* Ref.get(observed), ["cancel"]);
      yield* Deferred.succeed(releaseFirst, undefined);
      yield* Fiber.join(first);
      yield* Fiber.join(second);
      assert.deepStrictEqual(yield* Ref.get(observed), ["cancel", "rerun"]);
    }),
  ),
);

it.effect("does not replay a failed Roborev mutation", () =>
  Effect.scoped(
    Effect.gen(function* () {
      const workflow = yield* makeRoborevWorkflow;
      const attempts = yield* Ref.make(0);
      const exit = yield* workflow
        .mutate({
          key: "job:1",
          operation: "cancel Roborev job",
          mutation: Ref.update(attempts, (count) => count + 1).pipe(
            Effect.andThen(Effect.fail("rejected")),
          ),
          reconcile: () => Effect.succeed(Option.some(undefined)),
        })
        .pipe(Effect.exit);

      assert.isTrue(exit._tag === "Failure");
      assert.strictEqual(yield* Ref.get(attempts), 1);
    }),
  ),
);

it.effect(
  "keeps an acknowledged mutation successful when its authority refresh fails",
  () =>
    Effect.scoped(
      Effect.gen(function* () {
        const workflow = yield* makeRoborevWorkflow;
        const refreshFailures = yield* Ref.make(0);
        const result = yield* workflow.mutate({
          key: "review:42",
          operation: "add Roborev comment",
          mutation: Effect.succeed("accepted"),
          reconcile: () => Effect.fail(new Error("refresh unavailable")),
          onAcknowledgedRefreshFailure: () =>
            Ref.update(refreshFailures, (count) => count + 1),
        });

        assert.strictEqual(result, "accepted");
        assert.strictEqual(yield* Ref.get(refreshFailures), 1);
      }),
    ),
);

it.effect(
  "reconciles a retained uncertain mutation before accepting another write for its target",
  () =>
    Effect.scoped(
      Effect.gen(function* () {
        const workflow = yield* makeRoborevWorkflow;
        const originalAttempts = yield* Ref.make(0);
        const replacementAttempts = yield* Ref.make(0);
        const authorityAvailable = yield* Ref.make(false);
        const originalReconcile = Ref.get(authorityAvailable).pipe(
          Effect.flatMap((available) =>
            available
              ? Effect.succeed(Option.none<string>())
              : Effect.fail(new Error("authority unavailable")),
          ),
        );

        const firstExit = yield* workflow
          .mutate({
            key: "review:42",
            operation: "add Roborev comment",
            mutation: Ref.update(originalAttempts, (count) => count + 1).pipe(
              Effect.andThen(
                Effect.fail(
                  TransientTransportError.make({
                    operation: "add Roborev comment",
                    cause: new Error("response lost"),
                  }),
                ),
              ),
            ),
            reconcile: () => originalReconcile,
          })
          .pipe(Effect.exit);

        assert.isTrue(firstExit._tag === "Failure");
        assert.strictEqual(yield* Ref.get(originalAttempts), 1);

        yield* Ref.set(authorityAvailable, true);
        const replacement = yield* workflow.mutate({
          key: "review:42",
          operation: "add Roborev comment",
          mutation: Ref.update(replacementAttempts, (count) => count + 1).pipe(
            Effect.as("replacement"),
          ),
          reconcile: () => Effect.succeed(Option.some("replacement")),
        });

        assert.strictEqual(replacement, "replacement");
        assert.strictEqual(yield* Ref.get(originalAttempts), 1);
        assert.strictEqual(yield* Ref.get(replacementAttempts), 1);
      }),
    ),
);

it.effect(
  "distinguishes an original uncertain mutation from a later blocked action",
  () =>
    Effect.scoped(
      Effect.gen(function* () {
        const workflow = yield* makeRoborevWorkflow;
        const reconcile = Effect.fail(new Error("authority unavailable"));
        const original = yield* workflow
          .mutate({
            key: "review:42",
            operation: "add Roborev comment",
            mutation: Effect.fail(
              TransientTransportError.make({
                operation: "add Roborev comment",
                cause: new Error("response lost"),
              }),
            ),
            reconcile: () => reconcile,
          })
          .pipe(Effect.exit);
        const blocked = yield* workflow
          .mutate({
            key: "review:42",
            operation: "add Roborev comment",
            mutation: Effect.succeed("replacement"),
            reconcile: () => Effect.succeed(Option.some("replacement")),
          })
          .pipe(Effect.exit);

        assert.isTrue(original._tag === "Failure");
        assert.isTrue(blocked._tag === "Failure");
        if (original._tag === "Failure" && blocked._tag === "Failure") {
          assert.include(
            String(original.cause),
            "RoborevMutationOutcomeUnknown",
          );
          assert.include(
            String(blocked.cause),
            "RoborevMutationBlockedUnknown",
          );
        }
      }),
    ),
);

it.effect(
  "opens the replacement event stream before reconciling a reconnect",
  () => {
    let fetchCount = 0;
    let fetchCountAtReconnect = 0;
    let resolveFirstFetch!: () => void;
    const firstFetch = new Promise<void>((resolve) => {
      resolveFirstFetch = resolve;
    });
    let resolveFirstError!: () => void;
    const firstError = new Promise<void>((resolve) => {
      resolveFirstError = resolve;
    });
    let resolveReconnect!: () => void;
    const reconnected = new Promise<void>((resolve) => {
      resolveReconnect = resolve;
    });
    return Effect.scoped(
      Effect.gen(function* () {
        const workflow = yield* makeRoborevWorkflow;

        yield* workflow.connectEvents({
          owner: "reconnect-owner",
          baseUrl: "http://roborev.test",
          onInitialOpen: Effect.void,
          onOpen: Effect.void,
          onEvent: () => Effect.void,
          onReconnect: () =>
            Effect.sync(() => {
              fetchCountAtReconnect = fetchCount;
              resolveReconnect();
            }),
          onError: () => Effect.sync(resolveFirstError),
        });

        yield* Effect.promise(() => firstFetch);
        yield* Effect.promise(() => firstError);
        yield* TestClock.adjust("500 millis");
        yield* Effect.promise(() => reconnected);

        assert.strictEqual(fetchCountAtReconnect, 2);
        yield* workflow.disconnectEvents("reconnect-owner");
      }),
    ).pipe(
      Effect.provideService(StreamingFetch, {
        fetch: async () => {
          fetchCount += 1;
          if (fetchCount === 1) {
            resolveFirstFetch();
            return new Response("");
          }
          return new Response(new ReadableStream<Uint8Array>());
        },
      }),
    );
  },
);

it.effect(
  "stops active and pending panel refreshes for only the released owner",
  () =>
    Effect.scoped(
      Effect.gen(function* () {
        const workflow = yield* makeRoborevWorkflow;
        const firstStarted = yield* Deferred.make<void>();
        const releaseFirst = yield* Deferred.make<void>();
        const otherStarted = yield* Deferred.make<void>();
        const releaseOther = yield* Deferred.make<void>();
        const published = yield* Ref.make<ReadonlyArray<string>>([]);
        const panelOptions = (read: Effect.Effect<string>) => ({
          read,
          onStart: Effect.void,
          onSuccess: (value: string) =>
            Ref.update(published, (values) => [...values, value]),
          onFailure: () => Effect.void,
          onSettled: Effect.void,
        });

        yield* workflow.panel(
          "released-owner",
          "panel-1",
          panelOptions(
            Deferred.succeed(firstStarted, undefined).pipe(
              Effect.andThen(Deferred.await(releaseFirst)),
              Effect.as("released-active"),
            ),
          ),
        );
        yield* Deferred.await(firstStarted);
        yield* workflow.panel(
          "released-owner",
          "panel-1",
          panelOptions(Effect.succeed("released-pending")),
        );
        yield* workflow.panel(
          "other-owner",
          "panel-2",
          panelOptions(
            Deferred.succeed(otherStarted, undefined).pipe(
              Effect.andThen(Deferred.await(releaseOther)),
              Effect.as("other"),
            ),
          ),
        );
        yield* Deferred.await(otherStarted);

        yield* workflow.stop("released-owner");
        yield* Deferred.succeed(releaseFirst, undefined);
        yield* Deferred.succeed(releaseOther, undefined);
        yield* Effect.yieldNow;
        yield* Effect.yieldNow;

        assert.deepStrictEqual(yield* Ref.get(published), ["other"]);
      }),
    ),
);
