import type {
  ReviewProjection,
  ReviewProjectionJob,
  ReviewProjectionResponse,
  ReviewProjectionReview,
} from "./generated";

export type {
  ReviewProjection,
  ReviewProjectionJob,
  ReviewProjectionResponse,
  ReviewProjectionReview,
};

export const supportedReviewProjectionSchemaVersions = [1] as const;

export function supportsReviewProjection(
  projection: Pick<ReviewProjection, "schema_version">,
): boolean {
  return supportedReviewProjectionSchemaVersions.includes(
    projection.schema_version as 1,
  );
}
