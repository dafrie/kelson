import { Copyable } from "kelson-ui";

/** A revision sha, click-to-copy, in the DS mono. */
export function RevisionSha() {
  return <Copyable value="9f31c0d4e8a2b7665410fedc" label="9f31c0d" title="9f31c0d4e8a2b7665410fedc" />;
}

/** A full value as its own label — namespaces, error codes. */
export function PlainValues() {
  return (
    <div style={{ display: "flex", gap: 12, alignItems: "center", flexWrap: "wrap" }}>
      <Copyable value="shop-production" />
      <Copyable value="delivery/unsupported" />
      <Copyable value="kelson secret set checkout-db --env production" />
    </div>
  );
}
