import { Cause, Effect, Exit, Fiber, ManagedRuntime, Option } from "effect";
import type { Effect as EffectType } from "effect/Effect";
import type { Exit as ExitType } from "effect/Exit";
import type {
  Error as LayerError,
  Success as LayerSuccess,
} from "effect/Layer";
import type { ManagedRuntime as ManagedRuntimeType } from "effect/ManagedRuntime";
import { nextMicrotask } from "../browser/microtask";
import { AppLiveLayer } from "./layer";

export type AppServices = LayerSuccess<typeof AppLiveLayer>;
export type AppLayerError = LayerError<typeof AppLiveLayer>;

export interface CommandRunOptions<E> {
  readonly operation: string;
  readonly safeContext: Readonly<Record<string, string | number | boolean>>;
  readonly onFailure: (failure: E | AppLayerError) => void;
}

export interface AppExecution<A, E> {
  readonly interrupt: () => void;
  readonly await: EffectType<ExitType<A, E | AppLayerError>>;
  // Promise-only library callbacks observe the same owned fiber through this edge.
  readonly exit: Promise<ExitType<A, E | AppLayerError>>;
}

export interface AppRuntime {
  readonly runCommand: <A, E>(
    program: EffectType<A, E, AppServices>,
    options: CommandRunOptions<E>,
  ) => AppExecution<A, E>;
  readonly runMicrotask: (
    callback: () => void,
    options: Pick<CommandRunOptions<never>, "operation" | "safeContext">,
  ) => AppExecution<void, never>;
}

export type OwnedAppRuntime = AppRuntime &
  Pick<ManagedRuntimeType<AppServices, AppLayerError>, "disposeEffect">;

export function makeAppRuntimeBoundary(
  managed: ManagedRuntimeType<AppServices, AppLayerError>,
): OwnedAppRuntime {
  const runCommand = <A, E>(
    program: EffectType<A, E, AppServices>,
    options: CommandRunOptions<E>,
  ): AppExecution<A, E> => {
    const fiber = managed.runFork(program);
    const exit = new Promise<ExitType<A, E | AppLayerError>>((resolve) => {
      fiber.addObserver(resolve);
    });
    fiber.addObserver((exit) => {
      if (Exit.isSuccess(exit) || Cause.hasInterruptsOnly(exit.cause)) {
        return;
      }
      if (Cause.hasDies(exit.cause)) {
        console.error("Roborev web command failed with a defect", {
          operation: options.operation,
          context: options.safeContext,
          cause: Cause.pretty(exit.cause),
        });
      }
      const failure = Cause.findErrorOption(exit.cause);
      if (Option.isSome(failure)) {
        options.onFailure(failure.value);
      }
    });
    return {
      interrupt: () => fiber.interruptUnsafe(),
      await: Fiber.await(fiber),
      exit,
    };
  };

  return {
    disposeEffect: managed.disposeEffect,
    runCommand,
    runMicrotask: (callback, options) =>
      runCommand(nextMicrotask.pipe(Effect.andThen(Effect.sync(callback))), {
        ...options,
        onFailure: () => {},
      }),
  };
}

export const makeAppRuntime = (): OwnedAppRuntime =>
  makeAppRuntimeBoundary(ManagedRuntime.make(AppLiveLayer));
