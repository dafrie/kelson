import { describe, expect, it } from "vitest";

import { isDataComponentKind, isKnownKind, parseComponents } from "./components";

/**
 * The kind table of docs/model.md, read out of a document nobody promised was
 * written by this UI. That is the whole point of this reader as against
 * src/spec/edit.ts's: a hand-written Project is still a Project whose
 * components a reader is entitled to see listed.
 */
const PROJECT = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: checkout

spec:
  image: ghcr.io/acme/checkout:1.4.2

  components:
    - name: web
      port: 8080
      health: /healthz
      env:
        ROLE: web

    - name: worker
      image: ghcr.io/acme/checkout-worker:1.4.2

    - name: nightly
      schedule: "0 3 * * *"

    - name: db
      kind: postgres
      preset: ha-small

    - name: cache
      kind: valkey
`;

describe("parseComponents", () => {
  it("derives the kind from the shape, and reads it when the document states one", () => {
    expect(parseComponents(PROJECT).map((c) => [c.name, c.kind])).toEqual([
      ["web", "service"],
      ["worker", "worker"],
      ["nightly", "cron"],
      ["db", "postgres"],
      ["cache", "valkey"],
    ]);
  });

  it("keeps what each row has to say about itself", () => {
    const byName = new Map(parseComponents(PROJECT).map((c) => [c.name, c]));
    expect(byName.get("web")?.port).toBe("8080");
    expect(byName.get("nightly")?.schedule).toBe("0 3 * * *");
    expect(byName.get("db")?.preset).toBe("ha-small");
    // Rule P3: an empty image is the project's image, not a missing one.
    expect(byName.get("web")?.image).toBe("");
    expect(byName.get("worker")?.image).toBe("ghcr.io/acme/checkout-worker:1.4.2");
    // Whether the kind was written matters: `worker` is derived, `valkey` said.
    expect(byName.get("worker")?.written).toBe(false);
    expect(byName.get("cache")?.written).toBe(true);
  });

  it("reads a component out of a hand-written document the edit form refuses", () => {
    // Comments, another key order, four-space indentation: none of it stops a
    // list of names and kinds from being readable, and the form's byte guard is
    // a separate question from this one.
    const handWritten = `kind: Project
metadata:
    name: checkout
spec:
    components:
        # the one that serves traffic
        - port: 8080
          name: web
        - name: db
          kind: postgres
`;
    expect(parseComponents(handWritten).map((c) => [c.name, c.kind])).toEqual([
      ["web", "service"],
      ["db", "postgres"],
    ]);
  });

  it("says nothing rather than something wrong about a document it cannot follow", () => {
    expect(parseComponents("")).toEqual([]);
    expect(parseComponents("kind: Project\nmetadata:\n  name: checkout\n")).toEqual([]);
  });

  it("shows a kind this build has never heard of as itself", () => {
    // Guessing at it would be worse than reporting it: the server is the
    // authority on whether an unknown kind renders.
    const future = parseComponents(
      "spec:\n  components:\n    - name: thing\n      kind: quantum\n",
    );
    expect(future[0]?.kind).toBe("quantum");
    expect(isKnownKind("quantum")).toBe(false);
    expect(isKnownKind("helm")).toBe(true);
  });

  it("knows which kinds belong to the data-services section", () => {
    expect(isDataComponentKind("postgres")).toBe(true);
    expect(isDataComponentKind("valkey")).toBe(true);
    expect(isDataComponentKind("worker")).toBe(false);
  });
});
