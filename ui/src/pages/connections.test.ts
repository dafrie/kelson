import { describe, expect, it } from "vitest";
import { create } from "@bufbuild/protobuf";

import {
  GitAuthKind,
  GitConnectionSchema,
  GitOwnerKind,
  GitOwnerSchema,
} from "../gen/kelson/v1alpha1/gitconnection_pb";
import {
  appIdentity,
  authLabel,
  connectionHealth,
  DEFAULT_GITHUB_HOST,
  EMPTY_FORM,
  formProblems,
  hostForProvider,
  NOT_OBSERVED,
  ownerIsInstance,
  ownerLabel,
  repositoriesLabel,
  type ConnectionForm,
} from "./connections";

/**
 * The readings the screen makes of a connection, tested where they are made
 * rather than through the DOM: each one is a rule from ADR-0033 or from
 * gitconnection.proto, and each has a case that would be a lie if it were read
 * the obvious way instead.
 */

function connection(fields: Parameters<typeof create<typeof GitConnectionSchema>>[1]) {
  return create(GitConnectionSchema, fields);
}

describe("connectionHealth", () => {
  it("is ready when both conditions hold", () => {
    const health = connectionHealth(
      connection({ ready: true, reachable: true, message: "authenticated as acme" }),
    );
    expect(health.status).toBe("synced");
    expect(health.label).toBe("ready");
    expect(health.detail).toBe("authenticated as acme");
  });

  it("separates a well-formed connection the forge would not answer", () => {
    const health = connectionHealth(
      connection({
        ready: true,
        reachable: false,
        message: "the installation was suspended",
      }),
    );
    expect(health.status).toBe("degraded");
    expect(health.label).toBe("unreachable");
    expect(health.detail).toBe("the installation was suspended");
  });

  it("reports a broken connection as failed and keeps the reason verbatim", () => {
    const health = connectionHealth(
      connection({
        ready: false,
        reachable: false,
        message: 'Secret "acme-git-token" does not exist in kelson-system',
      }),
    );
    expect(health.status).toBe("failed");
    expect(health.detail).toBe(
      'Secret "acme-git-token" does not exist in kelson-system',
    );
  });

  /**
   * The case the proto is explicit about: both booleans are false before the
   * first probe as well as after a failed one. With no message there is nothing
   * to tell them apart, so the screen must not pick the alarming one.
   */
  it("calls an unobserved connection unknown rather than failed", () => {
    const health = connectionHealth(
      connection({ ready: false, reachable: false, message: "" }),
    );
    expect(health.status).toBe("unknown");
    expect(health.label).toBe("not observed");
    expect(health.detail).toBe(NOT_OBSERVED);
  });
});

describe("authLabel", () => {
  it("names the two kinds ADR-0033 decision 2 defines", () => {
    expect(authLabel(GitAuthKind.GITHUB_APP)).toBe("GitHub App");
    expect(authLabel(GitAuthKind.TOKEN)).toBe("token");
  });

  it("refuses to draw a kind it does not know as one it does", () => {
    expect(authLabel(GitAuthKind.UNSPECIFIED)).toBe("unknown auth");
    expect(authLabel(7 as GitAuthKind)).toBe("unknown auth");
  });
});

describe("appIdentity", () => {
  it("is absent for a token connection, which has no app", () => {
    expect(
      appIdentity(connection({ authKind: GitAuthKind.TOKEN, appId: 0n })),
    ).toBeUndefined();
  });

  it("reads a zero installation as the pre-install state, not as missing data", () => {
    expect(
      appIdentity(
        connection({
          authKind: GitAuthKind.GITHUB_APP,
          appId: 12345n,
          installationId: 0n,
        }),
      ),
    ).toBe("app 12345 · no installation yet — the id arrives on the installation webhook");
  });

  it("names the installation once one exists", () => {
    expect(
      appIdentity(
        connection({
          authKind: GitAuthKind.GITHUB_APP,
          appId: 12345n,
          installationId: 678910n,
        }),
      ),
    ).toBe("app 12345 · installation 678910");
  });
});

describe("repositoriesLabel", () => {
  it("reports a count the provider actually gave", () => {
    expect(repositoriesLabel(connection({ reachable: true, repositories: 42 }))).toBe(
      "42 repositories",
    );
    expect(repositoriesLabel(connection({ reachable: true, repositories: 1 }))).toBe(
      "1 repository",
    );
  });

  it("reports zero selected repositories, which is a real answer", () => {
    expect(repositoriesLabel(connection({ reachable: true, repositories: 0 }))).toBe(
      "0 repositories",
    );
  });

  it("reports nothing for a connection no probe reached", () => {
    expect(
      repositoriesLabel(connection({ reachable: false, repositories: 0 })),
    ).toBeUndefined();
  });
});

describe("ownerLabel", () => {
  it("treats an absent or unspecified owner as the instance", () => {
    expect(ownerLabel(undefined)).toBe("instance");
    expect(
      ownerLabel(create(GitOwnerSchema, { kind: GitOwnerKind.UNSPECIFIED })),
    ).toBe("instance");
    expect(ownerLabel(create(GitOwnerSchema, { kind: GitOwnerKind.INSTANCE }))).toBe(
      "instance",
    );
  });

  it("names the reserved principals when one is recorded", () => {
    expect(
      ownerLabel(create(GitOwnerSchema, { kind: GitOwnerKind.USER, name: "alice" })),
    ).toBe("user · alice");
    expect(
      ownerLabel(create(GitOwnerSchema, { kind: GitOwnerKind.TEAM, name: "platform" })),
    ).toBe("team · platform");
  });

  it("does not flatten a kind it cannot read into the instance", () => {
    expect(ownerLabel(create(GitOwnerSchema, { kind: 9 as GitOwnerKind }))).toBe(
      "unknown owner kind",
    );
  });

  it("knows which rows carry a principal to disclaim", () => {
    expect(ownerIsInstance(undefined)).toBe(true);
    expect(
      ownerIsInstance(create(GitOwnerSchema, { kind: GitOwnerKind.INSTANCE })),
    ).toBe(true);
    expect(
      ownerIsInstance(create(GitOwnerSchema, { kind: GitOwnerKind.USER, name: "alice" })),
    ).toBe(false);
  });
});

describe("hostForProvider", () => {
  it("fills github's default in and clears it for a provider that has none", () => {
    expect(hostForProvider("github", "")).toBe(DEFAULT_GITHUB_HOST);
    expect(hostForProvider("generic", DEFAULT_GITHUB_HOST)).toBe("");
  });

  it("never overwrites a host somebody typed", () => {
    expect(hostForProvider("github", "https://ghe.acme.internal")).toBe(
      "https://ghe.acme.internal",
    );
    expect(hostForProvider("generic", "https://git.acme.internal")).toBe(
      "https://git.acme.internal",
    );
  });
});

describe("formProblems", () => {
  const good: ConnectionForm = {
    name: "acme-github",
    provider: "github",
    host: DEFAULT_GITHUB_HOST,
    secretRef: "acme-git-token",
  };

  it("passes the request CreateConnection would accept", () => {
    expect(formProblems(good)).toEqual([]);
  });

  it("holds both names to a DNS-1123 label, which is what they become", () => {
    expect(formProblems({ ...good, name: "Acme GitHub" })[0]?.field).toBe("name");
    expect(formProblems({ ...good, secretRef: "Acme_Token" })[0]?.field).toBe(
      "secretRef",
    );
  });

  it("requires a host, because a default would silently claim github.com", () => {
    const problem = formProblems({ ...good, host: "  " })[0];
    expect(problem?.field).toBe("host");
    expect(problem?.message).toContain("silently claim github.com");
  });

  it("refuses a bare hostname, which is what a reader types first", () => {
    const problem = formProblems({ ...good, host: "github.com" })[0];
    expect(problem?.field).toBe("host");
    expect(problem?.message).toContain("is not a forge base URL");
  });

  it("starts empty except for the one default worth writing down", () => {
    expect(EMPTY_FORM.provider).toBe("github");
    expect(EMPTY_FORM.host).toBe(DEFAULT_GITHUB_HOST);
    expect(formProblems(EMPTY_FORM).map((p) => p.field)).toEqual([
      "name",
      "secretRef",
    ]);
  });
});
