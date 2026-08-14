import { Deferred, Effect, Exit, Fiber, Queue, Ref, Schema } from "effect";
import type { Scope } from "effect/Scope";

export class CommandQueueClosed extends Schema.TaggedErrorClass<CommandQueueClosed>()(
  "CommandQueueClosed",
  {
    queue: Schema.String,
  },
) {}

interface CommandEntry<Input, Output, Error> {
  readonly input: Input;
  readonly acknowledgement: Deferred.Deferred<
    Exit.Exit<Output, Error | CommandQueueClosed>
  >;
}

export interface OrderedCommandQueue<Input, Output, Error> {
  readonly accept: (
    input: Input,
  ) => Effect.Effect<
    Effect.Effect<Output, Error | CommandQueueClosed>,
    CommandQueueClosed
  >;
  readonly submit: (
    input: Input,
  ) => Effect.Effect<Output, Error | CommandQueueClosed>;
  readonly shutdown: Effect.Effect<void>;
}

export const makeOrderedCommandQueue = <Input, Output, Error, Requirements>(
  name: string,
  execute: (input: Input) => Effect.Effect<Output, Error, Requirements>,
  capacity = 64,
): Effect.Effect<
  OrderedCommandQueue<Input, Output, Error>,
  never,
  Requirements | Scope
> =>
  Effect.gen(function* () {
    const scope = yield* Effect.scope;
    const commands =
      yield* Queue.bounded<CommandEntry<Input, Output, Error>>(capacity);
    const closed = yield* Ref.make(false);
    const pending = new Set<
      Deferred.Deferred<Exit.Exit<Output, Error | CommandQueueClosed>>
    >();

    const consume = Effect.forever(
      Effect.gen(function* () {
        const entry = yield* Queue.take(commands);
        const exit = yield* Effect.exit(execute(entry.input));
        yield* Deferred.succeed(entry.acknowledgement, exit);
        yield* Effect.sync(() => pending.delete(entry.acknowledgement));
      }),
    );
    const consumer = yield* Effect.forkIn(consume, scope);
    const shutdown = Effect.gen(function* () {
      const shouldClose = yield* Ref.modify(
        closed,
        (isClosed): readonly [boolean, boolean] => [!isClosed, true],
      );
      if (!shouldClose) return;

      yield* Queue.shutdown(commands);
      yield* Fiber.interrupt(consumer);
      const acknowledgements = yield* Effect.sync(() => Array.from(pending));
      const exit = Exit.fail(new CommandQueueClosed({ queue: name }));
      yield* Effect.forEach(
        acknowledgements,
        (acknowledgement) => Deferred.succeed(acknowledgement, exit),
        {
          discard: true,
        },
      );
      yield* Effect.sync(() => pending.clear());
    });
    yield* Effect.addFinalizer(() => shutdown);

    const accept = (input: Input) =>
      Effect.gen(function* () {
        if (yield* Ref.get(closed)) {
          return yield* Effect.fail(new CommandQueueClosed({ queue: name }));
        }
        const acknowledgement =
          yield* Deferred.make<Exit.Exit<Output, Error | CommandQueueClosed>>();
        yield* Effect.sync(() => pending.add(acknowledgement));
        const offered = yield* Queue.offer(commands, {
          input,
          acknowledgement,
        }).pipe(
          Effect.onInterrupt(() =>
            Effect.sync(() => pending.delete(acknowledgement)),
          ),
        );
        if (!offered) {
          yield* Effect.sync(() => pending.delete(acknowledgement));
          return yield* Effect.fail(new CommandQueueClosed({ queue: name }));
        }
        return Deferred.await(acknowledgement).pipe(
          Effect.flatMap((exit) => exit),
        );
      });

    return {
      accept,
      submit: (input) =>
        accept(input).pipe(
          Effect.flatMap((acknowledgement) => acknowledgement),
        ),
      shutdown,
    };
  });
