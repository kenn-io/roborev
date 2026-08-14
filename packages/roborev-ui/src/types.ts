import type { components } from "./generated";

export type ReviewProjection = components["schemas"]["ReviewProjection"];
export type ReviewProjectionJob = components["schemas"]["ReviewProjectionJob"];
export type ReviewProjectionReview =
  components["schemas"]["ReviewProjectionReview"];
export type ReviewProjectionResponse =
  components["schemas"]["ReviewProjectionResponse"];

export const supportedReviewProjectionSchemaVersions = [1] as const;

export function supportsReviewProjection(
  projection: Pick<ReviewProjection, "schema_version">,
): boolean {
  return supportedReviewProjectionSchemaVersions.includes(
    projection.schema_version as 1,
  );
}
