import { assert, it } from "@effect/vitest";
import { Cause, Deferred, Effect, Exit, Fiber, Option, Ref } from "effect";
import { makeLatestCommandByKey } from "./latest-command-by-key";

it.effect(
  "runs the active command and only the latest pending command per key",
  () =>
    Effect.scoped(
      Effect.gen(function* () {
        const commands =
          yield* makeLatestCommandByKey<never>("latest commands");
        const firstStarted = yield* Deferred.make<void>();
        const releaseFirst = yield* Deferred.make<void>();
        const executed = yield* Ref.make<ReadonlyArray<string>>([]);
        const command = (
          value: string,
          wait: Effect.Effect<void> = Effect.void,
        ) =>
          Ref.update(executed, (values) => [...values, value]).pipe(
            Effect.andThen(wait),
          );

        const first = yield* Effect.forkChild(
          commands.submit(
            "pull",
            command(
              "first",
              Deferred.succeed(firstStarted, undefined).pipe(
                Effect.andThen(Deferred.await(releaseFirst)),
              ),
            ),
          ),
        );
        yield* Deferred.await(firstStarted);
        const superseded = yield* Effect.forkChild(
          commands.submit("pull", command("second")),
        );
        const latest = yield* Effect.forkChild(
          commands.submit("pull", command("third")),
        );
        yield* Effect.yieldNow;

        yield* Fiber.join(superseded);
        assert.deepStrictEqual(yield* Ref.get(executed), ["first"]);
        yield* Deferred.succeed(releaseFirst, undefined);
        yield* Fiber.join(first);
        yield* Fiber.join(latest);
        assert.deepStrictEqual(yield* Ref.get(executed), ["first", "third"]);
      }),
    ),
);

it.effect(
  "continues with the latest pending command after an active failure",
  () =>
    Effect.scoped(
      Effect.gen(function* () {
        const commands =
          yield* makeLatestCommandByKey<string>("latest commands");
        const firstStarted = yield* Deferred.make<void>();
        const releaseFirst = yield* Deferred.make<void>();
        const executed = yield* Ref.make<ReadonlyArray<string>>([]);
        const first = yield* Effect.forkChild(
          commands.submit(
            "pull",
            Ref.update(executed, (values) => [...values, "first"]).pipe(
              Effect.andThen(Deferred.succeed(firstStarted, undefined)),
              Effect.andThen(Deferred.await(releaseFirst)),
              Effect.andThen(Effect.fail("failed")),
            ),
          ),
        );
        yield* Deferred.await(firstStarted);
        const latest = yield* Effect.forkChild(
          commands.submit(
            "pull",
            Ref.update(executed, (values) => [...values, "latest"]),
          ),
        );

        yield* Deferred.succeed(releaseFirst, undefined);
        const firstResult = yield* Fiber.await(first);
        yield* Fiber.join(latest);

        assert.isTrue(firstResult._tag === "Failure");
        assert.deepStrictEqual(yield* Ref.get(executed), ["first", "latest"]);
      }),
    ),
);

it.effect("does not execute a pending command after a blocking failure", () =>
  Effect.scoped(
    Effect.gen(function* () {
      const commands = yield* makeLatestCommandByKey<string>(
        "latest commands",
        (failure) => failure === "review",
      );
      const firstStarted = yield* Deferred.make<void>();
      const releaseFirst = yield* Deferred.make<void>();
      const pendingRuns = yield* Ref.make(0);
      const first = yield* Effect.forkChild(
        commands.submit(
          "pull",
          Deferred.succeed(firstStarted, undefined).pipe(
            Effect.andThen(Deferred.await(releaseFirst)),
            Effect.andThen(Effect.fail("review")),
          ),
        ),
      );
      yield* Deferred.await(firstStarted);
      const pending = yield* Effect.forkChild(
        commands.submit(
          "pull",
          Ref.update(pendingRuns, (count) => count + 1),
        ),
      );
      yield* Effect.yieldNow;

      yield* Deferred.succeed(releaseFirst, undefined);
      const firstExit = yield* Fiber.await(first);
      const pendingExit = yield* Fiber.await(pending);

      assert.isTrue(Exit.isFailure(firstExit));
      assert.isTrue(Exit.isFailure(pendingExit));
      if (Exit.isFailure(firstExit) && Exit.isFailure(pendingExit)) {
        assert.deepStrictEqual(
          Cause.findErrorOption(firstExit.cause),
          Option.some("review"),
        );
        assert.deepStrictEqual(
          Cause.findErrorOption(pendingExit.cause),
          Option.some("review"),
        );
      }
      assert.strictEqual(yield* Ref.get(pendingRuns), 0);
    }),
  ),
);

it.effect(
  "completes active and pending acknowledgements when it shuts down",
  () =>
    Effect.scoped(
      Effect.gen(function* () {
        const commands =
          yield* makeLatestCommandByKey<never>("latest commands");
        const started = yield* Deferred.make<void>();
        const active = yield* Effect.forkChild(
          commands.submit(
            "pull",
            Deferred.succeed(started, undefined).pipe(
              Effect.andThen(Effect.never),
            ),
          ),
        );
        yield* Deferred.await(started);
        const pending = yield* Effect.forkChild(
          commands.submit("pull", Effect.void),
        );

        yield* commands.shutdown;
        const activeFailure = yield* Fiber.join(active).pipe(Effect.flip);
        const pendingFailure = yield* Fiber.join(pending).pipe(Effect.flip);

        assert.strictEqual(activeFailure._tag, "CommandQueueClosed");
        assert.strictEqual(pendingFailure._tag, "CommandQueueClosed");
      }),
    ),
);

it.effect(
  "does not interrupt a replacement submitted while cancellation finishes",
  () =>
    Effect.scoped(
      Effect.gen(function* () {
        const commands =
          yield* makeLatestCommandByKey<never>("latest commands");
        const started = yield* Deferred.make<void>();
        const cancellationStarted = yield* Deferred.make<void>();
        const releaseCancellation = yield* Deferred.make<void>();
        const replacementRan = yield* Deferred.make<void>();
        const active = yield* Effect.forkChild(
          commands.submit(
            "pull",
            Deferred.succeed(started, undefined).pipe(
              Effect.andThen(Effect.never),
              Effect.ensuring(
                Deferred.succeed(cancellationStarted, undefined).pipe(
                  Effect.andThen(Deferred.await(releaseCancellation)),
                ),
              ),
            ),
          ),
        );
        yield* Deferred.await(started);
        const cancelling = yield* Effect.forkChild(commands.cancel("pull"));
        yield* Deferred.await(cancellationStarted);
        const replacement = yield* Effect.forkChild(
          commands.submit("pull", Deferred.succeed(replacementRan, undefined)),
        );

        yield* Deferred.succeed(releaseCancellation, undefined);
        yield* Fiber.join(cancelling);
        yield* Deferred.await(replacementRan);
        yield* Fiber.join(replacement);
        yield* Fiber.await(active);
      }),
    ),
);

it.effect(
  "finishes cancellation cleanup when the cancelling fiber is interrupted",
  () =>
    Effect.scoped(
      Effect.gen(function* () {
        const commands =
          yield* makeLatestCommandByKey<never>("latest commands");
        const started = yield* Deferred.make<void>();
        const cancellationStarted = yield* Deferred.make<void>();
        const releaseCancellation = yield* Deferred.make<void>();
        const replacementRan = yield* Deferred.make<void>();
        const active = yield* Effect.forkChild(
          commands.submit(
            "pull",
            Deferred.succeed(started, undefined).pipe(
              Effect.andThen(Effect.never),
              Effect.ensuring(
                Deferred.succeed(cancellationStarted, undefined).pipe(
                  Effect.andThen(Deferred.await(releaseCancellation)),
                ),
              ),
            ),
          ),
        );
        yield* Deferred.await(started);
        const cancelling = yield* Effect.forkChild(commands.cancel("pull"));
        yield* Deferred.await(cancellationStarted);
        const interruption = yield* Effect.forkChild(
          Fiber.interrupt(cancelling),
        );
        const replacement = yield* Effect.forkChild(
          commands.submit("pull", Deferred.succeed(replacementRan, undefined)),
        );

        yield* Deferred.succeed(releaseCancellation, undefined);
        yield* Fiber.join(interruption);
        yield* Deferred.await(replacementRan);
        yield* Fiber.join(replacement);
        yield* Fiber.await(active);
      }),
    ),
);
