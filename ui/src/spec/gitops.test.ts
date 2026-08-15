import { beforeEach, describe, expect, it } from "vitest";
import { create } from "@bufbuild/protobuf";

import { GitOpsOwnershipSchema } from "../gen/kelson/v1alpha1/spec_pb";

import {
  autoDeployDocuments,
  bundle,
  declaresAutoDeploy,
  exportedDocuments,
  isGitOpsManaged,
  joinNames,
  kustomizationName,
  owningKustomizations,
  rememberPaths,
  rememberTarget,
  rememberedPaths,
  rememberedTarget,
  suggestedPath,
} from "./gitops";

function owner(document: string, kustomization: string, namespace = "flux-system") {
  return create(GitOpsOwnershipSchema, { document, kustomization, namespace });
}

const TEXT = {
  project: "kind: Project\nmetadata:\n  name: hello\n",
  environments: {
    production: "kind: Environment\nspec:\n  project: hello\n",
    development: "kind: Environment\nspec:\n  project: hello\n  autoDeploy: true\n",
  },
};

describe("ownership", () => {
  it("is any document, not all of them", () => {
    // Save writes the whole set, so one managed document is enough for the
    // write to be partly undone. A screen that only warned when everything was
    // managed would be silent in the mixed case, which is the common one.
    expect(isGitOpsManaged([owner("production", "apps")])).toBe(true);
    expect(isGitOpsManaged([])).toBe(false);
    expect(isGitOpsManaged(undefined)).toBe(false);
  });

  it("names a Kustomization the way flux does, and tolerates half a label set", () => {
    expect(kustomizationName(owner("project", "apps"))).toBe("flux-system/apps");
    expect(kustomizationName(owner("project", "apps", ""))).toBe("apps");
  });

  it("reports each Kustomization once", () => {
    const names = owningKustomizations([
      owner("project", "apps"),
      owner("production", "apps"),
      owner("staging", "other", "flux-system"),
    ]);
    expect(names).toEqual(["flux-system/apps", "flux-system/other"]);
  });
});

describe("joinNames", () => {
  it("reads as a sentence", () => {
    expect(joinNames([])).toBe("");
    expect(joinNames(["a"])).toBe("a");
    expect(joinNames(["a", "b"])).toBe("a and b");
    expect(joinNames(["a", "b", "c"])).toBe("a, b and c");
  });
});

describe("autoDeploy detection", () => {
  it("finds the environments ADR-0036 decision 5 is about", () => {
    expect(autoDeployDocuments(TEXT)).toEqual(["development"]);
  });

  it("matches a key and not a mention", () => {
    expect(declaresAutoDeploy("spec:\n  autoDeploy: true\n")).toBe(true);
    expect(declaresAutoDeploy("spec:\n  components:\n    - {name: web}\n")).toBe(false);
    expect(declaresAutoDeploy("# autoDeploy: removed last week\n")).toBe(false);
    // The documented limits of a line match, both directions. A key inside a
    // block scalar reads as a declaration, and a document written in flow style
    // on one line does not. Both are acceptable for a warning that names a real
    // incompatibility and tells the reader to look; neither would be for
    // anything that refuses.
    expect(declaresAutoDeploy("spec: {autoDeploy: true}\n")).toBe(false);
    expect(declaresAutoDeploy("notes: |\n  autoDeploy: mentioned in prose\n")).toBe(true);
  });
});

describe("documents", () => {
  it("lists the project first and attaches each owner", () => {
    const docs = exportedDocuments(TEXT, [owner("project", "apps"), owner("production", "apps")]);
    expect(docs.map((d) => d.name)).toEqual(["project", "development", "production"]);
    expect(docs[0]?.owner?.kustomization).toBe("apps");
    // `development` is kelson's own, so it carries no owner and the proposal
    // form leaves it unchecked.
    expect(docs[1]?.owner).toBeUndefined();
    expect(docs[2]?.owner?.kustomization).toBe("apps");
  });

  it("bundles into one multi-document file", () => {
    const text = bundle(exportedDocuments(TEXT, []));
    expect(text.split("---").length - 1).toBe(3);
    expect(text.endsWith("\n")).toBe(true);
  });
});

describe("suggestedPath", () => {
  it("is a starting point rather than an answer", () => {
    expect(suggestedPath("hello", "project")).toBe("hello.yaml");
    expect(suggestedPath("hello", "production")).toBe("hello-production.yaml");
  });
});

describe("remembering", () => {
  beforeEach(() => {
    window.localStorage.clear();
  });

  it("keeps paths and targets per project", () => {
    rememberPaths("hello", { project: "clusters/prod/hello.yaml" });
    rememberTarget("hello", {
      connection: "acme-github",
      repository: "acme/gitops",
      baseBranch: "main",
    });

    expect(rememberedPaths("hello")).toEqual({ project: "clusters/prod/hello.yaml" });
    expect(rememberedTarget("hello").repository).toBe("acme/gitops");
    // Another project starts empty: two projects rarely share a path.
    expect(rememberedPaths("other")).toEqual({});
    expect(rememberedTarget("other").repository).toBe("");
  });

  it("falls back to empty rather than throwing on a value it cannot read", () => {
    window.localStorage.setItem("kelson.gitops.paths.hello", "not json");
    window.localStorage.setItem("kelson.gitops.target.hello", "[]");
    expect(rememberedPaths("hello")).toEqual({});
    expect(rememberedTarget("hello").connection).toBe("");
  });
});
