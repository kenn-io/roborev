import {
  Context,
  Deferred,
  Effect,
  Exit,
  Fiber,
  FiberMap,
  Layer,
  Option,
  Queue,
  Ref,
  Schedule,
  Schema,
  Stream,
} from "effect";
import { TransientTransportError } from "../../api/effect-errors";
import { roborevEventStream, RoborevStreamError } from "../../api/client";
import type { RoborevEvent } from "../../api/schemas";
import { RoborevStreamOpened } from "../../api/schemas";
import { reconnectSchedule } from "../../api/retry-policy";
import type { StreamingFetch } from "../../browser/streaming-fetch";
import { makeLatestCommandByKey } from "../../effect/latest-command-by-key";
import type { CommandQueueClosed } from "../../effect/ordered-command-queue";
import { makeOrderedCommandQueue } from "../../effect/ordered-command-queue";

export interface RoborevWorkflowService {
  readonly catalog: <A, E, R>(
    owner: string,
    request: Effect.Effect<A, E, R>,
  ) => Effect.Effect<A, E, R>;
  readonly jobs: <A, E, R>(
    owner: string,
    request: Effect.Effect<A, E, R>,
  ) => Effect.Effect<A, E, R>;
  readonly review: <A, E, R>(
    owner: string,
    jobID: number,
    request: Effect.Effect<A, E, R>,
  ) => Effect.Effect<A, E, R>;
  readonly stopReview: (owner: string) => Effect.Effect<void>;
  readonly log: <A, E, R>(
    owner: string,
    jobID: number,
    request: Effect.Effect<A, E, R>,
  ) => Effect.Effect<A, E, R>;
  readonly stopLog: (owner: string) => Effect.Effect<void>;
  readonly stopCatalog: (owner: string) => Effect.Effect<void>;
  readonly panel: <A, E, R, StartR, SuccessR, FailureR, SettledR>(
    owner: string,
    panelRun: string,
    options: {
      readonly read: Effect.Effect<A, E, R>;
      readonly onStart: Effect.Effect<void, never, StartR>;
      readonly onSuccess: (value: A) => Effect.Effect<void, never, SuccessR>;
      readonly onFailure: (failure: E) => Effect.Effect<void, never, FailureR>;
      readonly onSettled: Effect.Effect<void, never, SettledR>;
    },
  ) => Effect.Effect<void, never, R | StartR | SuccessR | FailureR | SettledR>;
  readonly stop: (owner: string) => Effect.Effect<void>;
  readonly connectEvents: (options: {
    readonly owner: string;
    readonly baseUrl: string;
    readonly onInitialOpen: Effect.Effect<void, RoborevStreamError>;
    readonly onOpen: Effect.Effect<void>;
    readonly onEvent: (
      event: RoborevEvent,
    ) => Effect.Effect<void, RoborevStreamError>;
    readonly onEventReconcile?:
      | Effect.Effect<void, RoborevStreamError>
      | undefined;
    readonly onReconnect: (
      checkpoint: string | undefined,
    ) => Effect.Effect<void, RoborevStreamError>;
    readonly onError: (failure: RoborevStreamError) => Effect.Effect<void>;
  }) => Effect.Effect<void, never, StreamingFetch>;
  readonly disconnectEvents: (owner: string) => Effect.Effect<void>;
  readonly mutate: <A, E, R, ReconcileE, ReconcileR>(
    command: RoborevMutationCommand<A, E, R, ReconcileE, ReconcileR>,
  ) => Effect.Effect<
    A,
    | E
    | RoborevMutationBlocked
    | RoborevMutationBlockedUnknown
    | RoborevMutationNotApplied
    | RoborevMutationOutcomeUnknown
    | CommandQueueClosed,
    R | ReconcileR
  >;
}

export interface RoborevMutationCommand<A, E, R, ReconcileE, ReconcileR> {
  readonly key: string;
  readonly operation: string;
  readonly mutation: Effect.Effect<A, E | TransientTransportError, R>;
  readonly reconcile: (
    acknowledged: Option.Option<A>,
  ) => Effect.Effect<Option.Option<A>, ReconcileE, ReconcileR>;
  readonly onAcknowledgedRefreshFailure?:
    | ((failure: ReconcileE) => Effect.Effect<void>)
    | undefined;
}

export class RoborevWorkflow extends Context.Service<
  RoborevWorkflow,
  RoborevWorkflowService
>()("roborev/RoborevWorkflow") {}

let nextRoborevOwner = 0;

export function makeRoborevOwner(prefix: string): string {
  nextRoborevOwner += 1;
  return `${prefix}:${nextRoborevOwner}`;
}

export class RoborevMutationError extends Schema.TaggedErrorClass<RoborevMutationError>()(
  "RoborevMutationError",
  {
    operation: Schema.String,
    cause: Schema.Defect(),
  },
) {}

export class RoborevMutationBlocked extends Schema.TaggedErrorClass<RoborevMutationBlocked>()(
  "RoborevMutationBlocked",
  {
    key: Schema.String,
    operation: Schema.String,
    resolution: Schema.Literal("applied"),
  },
) {}

export class RoborevMutationNotApplied extends Schema.TaggedErrorClass<RoborevMutationNotApplied>()(
  "RoborevMutationNotApplied",
  {
    key: Schema.String,
    operation: Schema.String,
  },
) {}

export class RoborevMutationBlockedUnknown extends Schema.TaggedErrorClass<RoborevMutationBlockedUnknown>()(
  "RoborevMutationBlockedUnknown",
  {
    key: Schema.String,
    operation: Schema.String,
    cause: Schema.Defect(),
    reconciliationCause: Schema.Defect(),
  },
) {}

export class RoborevMutationOutcomeUnknown extends Schema.TaggedErrorClass<RoborevMutationOutcomeUnknown>()(
  "RoborevMutationOutcomeUnknown",
  {
    key: Schema.String,
    operation: Schema.String,
    cause: Schema.Defect(),
    reconciliationCause: Schema.Defect(),
  },
) {}

export function roborevMutationFailureMessage(
  fallback: string,
  failure: unknown,
): string {
  if (failure instanceof RoborevMutationBlocked) {
    return `The previous ${failure.operation} was applied. This action was not sent.`;
  }
  if (failure instanceof RoborevMutationBlockedUnknown) {
    return `Could not confirm whether the previous ${failure.operation} was applied. This action was not sent.`;
  }
  if (failure instanceof RoborevMutationOutcomeUnknown) {
    return `Could not confirm whether the ${failure.operation} was applied. Matching actions remain blocked until Roborev authority is available.`;
  }
  return fallback;
}

export class RoborevResponseError extends Schema.TaggedErrorClass<RoborevResponseError>()(
  "RoborevResponseError",
  {
    operation: Schema.String,
    message: Schema.String,
    cause: Schema.Defect(),
  },
) {}

interface RoborevMutationFence {
  readonly key: string;
  readonly operation: string;
  readonly cause: TransientTransportError;
  readonly reconcile: Effect.Effect<Option.Option<unknown>, unknown>;
}

export const makeRoborevWorkflow: Effect.Effect<
  RoborevWorkflowService,
  never,
  import("effect/Scope").Scope
> = Effect.gen(function* () {
  const scope = yield* Effect.scope;
  const jobFibers = yield* FiberMap.make<string, unknown, unknown>();
  const catalogFibers = yield* FiberMap.make<string, unknown, unknown>();
  const reviewFibers = yield* FiberMap.make<string, unknown, unknown>();
  const logFibers = yield* FiberMap.make<string, unknown, unknown>();
  const eventFibers = yield* FiberMap.make<string, void, never>();
  const eventAttempts = yield* Ref.make<ReadonlyMap<string, number>>(new Map());
  const eventCheckpoints = yield* Ref.make<ReadonlyMap<string, string>>(
    new Map(),
  );
  const panelCommands = yield* makeLatestCommandByKey<never>(
    "Roborev panel reads",
  );
  const panelGenerations = yield* Ref.make<ReadonlyMap<string, number>>(
    new Map(),
  );
  const panelKeysByOwner = yield* Ref.make<
    ReadonlyMap<string, ReadonlySet<string>>
  >(new Map());
  const mutationFences = yield* Ref.make<
    ReadonlyMap<string, RoborevMutationFence>
  >(new Map());
  const mutations = yield* makeOrderedCommandQueue(
    "Roborev mutations",
    (command: Effect.Effect<void>) => command,
    64,
  );

  const mutate = Effect.fn("RoborevWorkflow.mutate")(function* <
    A,
    E,
    R,
    ReconcileE,
    ReconcileR,
  >(submitted: RoborevMutationCommand<A, E, R, ReconcileE, ReconcileR>) {
    const context = yield* Effect.context<R | ReconcileR>();
    const result =
      yield* Deferred.make<
        Exit.Exit<
          A,
          | E
          | RoborevMutationBlocked
          | RoborevMutationBlockedUnknown
          | RoborevMutationNotApplied
          | RoborevMutationOutcomeUnknown
        >
      >();
    const reconcileUnknown = submitted
      .reconcile(Option.none())
      .pipe(Effect.provide(context));
    const clearFence = Ref.update(mutationFences, (fences) => {
      const next = new Map(fences);
      next.delete(submitted.key);
      return next;
    });
    const execute = Effect.gen(function* () {
      const existingFence = (yield* Ref.get(mutationFences)).get(submitted.key);
      if (existingFence !== undefined) {
        const reconciliation = yield* Effect.exit(existingFence.reconcile);
        if (Exit.isFailure(reconciliation)) {
          return yield* Effect.fail(
            RoborevMutationBlockedUnknown.make({
              key: existingFence.key,
              operation: existingFence.operation,
              cause: existingFence.cause,
              reconciliationCause: reconciliation.cause,
            }),
          );
        }
        yield* clearFence;
        if (Option.isSome(reconciliation.value)) {
          return yield* Effect.fail(
            RoborevMutationBlocked.make({
              key: existingFence.key,
              operation: existingFence.operation,
              resolution: "applied",
            }),
          );
        }
      }

      return yield* submitted.mutation.pipe(
        Effect.provide(context),
        Effect.flatMap((value) =>
          submitted.reconcile(Option.some(value)).pipe(
            Effect.provide(context),
            Effect.catch(
              (failure) =>
                submitted.onAcknowledgedRefreshFailure?.(failure) ??
                Effect.void,
            ),
            Effect.as(value),
          ),
        ),
        Effect.catchIf(
          (failure): failure is TransientTransportError =>
            failure instanceof TransientTransportError,
          (cause) =>
            Effect.gen(function* () {
              const fence: RoborevMutationFence = {
                key: submitted.key,
                operation: submitted.operation,
                cause,
                reconcile: reconcileUnknown,
              };
              yield* Ref.update(mutationFences, (fences) =>
                new Map(fences).set(submitted.key, fence),
              );
              const reconciliation = yield* Effect.exit(reconcileUnknown);
              if (Exit.isFailure(reconciliation)) {
                return yield* Effect.fail(
                  RoborevMutationOutcomeUnknown.make({
                    key: submitted.key,
                    operation: submitted.operation,
                    cause,
                    reconciliationCause: reconciliation.cause,
                  }),
                );
              }
              yield* clearFence;
              if (Option.isSome(reconciliation.value))
                return reconciliation.value.value;
              return yield* Effect.fail(
                RoborevMutationNotApplied.make({
                  key: submitted.key,
                  operation: submitted.operation,
                }),
              );
            }),
        ),
      );
    });
    const command = Effect.exit(execute).pipe(
      Effect.flatMap((exit) => Deferred.succeed(result, exit)),
      Effect.asVoid,
    );
    const completion = yield* mutations.accept(command);
    yield* completion;
    return yield* Deferred.await(result).pipe(Effect.flatMap((exit) => exit));
  });

  const eventRetrySchedule = reconnectSchedule.pipe(
    Schedule.while(
      ({ input }) => input instanceof RoborevStreamError && input.retryable,
    ),
  );

  const connectEvents = Effect.fn("RoborevWorkflow.connectEvents")(
    function* (options: {
      readonly owner: string;
      readonly baseUrl: string;
      readonly onInitialOpen: Effect.Effect<void, RoborevStreamError>;
      readonly onOpen: Effect.Effect<void>;
      readonly onEvent: (
        event: RoborevEvent,
      ) => Effect.Effect<void, RoborevStreamError>;
      readonly onEventReconcile?:
        | Effect.Effect<void, RoborevStreamError>
        | undefined;
      readonly onReconnect: (
        checkpoint: string | undefined,
      ) => Effect.Effect<void, RoborevStreamError>;
      readonly onError: (failure: RoborevStreamError) => Effect.Effect<void>;
    }) {
      yield* Ref.update(eventAttempts, (attempts) => {
        const next = new Map(attempts);
        next.set(options.owner, 0);
        return next;
      });
      const reconcileSignals = yield* Queue.sliding<true>(1);
      const stream = Stream.unwrap(
        Ref.modify(
          eventAttempts,
          (attempts): readonly [number, ReadonlyMap<string, number>] => {
            const attempt = attempts.get(options.owner) ?? 0;
            const next = new Map(attempts);
            next.set(options.owner, attempt + 1);
            return [attempt, next];
          },
        ).pipe(
          Effect.map((attempt) =>
            roborevEventStream(options.baseUrl).pipe(
              Stream.tap((event) => {
                if (event instanceof RoborevStreamOpened) {
                  const reconcile =
                    attempt === 0
                      ? options.onInitialOpen
                      : Ref.get(eventCheckpoints).pipe(
                          Effect.flatMap((checkpoints) =>
                            options.onReconnect(checkpoints.get(options.owner)),
                          ),
                        );
                  return reconcile.pipe(Effect.andThen(options.onOpen));
                }
                const scheduleReconcile =
                  options.onEventReconcile === undefined
                    ? Effect.void
                    : Queue.offer(reconcileSignals, true).pipe(Effect.asVoid);
                return options.onEvent(event).pipe(
                  Effect.andThen(scheduleReconcile),
                  Effect.andThen(
                    Ref.update(eventCheckpoints, (checkpoints) => {
                      const next = new Map(checkpoints);
                      next.set(options.owner, event.ts);
                      return next;
                    }),
                  ),
                );
              }),
            ),
          ),
        ),
      ).pipe(
        Stream.tapError(options.onError),
        Stream.retry(eventRetrySchedule),
      );
      const runStream = Stream.runDrain(stream).pipe(
        Effect.catch(() => Effect.void),
      );
      const run =
        options.onEventReconcile === undefined
          ? runStream
          : Effect.zip(
              runStream,
              Effect.forever(
                Queue.take(reconcileSignals).pipe(
                  Effect.andThen(options.onEventReconcile),
                  Effect.retry(eventRetrySchedule),
                  Effect.catch(() => Effect.void),
                ),
              ),
              { concurrent: true },
            ).pipe(Effect.asVoid);
      yield* FiberMap.run(eventFibers, options.owner, run);
    },
  );

  const disconnectEvents = (owner: string) =>
    FiberMap.remove(eventFibers, owner).pipe(
      Effect.andThen(
        Ref.update(eventAttempts, (attempts) => {
          const next = new Map(attempts);
          next.delete(owner);
          return next;
        }),
      ),
      Effect.andThen(
        Ref.update(eventCheckpoints, (checkpoints) => {
          const next = new Map(checkpoints);
          next.delete(owner);
          return next;
        }),
      ),
    );

  return {
    catalog: <A, E, R>(owner: string, request: Effect.Effect<A, E, R>) =>
      FiberMap.run(catalogFibers, owner, request).pipe(
        Effect.flatMap(Fiber.join),
      ),
    jobs: <A, E, R>(owner: string, request: Effect.Effect<A, E, R>) =>
      FiberMap.run(jobFibers, owner, request).pipe(Effect.flatMap(Fiber.join)),
    review: <A, E, R>(
      owner: string,
      _jobID: number,
      request: Effect.Effect<A, E, R>,
    ) =>
      FiberMap.run(reviewFibers, owner, request).pipe(
        Effect.flatMap(Fiber.join),
      ),
    stopReview: (owner: string) => FiberMap.remove(reviewFibers, owner),
    log: <A, E, R>(
      owner: string,
      _jobID: number,
      request: Effect.Effect<A, E, R>,
    ) =>
      FiberMap.run(logFibers, owner, request).pipe(Effect.flatMap(Fiber.join)),
    stopLog: (owner: string) => FiberMap.remove(logFibers, owner),
    stopCatalog: (owner: string) => FiberMap.remove(catalogFibers, owner),
    panel: <A, E, R, StartR, SuccessR, FailureR, SettledR>(
      owner: string,
      panelRun: string,
      options: {
        readonly read: Effect.Effect<A, E, R>;
        readonly onStart: Effect.Effect<void, never, StartR>;
        readonly onSuccess: (value: A) => Effect.Effect<void, never, SuccessR>;
        readonly onFailure: (
          failure: E,
        ) => Effect.Effect<void, never, FailureR>;
        readonly onSettled: Effect.Effect<void, never, SettledR>;
      },
    ) => {
      const key = JSON.stringify([owner, panelRun]);
      return Effect.gen(function* () {
        const context = yield* Effect.context<
          R | StartR | SuccessR | FailureR | SettledR
        >();
        yield* Ref.update(panelKeysByOwner, (keysByOwner) => {
          const next = new Map(keysByOwner);
          const ownedKeys = new Set(keysByOwner.get(owner));
          ownedKeys.add(key);
          next.set(owner, ownedKeys);
          return next;
        });
        const generation = yield* Ref.modify(
          panelGenerations,
          (generations): readonly [number, ReadonlyMap<string, number>] => {
            const nextGeneration = (generations.get(key) ?? 0) + 1;
            const next = new Map(generations);
            next.set(key, nextGeneration);
            return [nextGeneration, next];
          },
        );
        yield* options.onStart;
        const ownsGeneration = <Requirements>(
          callback: Effect.Effect<void, never, Requirements>,
        ): Effect.Effect<void, never, Requirements> =>
          Ref.get(panelGenerations).pipe(
            Effect.flatMap((generations) =>
              generations.get(key) === generation ? callback : Effect.void,
            ),
          );
        const command = Effect.matchEffect(options.read, {
          onFailure: (failure) => ownsGeneration(options.onFailure(failure)),
          onSuccess: (value) => ownsGeneration(options.onSuccess(value)),
        }).pipe(
          Effect.ensuring(ownsGeneration(options.onSettled)),
          Effect.provide(context),
        );
        yield* Effect.forkIn(
          panelCommands.submit(key, command).pipe(Effect.ignore),
          scope,
        );
      });
    },
    stop: (owner: string) =>
      Effect.gen(function* () {
        const panelKeys = yield* Ref.modify(
          panelKeysByOwner,
          (
            keysByOwner,
          ): readonly [
            ReadonlyArray<string>,
            ReadonlyMap<string, ReadonlySet<string>>,
          ] => {
            const ownedKeys = Array.from(keysByOwner.get(owner) ?? []);
            const next = new Map(keysByOwner);
            next.delete(owner);
            return [ownedKeys, next];
          },
        );
        yield* Ref.update(panelGenerations, (generations) => {
          const next = new Map(generations);
          for (const key of panelKeys) next.delete(key);
          return next;
        });
        yield* Effect.forEach(panelKeys, panelCommands.cancel, {
          discard: true,
        });
        yield* FiberMap.remove(jobFibers, owner);
        yield* FiberMap.remove(catalogFibers, owner);
        yield* FiberMap.remove(reviewFibers, owner);
        yield* FiberMap.remove(logFibers, owner);
        yield* disconnectEvents(owner);
      }),
    connectEvents,
    disconnectEvents,
    mutate,
  };
});

export const RoborevWorkflowLive =
  Layer.effect(RoborevWorkflow)(makeRoborevWorkflow);
