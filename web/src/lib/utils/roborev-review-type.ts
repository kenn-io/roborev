export function reviewTypeLabel(reviewType: string | undefined): string {
  if (
    reviewType === undefined ||
    reviewType === "" ||
    reviewType === "default" ||
    reviewType === "general" ||
    reviewType === "review"
  ) {
    return "default";
  }
  return reviewType;
}
