export function reviewTypeLabel(
  reviewType: string | undefined,
  panelRole?: string,
): string {
  if (panelRole === "synthesis") return "panel";
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
