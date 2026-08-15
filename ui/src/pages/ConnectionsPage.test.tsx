import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";

import { ErrorSchema } from "../gen/kelson/v1alpha1/common_pb";
import {
  GitAuthKind,
  GitConnectionService,
  GitOwnerKind,
  type CreateConnectionRequest,
} from "../gen/kelson/v1alpha1/gitconnection_pb";
import { renderAt } from "../test/render";
import { ConnectionsPage } from "./ConnectionsPage";

/**
 * The screen against a stub GitConnectionService — the real generated client,
 * the real serialisation, a server that is not there.
 *
 * The stub answers only what the schema can answer, which is the point: there
 * is no field in gitconnection.proto a credential could travel in, so a test
 * asserting "the token is not shown" would be asserting something the wire
 * cannot express. What is asserted instead is what the page *sends*, what it
 * does with three-state health, and that the two things ADR-0033 asks it to say
 * in words — instance visibility and the projects a delete breaks — are on
 * screen.
 */

const CONNECTIONS = [
  {
    name: "acme-github",
    provider: "github",
    host: "https://github.com",
    authKind: GitAuthKind.GITHUB_APP,
    appId: 12345n,
    installationId: 678910n,
    secretRef: "acme-github-app",
    owner: { kind: GitOwnerKind.INSTANCE },
    account: "acme",
    repositories: 42,
    ready: true,
    reachable: true,
    message: "authenticated as the acme installation",
  },
  {
    name: "internal-gitlab",
    provider: "generic",
    host: "https://git.acme.internal",
    authKind: GitAuthKind.TOKEN,
    secretRef: "internal-git-token",
    owner: { kind: GitOwnerKind.USER, name: "alice" },
    ready: false,
    reachable: false,
    message: "",
  },
];

/** The structured refusal internal/model returns for a malformed host. */
const BAD_HOST = {
  code: "field/invalid-format",
  field: "$.spec.host",
  message: '"ftp://git.acme.internal" is not an http(s) URL',
  remediation:
    "use the forge's base URL over HTTP(S) — the host the API and the clones are reached at",
};

interface Seen {
  creates: CreateConnectionRequest[];
  deletes: string[];
  probes: string[];
}

function transportFor(
  options: { refuseCreate?: boolean; affected?: string[] } = {},
) {
  const seen: Seen = { creates: [], deletes: [], probes: [] };
  const transport = createRouterTransport((router) => {
    router.service(GitConnectionService, {
      listConnections: () => ({ connections: CONNECTIONS }),
      createConnection: (req) => {
        seen.creates.push(req);
        if (options.refuseCreate) {
          throw new ConnectError(
            BAD_HOST.message,
            Code.InvalidArgument,
            undefined,
            [{ desc: ErrorSchema, value: BAD_HOST }],
          );
        }
        return {
          connection: {
            name: req.name,
            provider: req.provider,
            host: req.host,
            secretRef: req.secretRef,
            authKind: GitAuthKind.TOKEN,
            ready: false,
            reachable: false,
            message: "not yet observed: no probe has reached this connection",
          },
        };
      },
      deleteConnection: (req) => {
        seen.deletes.push(req.name);
        return { deleted: true, affectedProjects: options.affected ?? [] };
      },
      testConnection: (req) => {
        seen.probes.push(req.name);
        if (req.name === "acme-github") {
          return {
            reachable: true,
            account: "acme",
            repositories: 42,
            message: "GitHub answered in 84ms",
          };
        }
        return {
          reachable: false,
          message: "401 from https://git.acme.internal: the token was revoked",
        };
      },
    });
  });
  return { transport, seen };
}

function renderPage(options: { refuseCreate?: boolean; affected?: string[] } = {}) {
  const { transport, seen } = transportFor(options);
  renderAt(transport, "/connections", "/connections", <ConnectionsPage />);
  return seen;
}

/**
 * One connection's row. Scoped rather than global, because the create form's
 * provider select carries the same two words the rows' provider chips do — and
 * a query that matched either would pass for the wrong reason.
 */
function row(name: string): HTMLElement {
  const item = screen.getByText(name).closest("li");
  if (item === null) throw new Error(`no row for ${name}`);
  return item;
}

describe("ConnectionsPage: the list", () => {
  it("renders provider, host, account, owner, health and the reported count", async () => {
    renderPage();

    expect(await screen.findByText("acme-github")).toBeTruthy();
    const acme = within(row("acme-github"));
    expect(acme.getByText("github")).toBeTruthy();
    expect(acme.getByText("GitHub App")).toBeTruthy();
    expect(acme.getByText("https://github.com")).toBeTruthy();
    expect(acme.getByText("acme")).toBeTruthy();
    expect(acme.getByText("42 repositories")).toBeTruthy();
    expect(acme.getByText("ready")).toBeTruthy();
    expect(acme.getByText("authenticated as the acme installation")).toBeTruthy();
    // The app's public identifiers, and the installation it acts as.
    expect(acme.getByText("app 12345 · installation 678910")).toBeTruthy();
    // A reference and its namespace — never a value.
    expect(acme.getByText("acme-github-app · namespace kelson-system")).toBeTruthy();

    const gitlab = within(row("internal-gitlab"));
    expect(gitlab.getByText("generic")).toBeTruthy();
    expect(gitlab.getByText("token")).toBeTruthy();
    expect(gitlab.getByText("https://git.acme.internal")).toBeTruthy();
  });

  it("says a connection nothing has probed is unobserved, not broken", async () => {
    renderPage();
    await screen.findByText("internal-gitlab");

    expect(screen.getByText("not observed")).toBeTruthy();
    expect(screen.getByText(/both conditions are false/)).toBeTruthy();
    // A count nobody reported is absent rather than printed as zero.
    expect(
      screen.getAllByText("not reported — no probe has succeeded"),
    ).toHaveLength(2);
  });

  it("shows a recorded owner and refuses to render it as a permission", async () => {
    renderPage();
    await screen.findByText("internal-gitlab");

    expect(screen.getByText("user · alice")).toBeTruthy();
    // ADR-0033 decision 6: the copy has to say this until #231 gives the field
    // a subject, on the page and on the row that carries the principal.
    expect(
      screen.getByText(/Every connection below is visible to\s+everyone on this instance/),
    ).toBeTruthy();
    expect(screen.getByText(/a recorded owner and not a boundary/)).toBeTruthy();
  });

  it("explains what a connection is and offers both paths when there are none", async () => {
    const transport = createRouterTransport((router) => {
      router.service(GitConnectionService, {
        listConnections: () => ({ connections: [] }),
      });
    });
    renderAt(transport, "/connections", "/connections", <ConnectionsPage />);

    expect(
      await screen.findByText(
        /kelson can clone public repositories and nothing else/,
      ),
    ).toBeTruthy();
    expect(screen.getByText(/GitHub's app-manifest flow/)).toBeTruthy();
    expect(screen.getByText(/the token form below/)).toBeTruthy();
    expect(screen.getByRole("link", { name: "Connect GitHub" })).toBeTruthy();
  });

  it("surfaces a failed list as the server's own structured error", async () => {
    const transport = createRouterTransport((router) => {
      router.service(GitConnectionService, {
        listConnections: () => {
          throw new ConnectError(
            "api: listing connections: the cluster could not be reached",
            Code.Unavailable,
            undefined,
            [
              {
                desc: ErrorSchema,
                value: {
                  code: "delivery/unavailable",
                  message: "the cluster could not be reached",
                },
              },
            ],
          );
        },
      });
    });
    renderAt(transport, "/connections", "/connections", <ConnectionsPage />);

    expect(await screen.findByText("Cannot list git connections")).toBeTruthy();
    expect(screen.getByText("delivery/unavailable")).toBeTruthy();
  });
});

describe("ConnectionsPage: the manifest flow", () => {
  /**
   * A browser redirect, not an RPC (ADR-0033 decision 2): the credential is
   * minted by GitHub and handed to the server, so the only thing this page can
   * do is leave. An anchor is what leaves; a <Link> would stay inside the SPA.
   */
  it("starts the flow as a plain navigation to the server's own path", async () => {
    renderPage();
    const link = await screen.findByRole("link", { name: "Connect GitHub" });

    expect(link.getAttribute("href")).toBe("/forge/github/manifest/start");
    expect(link.tagName).toBe("A");
  });

  it("is honest that the flow finishes on GitHub and that the endpoint is not here yet", async () => {
    renderPage();

    expect(
      await screen.findByText(/GitHub redirects\s+back here with a one-time code/),
    ).toBeTruthy();
    expect(screen.getByText(/is\s+server-side and lands in a later slice/)).toBeTruthy();
    expect(screen.getByText(/the\s+button answers 404/)).toBeTruthy();
  });
});

describe("ConnectionsPage: creating a token connection", () => {
  it("refuses to send a request the server would reject on shape alone", async () => {
    const seen = renderPage();
    await screen.findByText("acme-github");

    fireEvent.change(screen.getByLabelText("Name"), {
      target: { value: "Acme GitHub" },
    });
    fireEvent.change(screen.getByLabelText("Host"), {
      target: { value: "github.com" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Create the connection" }));

    expect(await screen.findByText(/is not a DNS-1123 label/)).toBeTruthy();
    expect(screen.getByText(/is not a forge base URL/)).toBeTruthy();
    expect(screen.getByText(/a Secret name is required/)).toBeTruthy();
    expect(seen.creates).toEqual([]);
  });

  it("sends exactly CreateConnectionRequest's fields, trimmed, with a key", async () => {
    const seen = renderPage();
    await screen.findByText("acme-github");

    fireEvent.change(screen.getByLabelText("Name"), {
      target: { value: " internal-token " },
    });
    fireEvent.change(screen.getByLabelText("Provider"), {
      target: { value: "generic" },
    });
    // Switching to generic drops github's default rather than claiming it.
    expect((screen.getByLabelText("Host") as HTMLInputElement).value).toBe("");
    fireEvent.change(screen.getByLabelText("Host"), {
      target: { value: "https://git.acme.internal" },
    });
    fireEvent.change(screen.getByLabelText("Secret"), {
      target: { value: "internal-git-token" },
    });

    fireEvent.click(screen.getByRole("button", { name: "Create the connection" }));

    await waitFor(() => expect(seen.creates).toHaveLength(1));
    const sent = seen.creates[0];
    expect(sent?.name).toBe("internal-token");
    expect(sent?.provider).toBe("generic");
    expect(sent?.host).toBe("https://git.acme.internal");
    expect(sent?.secretRef).toBe("internal-git-token");
    // A create is not convergent, so a retried press has to be a replay.
    expect(sent?.idempotencyKey).not.toBe("");

    expect(await screen.findByText("internal-token is connected")).toBeTruthy();
    expect(
      screen.getByText("not yet observed: no probe has reached this connection"),
    ).toBeTruthy();
    // The form is empty again: what it described has been created.
    expect((screen.getByLabelText("Name") as HTMLInputElement).value).toBe("");
  });

  it("shows the server's refusal with its code and remediation intact", async () => {
    renderPage({ refuseCreate: true });
    await screen.findByText("acme-github");

    fireEvent.change(screen.getByLabelText("Name"), {
      target: { value: "acme-ftp" },
    });
    fireEvent.change(screen.getByLabelText("Host"), {
      target: { value: "https://git.acme.internal" },
    });
    fireEvent.change(screen.getByLabelText("Secret"), {
      target: { value: "acme-git-token" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Create the connection" }));

    expect(
      await screen.findByText("The server would not create this connection"),
    ).toBeTruthy();
    expect(screen.getByText("field/invalid-format")).toBeTruthy();
    expect(screen.getByText(BAD_HOST.message)).toBeTruthy();
    expect(screen.getByText(/the host the API and the clones are reached at/)).toBeTruthy();
    expect(screen.getByText("$.spec.host")).toBeTruthy();
  });
});

describe("ConnectionsPage: probing a connection", () => {
  it("reports the account and the count the provider gave", async () => {
    const seen = renderPage();
    await screen.findByText("acme-github");

    fireEvent.click(screen.getByRole("button", { name: "Test acme-github" }));

    await waitFor(() => expect(seen.probes).toEqual(["acme-github"]));
    expect(await screen.findByText("reachable")).toBeTruthy();
    expect(screen.getByText("acme · 42 repositories")).toBeTruthy();
    expect(screen.getByText("GitHub answered in 84ms")).toBeTruthy();
  });

  /**
   * A forge that says no is an answer, not a transport failure: the RPC
   * succeeded and `reachable` is false, so it renders as a result carrying the
   * forge's own refusal rather than as an error panel.
   */
  it("shows a refusal as the probe's answer, in the forge's own words", async () => {
    renderPage();
    await screen.findByText("internal-gitlab");

    fireEvent.click(screen.getByRole("button", { name: "Test internal-gitlab" }));

    expect(await screen.findByText("unreachable")).toBeTruthy();
    expect(
      screen.getByText("401 from https://git.acme.internal: the token was revoked"),
    ).toBeTruthy();
    expect(screen.queryByText("The probe could not be made")).toBeNull();
  });

  it("surfaces a probe that could not be made at all as an error", async () => {
    const transport = createRouterTransport((router) => {
      router.service(GitConnectionService, {
        listConnections: () => ({ connections: CONNECTIONS }),
        testConnection: () => {
          throw new ConnectError("no cluster", Code.Unavailable);
        },
      });
    });
    renderAt(transport, "/connections", "/connections", <ConnectionsPage />);
    await screen.findByText("acme-github");

    fireEvent.click(screen.getByRole("button", { name: "Test acme-github" }));

    expect(await screen.findByText("The probe could not be made")).toBeTruthy();
  });
});

describe("ConnectionsPage: deleting a connection", () => {
  it("confirms before it sends one, and says what the delete does not do", async () => {
    const seen = renderPage();
    await screen.findByText("acme-github");

    fireEvent.click(screen.getByRole("button", { name: "Delete acme-github" }));
    expect(seen.deletes).toEqual([]);
    expect(screen.getByText(/the delete is not\s+blocked by them/)).toBeTruthy();
    expect(screen.getByText(/The Secret it references stays/)).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByText(/The Secret it references stays/)).toBeNull();
  });

  it("names the projects whose builds it just broke", async () => {
    const seen = renderPage({ affected: ["checkout", "billing"] });
    await screen.findByText("acme-github");

    // The row's own Delete goes away while the confirm is up, so the button
    // carrying the name now is the one that sends the request.
    fireEvent.click(screen.getByRole("button", { name: "Delete acme-github" }));
    fireEvent.click(screen.getByRole("button", { name: "Delete acme-github" }));

    await waitFor(() => expect(seen.deletes).toEqual(["acme-github"]));
    expect(
      await screen.findByText("acme-github is gone, and 2 projects resolved through it"),
    ).toBeTruthy();
    expect(screen.getByText("checkout")).toBeTruthy();
    expect(screen.getByText("billing")).toBeTruthy();
    expect(screen.getByText(/their next build has no credential/)).toBeTruthy();
    expect(screen.getByText(/the Secret it named was not deleted/)).toBeTruthy();
  });

  it("says so plainly when nothing resolved through it", async () => {
    renderPage({ affected: [] });
    await screen.findByText("internal-gitlab");

    fireEvent.click(screen.getByRole("button", { name: "Delete internal-gitlab" }));
    fireEvent.click(screen.getByRole("button", { name: "Delete internal-gitlab" }));

    expect(await screen.findByText("internal-gitlab is gone")).toBeTruthy();
    expect(
      screen.getByText("no stored project resolved its source through it"),
    ).toBeTruthy();
  });
});
