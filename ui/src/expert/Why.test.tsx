import { beforeEach, describe, expect, it } from "vitest";
import { act, fireEvent, render, screen } from "@testing-library/react";

import { Detail, KubeFact } from "./Detail";
import { DetailToggle } from "./DetailToggle";
import { setDetail } from "./preference";
import { Evidence, Why } from "./Why";

/**
 * The three primitives expert mode is built from: the gate, the inline fact and
 * the per-statement caret.
 */

beforeEach(() => setDetail(false));

describe("Why", () => {
  it("is absent in normal mode — not disabled, not smaller", () => {
    render(
      <Why statement="live">
        <Evidence rows={[{ name: "answer", value: "live" }]} />
      </Why>,
    );
    expect(document.querySelector(".k-why")).toBeNull();
    expect(screen.queryByText("answer")).toBeNull();
  });

  it("appears collapsed in expert mode, and names the statement it belongs to", () => {
    setDetail(true);
    render(
      <Why statement="older than the spec">
        <Evidence rows={[{ name: "stale", value: "true" }]} />
      </Why>,
    );

    const details = document.querySelector(".k-why");
    expect(details).toBeTruthy();
    // Collapsed by default even here: expert mode says the evidence should be
    // available, not that fifteen panels should be open at once.
    expect((details as HTMLDetailsElement).open).toBe(false);
    // The caret carries the claim in its accessible name, because the third
    // caret in a row is otherwise indistinguishable from the first.
    expect(
      screen.getByLabelText("Evidence for “older than the spec”"),
    ).toBeTruthy();
  });
});

describe("Evidence", () => {
  it("drops a field the wire did not answer rather than rendering it blank", () => {
    setDetail(true);
    render(
      <Why statement="live">
        <Evidence
          rows={[
            { name: "answer", value: "live" },
            { name: "cause", value: "" },
          ]}
        />
      </Why>,
    );

    expect(screen.getByText("answer")).toBeTruthy();
    // An absent field is the wire declining to answer; a key with nothing
    // beside it reads as an empty string that was actually sent.
    expect(screen.queryByText("cause")).toBeNull();
  });

  it("keeps the note when every field dropped, because it is what explains the gap", () => {
    setDetail(true);
    render(
      <Why statement="unknown">
        <Evidence
          rows={[{ name: "answer", value: "" }]}
          note="The status call did not answer."
        />
      </Why>,
    );

    expect(screen.getByText("The status call did not answer.")).toBeTruthy();
  });
});

describe("Detail and KubeFact", () => {
  it("show nothing in normal mode and the fact in expert mode", () => {
    const { rerender } = render(
      <Detail>
        <KubeFact name="generation" value="45" />
      </Detail>,
    );
    expect(screen.queryByText("45")).toBeNull();

    act(() => setDetail(true));
    rerender(
      <Detail>
        <KubeFact name="generation" value="45" />
      </Detail>,
    );
    expect(screen.getByText("45")).toBeTruthy();
    expect(screen.getByText(/generation/)).toBeTruthy();
  });

  it("renders nothing for a value the wire did not send", () => {
    setDetail(true);
    render(
      <Detail>
        <KubeFact name="generation" value="" />
      </Detail>,
    );
    expect(screen.queryByText(/generation/)).toBeNull();
  });
});

describe("DetailToggle", () => {
  it("reads and writes the preference, and says which state it is in", () => {
    render(<DetailToggle />);

    const button = screen.getByRole("button", {
      name: "Kubernetes detail: off. Show it.",
    });
    expect(button.getAttribute("aria-pressed")).toBe("false");

    fireEvent.click(button);
    expect(
      screen
        .getByRole("button", { name: "Kubernetes detail: on. Hide it." })
        .getAttribute("aria-pressed"),
    ).toBe("true");

    fireEvent.click(
      screen.getByRole("button", { name: "Kubernetes detail: on. Hide it." }),
    );
    expect(
      screen
        .getByRole("button", { name: "Kubernetes detail: off. Show it." })
        .getAttribute("aria-pressed"),
    ).toBe("false");
  });
});
