import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";

import { ErrorSchema } from "../gen/kelson/v1alpha1/common_pb";
import {
  SecretService,
  type SetSecretRequest,
} from "../gen/kelson/v1alpha1/secret_pb";
import { formatAge } from "../components/phase";
import { renderAt } from "../test/render";
import { SecretsPanel } from "./SecretsPanel";

/**
 * The panel against a stub SecretService — the real generated client, the real
 * serialisation, a server that is not there.
 *
 * The stub is what the schema promises and nothing more: no message it returns
 * has a field a value could travel in (secret.proto), so a test that asserted
 * "the value is not displayed" would be asserting something the wire cannot
 * express. What is asserted instead is what the panel *sends* — the request the
 * server would act on — and what it says about the values afterwards.
 */

/** The structured refusal internal/secret returns for an unlabelled Secret. */
const NOT_MANAGED = {
  code: "secret/not-managed",
  resource: "Secret/checkout-production/tls",
  message:
    'Secret "tls" exists in namespace checkout-production but kelson did not write it',
  remediation:
    "adopt it deliberately with `kubectl -n checkout-production label secret tls kelson.dev/managed-secret=true`, or choose another name",
};

interface Stub {
  /** Every SetSecret the panel sent, in order. */
  sets: SetSecretRequest[];
  deletes: string[];
}

function transportFor(options: { refuseDelete?: boolean } = {}) {
  const seen: Stub = { sets: [], deletes: [] };
  const transport = createRouterTransport((router) => {
    router.service(SecretService, {
      listSecrets: () => ({
        namespace: "checkout-production",
        secrets: [
          {
            name: "checkout-db",
            namespace: "checkout-production",
            keys: ["password", "url"],
            createdAt: "2026-08-01T10:00:00Z",
            ageSeconds: 259200n,
          },
          {
            name: "stripe",
            namespace: "checkout-production",
            keys: ["api-key"],
            ageSeconds: 90n,
          },
        ],
      }),
      setSecret: (req) => {
        seen.sets.push(req);
        return {
          secret: {
            name: req.name,
            namespace: "checkout-production",
            keys: [...Object.keys(req.values).sort(), "kept-by-merge"],
            ageSeconds: 5n,
          },
          writtenKeys: Object.keys(req.values).sort(),
        };
      },
      deleteSecret: (req) => {
        seen.deletes.push(req.name);
        if (options.refuseDelete) {
          throw new ConnectError(
            NOT_MANAGED.message,
            Code.FailedPrecondition,
            undefined,
            [{ desc: ErrorSchema, value: NOT_MANAGED }],
          );
        }
        return { deleted: true, namespace: "checkout-production" };
      },
    });
  });
  return { transport, seen };
}

function renderPanel(options: { refuseDelete?: boolean } = {}) {
  const { transport, seen } = transportFor(options);
  renderAt(
    transport,
    "/",
    "/",
    <SecretsPanel project="checkout" environment="production" />,
  );
  return seen;
}

describe("SecretsPanel", () => {
  it("lists what kelson manages: names, keys and ages, never a value", async () => {
    renderPanel();

    expect(await screen.findByText("Secrets (2)")).toBeTruthy();
    expect(screen.getByText("checkout-db")).toBeTruthy();
    expect(screen.getByText("stripe")).toBeTruthy();
    // Keys are chips because a key name is a reference — it is what a spec's
    // `key:` names — and is reported everywhere a value is not.
    expect(screen.getByText("password")).toBeTruthy();
    expect(screen.getByText("url")).toBeTruthy();
    expect(screen.getByText("api-key")).toBeTruthy();
    // The age the CLI would print, from the seconds the server counted.
    expect(screen.getByText("3d")).toBeTruthy();
    expect(screen.getByText("1m")).toBeTruthy();
  });

  it("says an empty namespace holds nothing kelson wrote, not that it is empty", async () => {
    const transport = createRouterTransport((router) => {
      router.service(SecretService, {
        listSecrets: () => ({ namespace: "checkout-production", secrets: [] }),
      });
    });
    renderAt(
      transport,
      "/",
      "/",
      <SecretsPanel project="checkout" environment="production" />,
    );

    expect(
      await screen.findByText(/kelson manages no Secrets in namespace/),
    ).toBeTruthy();
    expect(screen.getByText(/absent rather than filtered/)).toBeTruthy();
  });

  it("sends the keys as typed and never asks for a value back", async () => {
    const seen = renderPanel();
    await screen.findByText("Secrets (2)");

    fireEvent.change(screen.getByLabelText("Secret name"), {
      target: { value: " payments " },
    });
    fireEvent.change(screen.getByLabelText("Key 1"), {
      target: { value: "api-key" },
    });
    const value = screen.getByLabelText("Value 1");
    // A credential is not something a shoulder-surfer gets for free.
    expect(value.getAttribute("type")).toBe("password");
    fireEvent.change(value, { target: { value: "sk_live_1" } });

    fireEvent.click(screen.getByRole("button", { name: "Add key" }));
    fireEvent.change(screen.getByLabelText("Key 2"), {
      target: { value: "webhook-secret" },
    });
    fireEvent.change(screen.getByLabelText("Value 2"), {
      target: { value: "whsec_2" },
    });

    fireEvent.click(screen.getByRole("button", { name: "Write the Secret" }));

    await waitFor(() => expect(seen.sets).toHaveLength(1));
    const sent = seen.sets[0];
    expect(sent?.name).toBe("payments");
    expect(sent?.target?.project).toBe("checkout");
    expect(sent?.target?.environment).toBe("production");
    expect(sent?.values).toEqual({
      "api-key": "sk_live_1",
      "webhook-secret": "whsec_2",
    });

    // What the response can say, said: which keys were written, which the merge
    // kept, and that no value is coming back.
    expect(await screen.findByText("payments is written")).toBeTruthy();
    expect(screen.getByText(/keys set: api-key, webhook-secret/)).toBeTruthy();
    expect(screen.getByText(/keys kept: kept-by-merge/)).toBeTruthy();
    // Said twice on purpose: once beside the button, before the value is sent,
    // and once after, where a reader would go looking for it.
    expect(screen.getAllByText(/values are write-only/)).toHaveLength(2);
    expect(screen.getByText(/nothing in kelson reads one back/)).toBeTruthy();

    // The ready-to-paste reference, one per key, exactly as `kelson secret set`
    // prints it and exactly as the spec editor writes it.
    expect(
      screen.getByRole("button", {
        name: "copy { secret: payments, key: api-key }",
      }),
    ).toBeTruthy();
    expect(
      screen.getByRole("button", {
        name: "copy { secret: payments, key: webhook-secret }",
      }),
    ).toBeTruthy();

    // And the values are gone from the page: nothing can show them again.
    expect((screen.getByLabelText("Value 1") as HTMLInputElement).value).toBe("");
  });

  it("confirms a delete before it sends one", async () => {
    const seen = renderPanel();
    await screen.findByText("Secrets (2)");

    fireEvent.click(screen.getByRole("button", { name: "Delete checkout-db" }));
    // Nothing has been sent yet — the confirm is the act.
    expect(seen.deletes).toEqual([]);
    expect(screen.getByText(/kelson does not look for referrers/)).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByText(/kelson does not look for referrers/)).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Delete checkout-db" }));
    // The row's own Delete is gone while the confirm is up, so the button that
    // carries the name now is the one that sends the request.
    fireEvent.click(screen.getByRole("button", { name: "Delete checkout-db" }));

    await waitFor(() => expect(seen.deletes).toEqual(["checkout-db"]));
  });

  it("shows the not-managed refusal as the server's own structured error", async () => {
    renderPanel({ refuseDelete: true });
    await screen.findByText("Secrets (2)");

    fireEvent.click(screen.getByRole("button", { name: "Delete stripe" }));
    fireEvent.click(screen.getByRole("button", { name: "Delete stripe" }));

    // The code an agent branches on, the message, and the remediation as the
    // CLI's own "fix:" line — none of it paraphrased in the browser.
    expect(await screen.findByText("secret/not-managed")).toBeTruthy();
    expect(screen.getByText(NOT_MANAGED.message)).toBeTruthy();
    expect(screen.getByText(/kubectl -n checkout-production label secret/)).toBeTruthy();
    expect(screen.getByText("Secret/checkout-production/tls")).toBeTruthy();
  });
});

describe("formatAge", () => {
  it("prints the ages `kelson secret list` prints", () => {
    expect(formatAge(0n)).toBe("-");
    expect(formatAge(-5n)).toBe("-");
    expect(formatAge(45n)).toBe("45s");
    expect(formatAge(90n)).toBe("1m");
    expect(formatAge(7200n)).toBe("2h");
    expect(formatAge(259200n)).toBe("3d");
  });
});
