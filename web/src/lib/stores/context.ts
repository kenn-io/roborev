import { getContext, setContext } from "svelte";

import type { ReviewStores } from "./composition.svelte";

export const REVIEW_STORES_KEY = Symbol("roborev-review-stores");

export function provideReviewStores(stores: ReviewStores): ReviewStores {
  return setContext(REVIEW_STORES_KEY, stores);
}

export function getReviewStores(): ReviewStores {
  return getContext(REVIEW_STORES_KEY);
}
