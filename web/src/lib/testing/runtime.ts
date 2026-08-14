import { ManagedRuntime } from "effect";

import { AppLiveLayer } from "../runtime/layer";
import {
  makeAppRuntimeBoundary,
  type OwnedAppRuntime,
} from "../runtime/runtime";

export function makeTestAppRuntime(): OwnedAppRuntime {
  return makeAppRuntimeBoundary(ManagedRuntime.make(AppLiveLayer));
}
