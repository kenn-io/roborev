import { describe, expect, it } from "vitest";
import type { components } from "../api/generated";
import {
  canCancelJob,
  canRerunJob,
  isPanelParent,
  isTerminalStatus,
  panelCostUsd,
  panelElapsedStart,
  panelReviewHeader,
  panelStatusLabel,
} from "./roborev-panel";

type ReviewJob = components["schemas"]["ReviewJob"];
type PanelSummary = components["schemas"]["PanelSummary"];

function makeJob(overrides: Partial<ReviewJob> = {}): ReviewJob {
  return {
    id: 1,
    agent: "codex",
    agentic: false,
    enqueued_at: "2026-04-11T11:00:00Z",
    git_ref: "deadbeef1",
    job_type: "review",
    prompt_prebuilt: false,
    repo_id: 1,
    retry_count: 0,
    status: "done",
    ...overrides,
  };
}

function makeSummary(overrides: Partial<PanelSummary> = {}): PanelSummary {
  return {
    panel_run_uuid: "run-1",
    members_total: 3,
    members_terminal: 3,
    members_succeeded: 3,
    members_failed: 0,
    members_canceled: 0,
    members_skipped: 0,
    ...overrides,
  };
}

function makeParent(
  summary: Partial<PanelSummary> = {},
  overrides: Partial<ReviewJob> = {},
): ReviewJob {
  return makeJob({
    job_type: "synthesis",
    panel_role: "synthesis",
    panel_run_uuid: "run-1",
    panel_summary: makeSummary(summary),
    ...overrides,
  });
}

function makeMember(
  index: number,
  overrides: Partial<ReviewJob> = {},
): ReviewJob {
  return makeJob({
    id: 100 + index,
    panel_role: "member",
    panel_run_uuid: "run-1",
    panel_member_index: index,
    panel_member_name:
      ["default", "security", "design"][index] ?? `member-${index}`,
    ...overrides,
  });
}

describe("isPanelParent", () => {
  it("is true only for synthesis rows with a non-empty summary", () => {
    expect(isPanelParent(makeParent())).toBe(true);
    expect(isPanelParent(makeJob())).toBe(false);
    expect(isPanelParent(makeParent({ members_total: 0 }))).toBe(false);
    expect(isPanelParent(makeMember(0))).toBe(false);
  });
});

describe("isTerminalStatus", () => {
  it("matches roborev terminal statuses", () => {
    for (const s of [
      "done",
      "applied",
      "rebased",
      "failed",
      "canceled",
      "skipped",
    ]) {
      expect(isTerminalStatus(s)).toBe(true);
    }
    expect(isTerminalStatus("running")).toBe(false);
    expect(isTerminalStatus("queued")).toBe(false);
  });
});

describe("canRerunJob", () => {
  it("matches daemon rerun states and excludes panel members", () => {
    for (const status of ["done", "failed", "canceled", "skipped"] as const) {
      expect(canRerunJob(makeJob({ status }))).toBe(true);
    }
    expect(canRerunJob(makeJob({ status: "running" }))).toBe(false);
    expect(canRerunJob(makeJob({ status: "applied" }))).toBe(false);
    expect(canRerunJob(makeMember(0))).toBe(false);
  });

  it("does not offer reruns for skipped auto-design jobs", () => {
    expect(
      canRerunJob(makeJob({ status: "skipped", source: "auto_design" })),
    ).toBe(false);
    expect(canRerunJob(makeJob({ status: "skipped" }))).toBe(true);
  });
});

describe("canCancelJob", () => {
  const remoteCapabilities = {
    cancelAnyJob: false,
    cancelReviewJob: true,
    rerunJob: false,
  };

  it("limits remote cancellation to standalone non-CI reviews", () => {
    expect(
      canCancelJob(makeJob({ status: "queued" }), remoteCapabilities),
    ).toBe(true);
    expect(
      canCancelJob(
        makeJob({ status: "queued", source: "ci" }),
        remoteCapabilities,
      ),
    ).toBe(false);
    expect(
      canCancelJob(makeMember(0, { status: "queued" }), remoteCapabilities),
    ).toBe(false);
    expect(
      canCancelJob(makeParent({}, { status: "queued" }), remoteCapabilities),
    ).toBe(false);
  });
});

describe("panelStatusLabel", () => {
  it("shows progress while the parent is not terminal", () => {
    const job = makeParent({ members_terminal: 2 }, { status: "running" });
    expect(panelStatusLabel(job)).toBe("synthesizing… 2/3 reviewers done");
  });

  it("shows the outcome split when terminal", () => {
    const job = makeParent({
      members_succeeded: 2,
      members_failed: 1,
      members_skipped: 1,
      members_total: 4,
      members_terminal: 4,
    });
    expect(panelStatusLabel(job)).toBe("2 ok · 1 failed · 1 skipped");
  });

  it("returns null for non-panel jobs", () => {
    expect(panelStatusLabel(makeJob())).toBeNull();
  });
});

describe("panelCostUsd", () => {
  const priced = (usd: number) =>
    JSON.stringify({ has_cost: true, cost_usd: usd });

  it("returns own cost for non-panel jobs", () => {
    expect(
      panelCostUsd(makeJob({ token_usage: priced(0.1) }), undefined),
    ).toBeCloseTo(0.1);
    expect(panelCostUsd(makeJob(), undefined)).toBeNull();
  });

  it("sums own plus fetched member costs, skipping unpriced members", () => {
    const parent = makeParent({}, { token_usage: priced(0.05) });
    const members = [
      makeMember(0, { token_usage: priced(0.2) }),
      makeMember(1),
    ];
    expect(panelCostUsd(parent, members)).toBeCloseTo(0.25);
  });

  it("falls back to the summary aggregate when members are not fetched", () => {
    const parent = makeParent(
      {
        members_with_cost: 2,
        members_cost_usd: 0.3,
        members_cost_complete: false,
      },
      { token_usage: priced(0.05) },
    );
    expect(panelCostUsd(parent, undefined)).toBeCloseTo(0.35);
  });

  it("returns null when nothing is priced", () => {
    expect(panelCostUsd(makeParent(), undefined)).toBeNull();
    expect(panelCostUsd(makeParent(), [makeMember(0)])).toBeNull();
  });
});

describe("panelElapsedStart", () => {
  it("prefers the earliest of summary first start, member starts, and own start", () => {
    const parent = makeParent(
      { first_started_at: "2026-04-11T11:10:00Z" },
      { started_at: "2026-04-11T11:30:00Z" },
    );
    expect(panelElapsedStart(parent, undefined)).toBe("2026-04-11T11:10:00Z");
    const members = [makeMember(0, { started_at: "2026-04-11T11:05:00Z" })];
    expect(panelElapsedStart(parent, members)).toBe("2026-04-11T11:05:00Z");
  });

  it("returns own start for non-panel jobs", () => {
    expect(
      panelElapsedStart(
        makeJob({ started_at: "2026-04-11T11:00:00Z" }),
        undefined,
      ),
    ).toBe("2026-04-11T11:00:00Z");
    expect(panelElapsedStart(makeJob(), undefined)).toBeUndefined();
  });
});

describe("panelReviewHeader", () => {
  it("lists member names and verdicts when members are loaded", () => {
    const members = [
      makeMember(0, { verdict: "P" }),
      makeMember(1, { verdict: "F" }),
      makeMember(2),
    ];
    expect(panelReviewHeader(makeParent(), members)).toBe(
      "3 reviewers: default P, security F, design ·",
    );
  });

  it("falls back to the summary split without members", () => {
    const parent = makeParent({ members_succeeded: 2, members_failed: 1 });
    expect(panelReviewHeader(parent, undefined)).toBe(
      "3 reviewers: 2 ok · 1 failed",
    );
  });

  it("returns null for non-panel jobs", () => {
    expect(panelReviewHeader(makeJob(), undefined)).toBeNull();
  });
});
