import { describe, expect, it } from "vitest";

import {
  bindingFor,
  effectiveImage,
  isDataComponentKind,
  isKnownKind,
  parseComponents,
  parseOverrides,
  parseSources,
  projectImage,
} from "./components";

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

/**
 * The source binding (ADR-0035), which is what a component page states beside
 * the image: a name, a repository and a ref, and how the binding was decided.
 */
describe("parseSources and bindingFor", () => {
  const LISTED = `kind: Project
metadata:
  name: checkout

spec:
  sources:
    - name: app
      git: https://github.com/acme/checkout
      ref: main
    - name: tools
      git: https://github.com/acme/build-tools
      ref: v2
      connection: acme-github

  components:
    - {name: web, port: 8080, source: app}
    - {name: worker, source: tools}
    - name: orphan
      source: platform
    - name: unbound
`;

  it("reads the declared list, with each entry's own ref and connection", () => {
    expect(parseSources(LISTED)).toEqual([
      {
        name: "app",
        git: "https://github.com/acme/checkout",
        ref: "main",
        connection: "",
        shorthand: false,
      },
      {
        name: "tools",
        git: "https://github.com/acme/build-tools",
        ref: "v2",
        connection: "acme-github",
        shorthand: false,
      },
    ]);
  });

  it("reads the singular shorthand as the one-entry list it declares", () => {
    const shorthand = `spec:
  source:
    git: https://github.com/acme/checkout
    ref: main
  components:
    - name: web
      port: 8080
`;
    expect(parseSources(shorthand)).toEqual([
      {
        name: "default",
        git: "https://github.com/acme/checkout",
        ref: "main",
        connection: "",
        shorthand: true,
      },
    ]);
    // And a component that names none binds to it, because a list of one is
    // not a decision anybody took.
    const web = parseComponents(shorthand)[0];
    expect(web).toBeDefined();
    if (web) {
      const binding = bindingFor(web, parseSources(shorthand));
      expect(binding.basis).toBe("sole");
      expect(binding.source?.git).toBe("https://github.com/acme/checkout");
    }
  });

  it("binds a component to the source it names", () => {
    const byName = new Map(parseComponents(LISTED).map((c) => [c.name, c]));
    const sources = parseSources(LISTED);
    const worker = byName.get("worker");
    expect(worker).toBeDefined();
    if (worker) {
      const binding = bindingFor(worker, sources);
      expect(binding.basis).toBe("named");
      expect(binding.source?.ref).toBe("v2");
    }
  });

  it("reports a name this document does not declare as exactly that", () => {
    // It may be a perfectly good instance-wide GitSource, which no document
    // can see — so the reader states what it knows and refuses to call it an
    // error.
    const orphan = parseComponents(LISTED).find((c) => c.name === "orphan");
    expect(orphan).toBeDefined();
    if (orphan) {
      const binding = bindingFor(orphan, parseSources(LISTED));
      expect(binding.basis).toBe("undeclared");
      expect(binding.requested).toBe("platform");
      expect(binding.source).toBeUndefined();
    }
  });

  it("refuses to pick between several sources when none is the default", () => {
    const unbound = parseComponents(LISTED).find((c) => c.name === "unbound");
    expect(unbound).toBeDefined();
    if (unbound) {
      expect(bindingFor(unbound, parseSources(LISTED)).basis).toBe("ambiguous");
      // With a `default` entry there is a decision to read.
      const withDefault = parseSources(
        "spec:\n  sources:\n    - name: default\n      git: https://example.test/a\n    - name: other\n      git: https://example.test/b\n",
      );
      expect(bindingFor(unbound, withDefault).basis).toBe("default");
    }
  });

  it("says a project with no source at all has none", () => {
    const imageOnly = parseComponents(
      "spec:\n  image: ghcr.io/acme/checkout:1.4.2\n  components:\n    - name: web\n      port: 8080\n",
    )[0];
    expect(imageOnly).toBeDefined();
    if (imageOnly) {
      expect(bindingFor(imageOnly, []).basis).toBe("none");
    }
  });
});

/** Rule P3, and the scope that decided it — the half a merge usually hides. */
describe("effectiveImage", () => {
  const PROJECT_DOC = `spec:
  image: ghcr.io/acme/checkout:1.4.2
  components:
    - name: web
      port: 8080
    - name: worker
      image: ghcr.io/acme/checkout-worker:1.4.2
`;
  const ENVIRONMENT_DOC = `spec:
  components:
    - name: web
      image: ghcr.io/acme/checkout:1.4.3
`;

  it("gives the innermost scope that names one, and says which it was", () => {
    const byName = new Map(parseComponents(PROJECT_DOC).map((c) => [c.name, c]));
    const overrides = parseOverrides(ENVIRONMENT_DOC);
    const web = byName.get("web");
    const worker = byName.get("worker");
    expect(
      web && effectiveImage(web, overrides.get("web"), projectImage(PROJECT_DOC)),
    ).toEqual({
      image: "ghcr.io/acme/checkout:1.4.3",
      scope: "environment",
    });
    expect(
      worker &&
        effectiveImage(worker, overrides.get("worker"), projectImage(PROJECT_DOC)),
    ).toEqual({
      image: "ghcr.io/acme/checkout-worker:1.4.2",
      scope: "component",
    });
    expect(
      web && effectiveImage(web, undefined, projectImage(PROJECT_DOC)),
    ).toEqual({ image: "ghcr.io/acme/checkout:1.4.2", scope: "project" });
  });

  it("treats no image anywhere as a scope of its own, not as a blank", () => {
    // A component with no image builds from its source; that is not an error
    // and must not read as a missing value.
    const nothing = parseComponents(
      "spec:\n  components:\n    - name: web\n      port: 8080\n",
    )[0];
    expect(nothing && effectiveImage(nothing, undefined, "")).toEqual({
      image: "",
      scope: "none",
    });
  });
});
