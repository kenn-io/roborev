import { expect, test } from "vitest";
import cases from "./readable-review-cases.json";
import { isReadableReview } from "./roborev-review-type";

test.each(cases)("$job_type", ({ readable, ...job }) => {
  expect(isReadableReview(job)).toBe(readable);
});
