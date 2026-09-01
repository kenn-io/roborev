import { cleanup, render, screen } from "@testing-library/svelte";
import { afterEach, describe, expect, it } from "vitest";

import JobRow from "./JobRow.svelte";
import type { components } from "../../api/generated";

type ReviewJob = components["schemas"]["ReviewJob"];

function makeJob(tokenUsage?: string): ReviewJob {
  return {
    id: 42,
    agent: "codex",
    agentic: false,
    enqueued_at: "2026-04-10T12:00:00Z",
    finished_at: "2026-04-10T12:08:00Z",
    git_ref: "abcdef123456",
    job_type: "review",
    prompt_prebuilt: false,
    repo_id: 1,
    retry_count: 0,
    started_at: "2026-04-10T12:03:00Z",
    status: "done",
    token_usage: tokenUsage,
  };
}

describe("JobRow", () => {
  afterEach(() => {
    cleanup();
  });

  it("renders priced roborev token usage as an estimated cost", () => {
    render(JobRow, {
      props: {
        job: makeJob(
          '{"total_output_tokens":28800,"peak_context_tokens":118000,"cost_usd":0.42,"has_cost":true}',
        ),
        selected: false,
        highlighted: false,
        onclick: () => {},
      },
    });

    expect(screen.getByText("~$0.42")).toBeTruthy();
  });

  it("renders review type separately from job type", () => {
    const view = render(JobRow, {
      props: {
        job: { ...makeJob(), review_type: "project-conventions" },
        selected: false,
        highlighted: false,
        onclick: () => {},
      },
    });

    expect(view.container.querySelector(".col-review-type")).toHaveTextContent(
      "project-conventions",
    );
    expect(view.container.querySelector(".col-type")).toHaveTextContent(
      "review",
    );
  });

  it("labels an omitted review type as default", () => {
    const view = render(JobRow, {
      props: {
        job: makeJob(),
        selected: false,
        highlighted: false,
        onclick: () => {},
      },
    });

    expect(view.container.querySelector(".col-review-type")).toHaveTextContent(
      "default",
    );
  });

  describe("panel rows", () => {
    it("renders a chevron, outcome split, and aggregate cost on a panel parent", () => {
      const parent: ReviewJob = {
        ...makeJob(JSON.stringify({ has_cost: true, cost_usd: 0.05 })),
        id: 10,
        job_type: "synthesis",
        panel_role: "synthesis",
        panel_run_uuid: "run-10",
        status: "done",
        panel_summary: {
          panel_run_uuid: "run-10",
          members_total: 3,
          members_terminal: 3,
          members_succeeded: 2,
          members_failed: 1,
          members_canceled: 0,
          members_skipped: 0,
          members_with_cost: 3,
          members_cost_usd: 0.3,
          members_cost_complete: true,
        },
      };

      render(JobRow, {
        props: {
          job: parent,
          selected: false,
          highlighted: false,
          onclick: () => {},
          expandable: true,
          expanded: false,
          ontoggle: () => {},
        },
      });

      expect(screen.getByText("2 ok · 1 failed")).toBeTruthy();
      expect(screen.getByText("~$0.35")).toBeTruthy();
      expect(document.querySelector(".col-review-type")).toHaveTextContent(
        "panel",
      );
      expect(
        screen.getByRole("button", { name: /expand panel/i }),
      ).toBeTruthy();
    });

    it("renders a member row with indented ref content and member name", () => {
      const member: ReviewJob = {
        ...makeJob(),
        id: 11,
        panel_role: "member",
        panel_run_uuid: "run-10",
        panel_member_index: 0,
        panel_member_name: "security",
        verdict: "F",
      };

      render(JobRow, {
        props: {
          job: member,
          selected: false,
          highlighted: false,
          onclick: () => {},
          member: true,
        },
      });

      expect(screen.getByText("security")).toBeTruthy();
      const refCell = screen.getByText("security").closest(".col-ref");
      expect(refCell?.classList.contains("tree-cell")).toBe(true);
      expect(refCell?.querySelector(".tree-spacer")).toBeTruthy();
      expect(screen.queryByText("└")).toBeNull();
      expect(screen.queryByText("├")).toBeNull();
    });
  });
});
