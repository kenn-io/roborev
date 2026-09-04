import { getContext, setContext } from "svelte";

import type { AppRuntime } from "./runtime";

export const APP_RUNTIME_KEY = Symbol("roborev-app-runtime");

export function getAppRuntime(): AppRuntime {
  return getContext(APP_RUNTIME_KEY);
}

export function setAppRuntime(runtime: AppRuntime): AppRuntime {
  return setContext(APP_RUNTIME_KEY, runtime);
}
