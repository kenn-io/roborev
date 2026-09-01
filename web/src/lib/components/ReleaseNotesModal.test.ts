import { fireEvent, render, screen } from "@testing-library/svelte";
import { describe, expect, it } from "vitest";

import ReleaseNotesModal from "./ReleaseNotesModal.svelte";

describe("ReleaseNotesModal", () => {
  it("shows recent releases and switches the selected notes", async () => {
    render(ReleaseNotesModal, {
      open: true,
      loading: false,
      stale: false,
      error: null,
      onclose: () => {},
      onretry: () => {},
      releases: [
        {
          tag_name: "v2.0.0",
          name: "Roborev 2.0",
          body: "Latest notes",
          html_url: "https://example.com/releases/v2.0.0",
          prerelease: false,
          published_at: "2026-09-01T12:00:00Z",
          updated_at: "2026-09-01T12:00:00Z",
        },
        {
          tag_name: "v1.9.0",
          name: "Roborev 1.9",
          body: "Earlier notes",
          html_url: "https://example.com/releases/v1.9.0",
          prerelease: false,
          published_at: "2026-08-20T12:00:00Z",
          updated_at: "2026-08-20T12:00:00Z",
        },
      ],
    });

    expect(await screen.findByText("Latest notes")).toBeInTheDocument();
    await fireEvent.click(screen.getByRole("button", { name: /Roborev 1.9/ }));
    expect(await screen.findByText("Earlier notes")).toBeInTheDocument();
  });
});
