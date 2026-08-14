import { assert, it } from "@effect/vitest";
import { Cause, Deferred, Effect, Exit, Fiber, Option, Ref } from "effect";
import { makeOrderedCommandQueue } from "./ordered-command-queue";

it.effect("runs commands in submission order", () =>
  Effect.scoped(
    Effect.gen(function* () {
      const observed = yield* Ref.make<ReadonlyArray<number>>([]);
      const queue = yield* makeOrderedCommandQueue(
        "ordered test commands",
        (command: number) =>
          Ref.update(observed, (commands) => [...commands, command]),
        2,
      );

      const submissions = yield* Effect.all(
        [queue.submit(1), queue.submit(2), queue.submit(3)],
        {
          concurrency: "unbounded",
        },
      );

      assert.deepStrictEqual(submissions, [undefined, undefined, undefined]);
      assert.deepStrictEqual(yield* Ref.get(observed), [1, 2, 3]);
    }),
  ),
);

it.effect(
  "acknowledges one command failure without stopping later commands",
  () =>
    Effect.scoped(
      Effect.gen(function* () {
        const observed = yield* Ref.make<ReadonlyArray<number>>([]);
        const queue = yield* makeOrderedCommandQueue(
          "failure-isolating commands",
          (command: number) =>
            Ref.update(observed, (commands) => [...commands, command]).pipe(
              Effect.andThen(
                command === 2
                  ? Effect.fail("rejected")
                  : Effect.succeed(command),
              ),
            ),
          2,
        );

        const exits = yield* Effect.all(
          [queue.submit(1), queue.submit(2), queue.submit(3)].map(Effect.exit),
          {
            concurrency: "unbounded",
          },
        );

        assert.isTrue(Exit.isSuccess(exits[0]));
        assert.isTrue(Exit.isFailure(exits[1]));
        assert.isTrue(Exit.isSuccess(exits[2]));
        assert.deepStrictEqual(yield* Ref.get(observed), [1, 2, 3]);
      }),
    ),
);

it.effect("applies backpressure when its bounded buffer is full", () =>
  Effect.scoped(
    Effect.gen(function* () {
      const releaseFirst = yield* Deferred.make<void>();
      const firstStarted = yield* Deferred.make<void>();
      const thirdCompleted = yield* Deferred.make<void>();
      const queue = yield* makeOrderedCommandQueue(
        "bounded commands",
        (command: number) =>
          command === 1
            ? Deferred.succeed(firstStarted, undefined).pipe(
                Effect.andThen(Deferred.await(releaseFirst)),
              )
            : Effect.void,
        1,
      );

      const first = yield* Effect.forkChild(queue.submit(1));
      yield* Deferred.await(firstStarted);
      const second = yield* Effect.forkChild(queue.submit(2));
      yield* Effect.yieldNow;
      const third = yield* Effect.forkChild(
        queue
          .submit(3)
          .pipe(Effect.andThen(Deferred.succeed(thirdCompleted, undefined))),
      );
      yield* Effect.yieldNow;

      assert.isFalse(yield* Deferred.isDone(thirdCompleted));
      yield* Deferred.succeed(releaseFirst, undefined);
      yield* Fiber.join(first);
      yield* Fiber.join(second);
      yield* Fiber.join(third);
      assert.isTrue(yield* Deferred.isDone(thirdCompleted));
    }),
  ),
);

it.effect("completes active and pending acknowledgements when shut down", () =>
  Effect.scoped(
    Effect.gen(function* () {
      const active = yield* Deferred.make<void>();
      const activeStarted = yield* Deferred.make<void>();
      const queue = yield* makeOrderedCommandQueue(
        "shutdown commands",
        () =>
          Deferred.succeed(activeStarted, undefined).pipe(
            Effect.andThen(Deferred.await(active)),
          ),
        1,
      );
      const first = yield* Effect.forkChild(Effect.exit(queue.submit("first")));
      yield* Deferred.await(activeStarted);
      const second = yield* Effect.forkChild(
        Effect.exit(queue.submit("second")),
      );
      yield* Effect.yieldNow;

      yield* queue.shutdown;
      const firstExit = yield* Fiber.join(first);
      const secondExit = yield* Fiber.join(second);

      assert.strictEqual(firstExit._tag, "Failure");
      assert.strictEqual(secondExit._tag, "Failure");
      if (Exit.isFailure(firstExit) && Exit.isFailure(secondExit)) {
        const firstFailure = Cause.findErrorOption(firstExit.cause);
        const secondFailure = Cause.findErrorOption(secondExit.cause);
        assert.isTrue(Option.isSome(firstFailure));
        assert.isTrue(Option.isSome(secondFailure));
        if (Option.isSome(firstFailure) && Option.isSome(secondFailure)) {
          assert.strictEqual(firstFailure.value._tag, "CommandQueueClosed");
          assert.strictEqual(secondFailure.value._tag, "CommandQueueClosed");
        }
      }
    }),
  ),
);
