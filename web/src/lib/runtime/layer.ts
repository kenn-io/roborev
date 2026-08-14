import { Clipboard } from "@effect/platform-browser";
import { Layer } from "effect";

import { MicrotasksLive } from "../browser/microtask";
import { StreamingFetchLive } from "../browser/streaming-fetch";
import { RoborevWorkflowLive } from "../stores/roborev/workflow";

export const AppLiveLayer = Layer.mergeAll(
  Clipboard.layer,
  MicrotasksLive,
  StreamingFetchLive,
  RoborevWorkflowLive,
);
