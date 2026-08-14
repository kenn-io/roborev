import { render, screen } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";

import {
  ReviewProjectionView,
  type ReviewProjection,
} from "@kenn-io/roborev-ui";

const projection: ReviewProjection = {
  schema_version: 1,
  job: {
    id: 42,
    project: "project-a",
    git_ref: "abcdef123456",
    branch: "main",
    commit_subject: "Keep review rendering reusable",
    agent: "codex",
    model: "model-a",
    status: "done",
    verdict: "P",
    enqueued_at: "2026-08-13T12:00:00Z",
    panel_name: "standard",
  },
  review: {
    id: 9,
    output: "## Result\n\n<script>alert('no')</script>Looks good.",
    created_at: "2026-08-13T12:01:00Z",
    closed: false,
  },
  responses: [
    {
      id: 3,
      responder: "reviewer-a",
      response: "Confirmed",
      created_at: "2026-08-13T12:02:00Z",
    },
  ],
  panel_members: [
    {
      id: 41,
      project: "project-a",
      git_ref: "abcdef123456",
      agent: "claude-code",
      status: "done",
      verdict: "F",
      enqueued_at: "2026-08-13T12:00:00Z",
      panel_role: "member",
      panel_member_name: "correctness",
    },
  ],
};

describe("@kenn-io/roborev-ui", () => {
  it("renders the complete read-only review projection", async () => {
    const view = render(ReviewProjectionView, { projection });

    expect(screen.getByText("project-a")).toBeInTheDocument();
    expect(screen.getByText("Pass")).toBeInTheDocument();
    expect(screen.getByText("correctness")).toBeInTheDocument();
    expect(screen.getByText("Confirmed")).toBeInTheDocument();
    await screen.findByRole("heading", { name: "Result" });
    expect(view.container.innerHTML).not.toContain("<script");
    expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
  });

  it("rejects an unsupported projection schema", () => {
    render(ReviewProjectionView, {
      projection: { ...projection, schema_version: 2 } as ReviewProjection,
    });

    expect(
      screen.getByRole("alert", { name: "Unsupported review data" }),
    ).toHaveTextContent("Update Roborev");
    expect(screen.queryByText("project-a")).not.toBeInTheDocument();
  });
});
