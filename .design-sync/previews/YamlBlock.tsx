import { YamlBlock } from "kelson-ui";

/** Bytes printed exactly as they arrived, in the DS mono on a dim panel. */
export function StoredDocument() {
  return (
    <YamlBlock
      bytes={`apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: checkout
spec:
  components:
    web:
      image: ghcr.io/acme/checkout:1.42.0`}
    />
  );
}
