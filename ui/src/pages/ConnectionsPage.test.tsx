import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
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
 * Same as [renderPage], at a path a test chooses — the manifest callback's own
 * shape (`/connections?connected=…`) — and with the router handed back, which
 * is what the URL-clearing tests need to look at.
 */
function renderPageAt(path: string) {
  const { transport } = transportFor();
  return renderAt(transport, path, "/connections", <ConnectionsPage />);
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

// `startManifestSession` and the button's `window.location.assign` both go
// around the router transport entirely (POST /forge/github/manifest/session
// is not a ConnectRPC call), so they are stubbed the same way auth.test.tsx
// stubs /auth/*: a fetch mock and a cleared global between tests.
beforeEach(() => {
  vi.unstubAllGlobals();
});

afterEach(() => {
  vi.unstubAllGlobals();
});

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
    expect(
      screen.getByText(/nothing has probed this connection yet/),
    ).toBeTruthy();
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
    expect(screen.getByText(/a recorded owner, not a boundary/)).toBeTruthy();
  });

  it("says what is missing and offers both paths when there are none", async () => {
    const transport = createRouterTransport((router) => {
      router.service(GitConnectionService, {
        listConnections: () => ({ connections: [] }),
      });
    });
    renderAt(transport, "/connections", "/connections", <ConnectionsPage />);

    expect(await screen.findByText("No connections yet")).toBeTruthy();
    expect(
      screen.getByText(/kelson can clone public repositories and nothing else/),
    ).toBeTruthy();
    expect(screen.getByText(/the token form below/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Connect GitHub" })).toBeTruthy();
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

/** A one-shot fetch mock for `POST /forge/github/manifest/session`. */
function stubManifestSession(answer: { status: number; body?: unknown }) {
  const calls: (RequestInit | undefined)[] = [];
  const fetchMock = vi.fn(
    async (_input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      calls.push(init);
      return new Response(
        answer.body === undefined ? null : JSON.stringify(answer.body),
        { status: answer.status, headers: { "Content-Type": "application/json" } },
      );
    },
  );
  vi.stubGlobal("fetch", fetchMock);
  return { calls, fetchMock };
}

describe("ConnectionsPage: the authenticated start", () => {
  // jsdom's `Location.assign` cannot be spied on directly (it is not
  // configurable), so the whole property is replaced for the duration of the
  // one test that navigates, and put back afterwards.
  const originalLocation = window.location;
  afterEach(() => {
    Object.defineProperty(window, "location", {
      configurable: true,
      value: originalLocation,
    });
  });

  /**
   * The fixed contract (#248): a `POST` carrying the session cookie, answering
   * a one-time `startUrl` this button then top-level-navigates to. Nothing
   * about the ticket is this page's to invent, so the test only pins the shape
   * of the request and that the exact `startUrl` the server names is where the
   * browser goes — never a path this page reconstructs itself.
   */
  it("fetches a ticket, says it is working, and navigates to the startUrl it names", async () => {
    const assign = vi.fn();
    Object.defineProperty(window, "location", {
      configurable: true,
      value: { ...originalLocation, assign },
    });

    let resolveFetch!: (res: Response) => void;
    const fetchMock = vi.fn(
      () =>
        new Promise<Response>((resolve) => {
          resolveFetch = resolve;
        }),
    );
    vi.stubGlobal("fetch", fetchMock);
    renderPage();
    await screen.findByText("acme-github");

    const button = screen.getByRole("button", { name: "Connect GitHub" });
    fireEvent.click(button);

    expect(await screen.findByRole("button", { name: "Connecting…" })).toBeTruthy();
    expect((button as HTMLButtonElement).disabled).toBe(true);
    expect(fetchMock).toHaveBeenCalledWith(
      "/forge/github/manifest/session",
      expect.objectContaining({ method: "POST", credentials: "same-origin" }),
    );

    resolveFetch(
      new Response(
        JSON.stringify({ startUrl: "/forge/github/manifest/start?ticket=abc123" }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );

    await waitFor(() =>
      expect(assign).toHaveBeenCalledWith("/forge/github/manifest/start?ticket=abc123"),
    );
    expect(await screen.findByRole("button", { name: "Connect GitHub" })).toBeTruthy();
  });

  it("surfaces a failed fetch through the page's own error panel", async () => {
    stubManifestSession({ status: 404 });
    renderPage();
    await screen.findByText("acme-github");

    fireEvent.click(screen.getByRole("button", { name: "Connect GitHub" }));

    expect(
      await screen.findByText("Could not start the GitHub connection"),
    ).toBeTruthy();
    expect(screen.getByText(/404/)).toBeTruthy();
  });

  it("is honest that a 401 means the session password is wrong or expired", async () => {
    stubManifestSession({ status: 401 });
    renderPage();
    await screen.findByText("acme-github");

    fireEvent.click(screen.getByRole("button", { name: "Connect GitHub" }));

    expect(
      await screen.findByText(/the session password is wrong or has expired/),
    ).toBeTruthy();
  });
});

describe("ConnectionsPage: the manifest callback", () => {
  /**
   * `internal/forgehttp`'s `redirectToConnections` always pairs `connected`
   * with `install` on a success (the app exists, but nothing is installed
   * yet) — ADR-0033 decision 2 step 3, the half of the ceremony GitHub's own
   * site finishes.
   */
  it("announces the new connection and prompts to finish installing it on GitHub", async () => {
    renderPageAt(
      "/connections?connected=acme-github&install=" +
        encodeURIComponent("https://github.com/apps/kelson-acme/installations/new"),
    );

    expect(await screen.findByText("acme-github is connected")).toBeTruthy();
    expect(screen.getByText(/shows unreachable/)).toBeTruthy();
    const install = screen.getByRole("link", { name: "Install the app on GitHub" });
    expect(install.getAttribute("href")).toBe(
      "https://github.com/apps/kelson-acme/installations/new",
    );
  });

  it("announces the connection without an install prompt when the callback named none", async () => {
    renderPageAt("/connections?connected=acme-github");

    expect(await screen.findByText("acme-github is connected")).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Install the app on GitHub" })).toBeNull();
  });

  it("renders the server's own message on a refused flow", async () => {
    renderPageAt(
      "/connections?error=exchange&message=" +
        encodeURIComponent(
          "GitHub refused the one-time code; the code is single-use and expires an hour after the redirect, so start again",
        ),
    );

    expect(await screen.findByText("Connect GitHub did not finish")).toBeTruthy();
    expect(screen.getByText(/GitHub refused the one-time code/)).toBeTruthy();
  });

  it("clears the params from the URL once rendered, so a refresh does not repeat the announcement", async () => {
    const { router } = renderPageAt(
      "/connections?connected=acme-github&install=" +
        encodeURIComponent("https://github.com/apps/kelson-acme/installations/new"),
    );

    expect(await screen.findByText("acme-github is connected")).toBeTruthy();
    await waitFor(() => expect(router.state.location.search).toBe(""));
    // The announcement itself survives the clearing — it is state, not a
    // read of the (now scrubbed) URL.
    expect(screen.getByText("acme-github is connected")).toBeTruthy();
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
    expect(
      screen.getByText(/are named once it is gone/),
    ).toBeTruthy();
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
