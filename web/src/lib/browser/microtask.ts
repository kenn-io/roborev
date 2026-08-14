import { Context, Effect, FiberHandle, Layer } from "effect";
import type { Scope } from "effect/Scope";

interface MicrotaskFactory {
  readonly schedule: (callback: () => void) => void;
}

export class Microtasks extends Context.Service<Microtasks, MicrotaskFactory>()(
  "roborev/browser/Microtasks",
) {}

export const MicrotasksLive = Layer.succeed(Microtasks)({
  schedule: (callback) => queueMicrotask(callback),
});

export const nextMicrotask = Effect.gen(function* () {
  const microtasks = yield* Microtasks;
  return yield* Effect.callback<void>((resume) => {
    microtasks.schedule(() => resume(Effect.void));
  });
});

export interface MicrotaskScheduler {
  readonly cancel: () => void;
  readonly schedule: () => boolean;
}

export function makeMicrotaskScheduler<R>(
  onMicrotask: Effect.Effect<void, never, R>,
): Effect.Effect<MicrotaskScheduler, never, Microtasks | R | Scope> {
  return Effect.gen(function* () {
    const runMicrotask = yield* FiberHandle.makeRuntime<
      Microtasks | R,
      never,
      void
    >();
    let generation = 0;
    let scheduled = false;
    let activeMicrotask: ReturnType<typeof runMicrotask> | undefined;
    return {
      cancel: () => {
        if (!scheduled) return;
        generation += 1;
        scheduled = false;
        activeMicrotask?.interruptUnsafe();
        activeMicrotask = undefined;
      },
      schedule: () => {
        if (scheduled) return false;
        scheduled = true;
        const requestGeneration = generation;
        activeMicrotask = runMicrotask(
          nextMicrotask.pipe(
            Effect.andThen(
              Effect.suspend(() =>
                requestGeneration === generation ? onMicrotask : Effect.void,
              ),
            ),
            Effect.ensuring(
              Effect.sync(() => {
                if (requestGeneration !== generation) return;
                scheduled = false;
                activeMicrotask = undefined;
              }),
            ),
          ),
        );
        return true;
      },
    };
  });
}
