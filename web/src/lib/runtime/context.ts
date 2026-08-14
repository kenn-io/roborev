import { getContext, setContext } from "svelte";

import type { RoborevClient } from "../api/client";
import type { AppRuntime } from "./runtime";

export const APP_RUNTIME_KEY = Symbol("roborev-app-runtime");
export const ROBOREV_CLIENT_KEY = Symbol("roborev-client");

export function getAppRuntime(): AppRuntime {
  return getContext(APP_RUNTIME_KEY);
}

export function setAppRuntime(runtime: AppRuntime): AppRuntime {
  return setContext(APP_RUNTIME_KEY, runtime);
}

export function getRoborevClient(): RoborevClient {
  return getContext(ROBOREV_CLIENT_KEY);
}

export function setRoborevClient(client: RoborevClient): RoborevClient {
  return setContext(ROBOREV_CLIENT_KEY, client);
}
