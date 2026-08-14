import {
  Cause,
  Deferred,
  Effect,
  Exit,
  FiberMap,
  Option,
  Ref,
  Semaphore,
} from "effect";
import type { Scope } from "effect/Scope";
import { CommandQueueClosed } from "./ordered-command-queue";

interface CommandEntry<Error> {
  readonly command: Effect.Effect<void, Error>;
  readonly acknowledgement: Deferred.Deferred<
    Exit.Exit<void, Error | CommandQueueClosed>
  >;
}

interface KeyState<Error> {
  readonly active: Option.Option<CommandEntry<Error>>;
  readonly pending: Option.Option<CommandEntry<Error>>;
  readonly cancelling: boolean;
  readonly workerKey: string;
}

interface AcceptResult<Error> {
  readonly startWorker: boolean;
  readonly superseded: Option.Option<CommandEntry<Error>>;
  readonly workerKey: string;
}

export interface LatestCommandByKey<Error> {
  readonly submit: (
    key: string,
    command: Effect.Effect<void, Error>,
  ) => Effect.Effect<void, Error | CommandQueueClosed>;
  readonly cancel: (key: string) => Effect.Effect<void>;
  readonly shutdown: Effect.Effect<void>;
}

export function makeLatestCommandByKey<Error>(
  name: string,
  blocksPending: (failure: Error) => boolean = () => false,
): Effect.Effect<LatestCommandByKey<Error>, never, Scope> {
  return Effect.gen(function* () {
    const states = yield* Ref.make<ReadonlyMap<string, KeyState<Error>>>(
      new Map(),
    );
    const closed = yield* Ref.make(false);
    const acceptance = yield* Semaphore.make(1);
    const workers = yield* FiberMap.make<string, void, never>();
    const acknowledgements = new Set<
      Deferred.Deferred<Exit.Exit<void, Error | CommandQueueClosed>>
    >();
    let nextWorkerID = 0;
    const newWorkerKey = (key: string) => `${key}\0${++nextWorkerID}`;

    const takeNext = (key: string) =>
      acceptance.withPermit(
        Ref.modify(
          states,
          (
            current,
          ): readonly [
            Option.Option<CommandEntry<Error>>,
            ReadonlyMap<string, KeyState<Error>>,
          ] => {
            const state = current.get(key);
            if (state === undefined) return [Option.none(), current];
            if (state.cancelling) return [Option.none(), current];
            const next = new Map(current);
            if (Option.isNone(state.pending)) {
              next.delete(key);
              return [Option.none(), next];
            }
            next.set(key, {
              active: state.pending,
              pending: Option.none(),
              cancelling: false,
              workerKey: state.workerKey,
            });
            return [state.pending, next];
          },
        ),
      );

    const failPending = (key: string, failure: Error) =>
      acceptance.withPermit(
        Effect.gen(function* () {
          const pending = yield* Ref.modify(
            states,
            (
              current,
            ): readonly [
              Option.Option<CommandEntry<Error>>,
              ReadonlyMap<string, KeyState<Error>>,
            ] => {
              const entry = current.get(key)?.pending ?? Option.none();
              const next = new Map(current);
              next.delete(key);
              return [entry, next];
            },
          );
          if (Option.isNone(pending)) return;
          acknowledgements.delete(pending.value.acknowledgement);
          yield* Deferred.succeed(
            pending.value.acknowledgement,
            Exit.fail(failure),
          );
        }),
      );

    const clearActive = (key: string) =>
      acceptance.withPermit(
        Ref.update(states, (current) => {
          const state = current.get(key);
          if (state === undefined) return current;
          const next = new Map(current);
          next.set(key, { ...state, active: Option.none() });
          return next;
        }),
      );

    function consume(key: string): Effect.Effect<void> {
      return Effect.suspend(() =>
        takeNext(key).pipe(
          Effect.flatMap((entry) => {
            if (Option.isNone(entry)) return Effect.void;
            return Effect.exit(entry.value.command).pipe(
              Effect.tap((exit) =>
                Deferred.succeed(entry.value.acknowledgement, exit),
              ),
              Effect.tap(() =>
                Effect.sync(() =>
                  acknowledgements.delete(entry.value.acknowledgement),
                ),
              ),
              Effect.tap(() => clearActive(key)),
              Effect.flatMap((exit) => {
                if (Exit.isFailure(exit)) {
                  const failure = Cause.findErrorOption(exit.cause);
                  if (Option.isSome(failure) && blocksPending(failure.value)) {
                    return failPending(key, failure.value);
                  }
                }
                return consume(key);
              }),
            );
          }),
        ),
      );
    }

    const submit = Effect.fn("LatestCommandByKey.submit")(function* (
      key: string,
      command: Effect.Effect<void, Error>,
    ) {
      const acknowledgement =
        yield* Deferred.make<Exit.Exit<void, Error | CommandQueueClosed>>();
      yield* acceptance.withPermit(
        Effect.gen(function* () {
          if (yield* Ref.get(closed)) {
            return yield* Effect.fail(new CommandQueueClosed({ queue: name }));
          }
          acknowledgements.add(acknowledgement);
          const entry: CommandEntry<Error> = { command, acknowledgement };
          const accepted = yield* Ref.modify(
            states,
            (
              current,
            ): readonly [
              AcceptResult<Error>,
              ReadonlyMap<string, KeyState<Error>>,
            ] => {
              const existing = current.get(key);
              const workerKey = existing?.workerKey ?? newWorkerKey(key);
              const next = new Map(current);
              next.set(key, {
                active: existing?.active ?? Option.none(),
                pending: Option.some(entry),
                cancelling: existing?.cancelling ?? false,
                workerKey,
              });
              return [
                {
                  startWorker: existing === undefined,
                  superseded: existing?.pending ?? Option.none(),
                  workerKey,
                },
                next,
              ];
            },
          );
          if (Option.isSome(accepted.superseded)) {
            acknowledgements.delete(accepted.superseded.value.acknowledgement);
            yield* Deferred.succeed(
              accepted.superseded.value.acknowledgement,
              Exit.succeed(undefined),
            );
          }
          if (accepted.startWorker)
            yield* FiberMap.run(workers, accepted.workerKey, consume(key));
        }),
      );
      return yield* Deferred.await(acknowledgement).pipe(
        Effect.flatMap((exit) => exit),
      );
    });

    const cancel = Effect.fn("LatestCommandByKey.cancel")(function* (
      key: string,
    ) {
      const cancellation = yield* acceptance.withPermit(
        Ref.modify(
          states,
          (
            current,
          ): readonly [
            {
              pending: Option.Option<CommandEntry<Error>>;
              active: Option.Option<CommandEntry<Error>>;
              workerKey: Option.Option<string>;
            },
            ReadonlyMap<string, KeyState<Error>>,
          ] => {
            const state = current.get(key);
            if (state === undefined) {
              return [
                {
                  active: Option.none(),
                  pending: Option.none(),
                  workerKey: Option.none(),
                },
                current,
              ];
            }
            const next = new Map(current);
            next.set(key, {
              active: state.active,
              pending: Option.none(),
              cancelling: true,
              workerKey: state.workerKey,
            });
            return [
              {
                active: state.active,
                pending: state.pending,
                workerKey: Option.some(state.workerKey),
              },
              next,
            ];
          },
        ),
      );
      if (Option.isSome(cancellation.active)) {
        acknowledgements.delete(cancellation.active.value.acknowledgement);
        yield* Deferred.succeed(
          cancellation.active.value.acknowledgement,
          Exit.interrupt(),
        );
      }
      if (Option.isSome(cancellation.pending)) {
        acknowledgements.delete(cancellation.pending.value.acknowledgement);
        yield* Deferred.succeed(
          cancellation.pending.value.acknowledgement,
          Exit.interrupt(),
        );
      }
      if (Option.isSome(cancellation.workerKey)) {
        yield* FiberMap.remove(workers, cancellation.workerKey.value);
      }
      yield* acceptance.withPermit(
        Effect.gen(function* () {
          const shouldRestart = yield* Ref.modify(
            states,
            (
              current,
            ): readonly [boolean, ReadonlyMap<string, KeyState<Error>>] => {
              const state = current.get(key);
              if (state === undefined) return [false, current];
              const next = new Map(current);
              if (Option.isNone(state.pending)) {
                next.delete(key);
                return [false, next];
              }
              next.set(key, {
                active: Option.none(),
                pending: state.pending,
                cancelling: false,
                workerKey: newWorkerKey(key),
              });
              return [true, next];
            },
          );
          if (shouldRestart) {
            const state = yield* Ref.get(states);
            const workerKey = state.get(key)?.workerKey;
            if (workerKey !== undefined) {
              yield* FiberMap.run(workers, workerKey, consume(key));
            }
          }
        }),
      );
    });

    const shutdown = Effect.gen(function* () {
      const shouldClose = yield* acceptance.withPermit(
        Ref.modify(closed, (isClosed): readonly [boolean, boolean] => [
          !isClosed,
          true,
        ]),
      );
      if (!shouldClose) return;
      yield* FiberMap.clear(workers);
      const exit = Exit.fail(new CommandQueueClosed({ queue: name }));
      yield* Effect.forEach(
        Array.from(acknowledgements),
        (acknowledgement) => Deferred.succeed(acknowledgement, exit),
        { discard: true },
      );
      acknowledgements.clear();
      yield* Ref.set(states, new Map());
    });
    yield* Effect.addFinalizer(() => shutdown);

    return { submit, cancel, shutdown };
  });
}
