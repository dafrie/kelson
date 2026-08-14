import { Disclosure, YamlBlock } from "kelson-ui";

const SPEC = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: checkout
spec:
  components:
    web:
      image: ghcr.io/acme/checkout:1.42.0
      env:
        LOG_LEVEL: info
    cache:
      dataService: redis`;

/** Open, showing a byte-faithful stored spec document. */
export function OpenWithSpec() {
  return (
    <Disclosure summary="checkout.kelson.yaml" meta="812 B · rev 9f31c0d" open>
      <YamlBlock bytes={SPEC} />
    </Disclosure>
  );
}

/** Collapsed rows the reader expands on demand. */
export function CollapsedList() {
  return (
    <div style={{ display: "grid", gap: 8 }}>
      <Disclosure summary="rendered manifests" meta="14 documents">
        <YamlBlock bytes={SPEC} />
      </Disclosure>
      <Disclosure summary="previous revision" meta="rev b4d21aa">
        <YamlBlock bytes={SPEC} />
      </Disclosure>
    </div>
  );
}
