import { Schema } from "effect";

export class TransientTransportError extends Schema.TaggedErrorClass<TransientTransportError>()(
  "TransientTransportError",
  {
    operation: Schema.String,
    cause: Schema.Defect(),
  },
) {}

export class InvalidExternalPayload extends Schema.TaggedErrorClass<InvalidExternalPayload>()(
  "InvalidExternalPayload",
  {
    operation: Schema.String,
    cause: Schema.Defect(),
  },
) {}

export class BrowserStreamError extends Schema.TaggedErrorClass<BrowserStreamError>()(
  "BrowserStreamError",
  {
    operation: Schema.String,
    cause: Schema.Defect(),
  },
) {}
