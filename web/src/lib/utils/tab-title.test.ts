import { describe, expect, test } from "vitest";
import type { ReviewJob } from "../api/generated/models";
import { tabTitle } from "./tab-title";

const job: ReviewJob = {
  id: 9,
  agent: "test",
  agentic: false,
  enqueued_at: "",
  git_ref: "abc",
  job_type: "review",
  prompt_prebuilt: false,
  repo_id: 1,
  retry_count: 0,
  status: "done",
  commit_subject: "Fix login race",
  repo_name: "my-repo",
};

describe("tabTitle", () => {
  test("names workspaces and reviews", () => {
    expect(tabTitle({ page: "reviews" }, null)).toBe("Reviews · Roborev");
    expect(tabTitle({ page: "analytics" }, null)).toBe("Analytics · Roborev");
    const route = { page: "reviews", jobId: 9 } as const;
    expect(tabTitle(route, job)).toBe(
      "Fix login race (#9) · my-repo · Roborev",
    );
    expect(tabTitle(route, { ...job, commit_subject: undefined })).toBe(
      "Review (#9) · my-repo · Roborev",
    );
    expect(tabTitle(route, { ...job, repo_name: undefined })).toBe(
      "Fix login race (#9) · Roborev",
    );
    expect(tabTitle(route, null)).toBe("Review (#9) · Roborev");
    expect(tabTitle(route, { ...job, id: 8 })).toBe("Review (#9) · Roborev");
  });
});
