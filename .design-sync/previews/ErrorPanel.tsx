import { ErrorPanel } from "kelson-ui";

/**
 * Structured wire errors as RenderResponse.errors carries them: code chip,
 * location trail, message, cause, fix line and docs link.
 */
export function StructuredErrors() {
  return (
    <ErrorPanel
      title="The spec did not render"
      errors={[
        {
          code: "renderer/unknown-component",
          resource: "",
          application: "checkout",
          target: "",
          overlay: "",
          field: "components.cache",
          line: 14,
          column: 3,
          message: 'no data service named "redis-7" is available in this cluster',
          cause: "",
          remediation: 'run `kelson capabilities` to list the data services this cluster offers',
          docsUrl: "https://github.com/dafrie/kelson/tree/main/docs/model.md",
        },
        {
          code: "model/invalid-reference",
          resource: "",
          application: "checkout",
          target: "",
          overlay: "production",
          field: "env.DATABASE_URL",
          line: 0,
          column: 0,
          message: "the overlay names a secret this environment does not define",
          cause: "secret checkout-db not found in namespace shop-production",
          remediation: "kelson secret set checkout-db --env production",
          docsUrl: "",
        },
      ] as never[]}
    />
  );
}

/** A transport failure with no structured details — the message stands alone. */
export function PlainFailure() {
  return (
    <ErrorPanel
      title="Deploy failed"
      error={new Error("deadline exceeded while waiting for the reconciler")}
    />
  );
}
