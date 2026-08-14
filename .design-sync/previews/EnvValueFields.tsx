import { EnvValueFields } from "kelson-ui";

const row: React.CSSProperties = {
  display: "grid",
  gridTemplateColumns: "140px 1fr 1fr",
  gap: 8,
  alignItems: "center",
  maxWidth: 560,
};

/** The plain form: the value is stored in the spec as written. */
export function Plain() {
  return (
    <div style={row}>
      <EnvValueFields
        name="Variable 1"
        value={{ kind: "plain", value: "info" }}
        onChange={() => {}}
      />
    </div>
  );
}

/** A secret reference: name + key, never the credential itself. */
export function SecretRef() {
  return (
    <div style={row}>
      <EnvValueFields
        name="Variable 2"
        value={{ kind: "secret", secret: "checkout-db", key: "url" }}
        onChange={() => {}}
      />
    </div>
  );
}

/** A service binding to a data component this project declares. */
export function ServiceBinding() {
  return (
    <div style={row}>
      <EnvValueFields
        name="Variable 3"
        value={{ kind: "binding", service: "db", key: "uri" }}
        onChange={() => {}}
      />
    </div>
  );
}

/** Read-only, as the promote screen shows it. */
export function ReadOnly() {
  return (
    <div style={row}>
      <EnvValueFields
        name="Variable 4"
        value={{ kind: "plain", value: "warn" }}
        readOnly
        onChange={() => {}}
      />
    </div>
  );
}
