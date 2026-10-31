import { cleanup, render, screen } from "@testing-library/svelte";
import { afterEach, describe, expect, it } from "vitest";
import SessionProvenance from "./SessionProvenance.svelte";

afterEach(cleanup);

describe("SessionProvenance", () => {
  it("shows recorded evidence and keeps unsafe URLs inert", () => {
    render(SessionProvenance, {
      commits: [
        {
          commit_sha: "0123456789abcdef0123456789abcdef01234567",
          coverage: "why-selected",
          sessions: [
            {
              agent: "pi",
              session_id: "pi:writer",
              source: "transcript",
              commit_sha: "0123456",
              resolved: false,
              ambiguous: true,
              availability: "gone",
              web_url: "javascript:alert(1)",
            },
          ],
        },
      ],
    });
    expect(screen.getByText("pi:writer")).toBeInTheDocument();
    expect(screen.getByText(/ambiguous/i)).toBeInTheDocument();
    expect(screen.getByText(/unavailable/i)).toBeInTheDocument();
    expect(screen.getByText(/up to three sessions/i)).toBeInTheDocument();
    expect(screen.queryByRole("link")).toBeNull();
  });

  it("renders the stored prompt as text and links HTTP sessions", () => {
    render(SessionProvenance, {
      commits: [
        {
          commit_sha: "0123456789abcdef0123456789abcdef01234567",
          coverage: "http",
          sessions: [
            {
              agent: "pi",
              session_id: "pi:writer",
              source: "trailer",
              commit_sha: "0123456789abcdef0123456789abcdef01234567",
              resolved: true,
              availability: "available",
              web_url: "https://archive.example/sessions/writer",
              turn: {
                ordinal: 0,
                why: "before",
                prompt: "<script>untrusted</script>",
              },
            },
          ],
        },
      ],
    });
    expect(screen.getByText("<script>untrusted</script>")).toBeInTheDocument();
    expect(screen.getByRole("link")).toHaveAttribute(
      "href",
      "https://archive.example/sessions/writer",
    );
  });

  it("emits nothing without a capture", () => {
    const { container } = render(SessionProvenance, { commits: [] });
    expect(container.textContent).toBe("");
  });
});
