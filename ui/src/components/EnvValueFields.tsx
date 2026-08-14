import {
  bindingEnv,
  plainEnv,
  secretEnv,
  type EnvValue,
} from "../spec/documents";

/**
 * The value half of one environment-variable row, in whichever of the three
 * forms it has (ADR-0018).
 *
 * A scalar is a value; a mapping is a reference. The form picker is a select
 * rather than a mode inferred from what is typed, because the three forms are
 * three *different statements* — "this is the value", "this key of that Secret",
 * "this key of that data component" — and a UI that guessed which one an author
 * meant from the shape of a string would be reinventing the templating language
 * ADR-0018 refuses to have.
 *
 * Switching form keeps nothing. A Secret name is not a value and a value is not
 * a service name, so carrying text across would produce a reference to
 * something the author never named; the empty fields are the honest state.
 *
 * There is no value input for either reference form, and that is the point: the
 * spec carries references, never credentials (ADR-0009). Where the credential
 * itself is written is the Secrets panel on the project page, or `kelson secret
 * set` — the hint beside every env block says so.
 */
export function EnvValueFields({
  value,
  name,
  readOnly = false,
  onChange,
}: {
  value: EnvValue;
  /**
   * The accessible-name prefix for the inputs — "Variable 1", "Environment
   * variables 2". Each field appends what it is, so a test and a screen reader
   * address the same control by the same words.
   */
  name: string;
  readOnly?: boolean;
  onChange: (value: EnvValue) => void;
}) {
  return (
    <>
      <select
        className="k-select k-mono"
        aria-label={`${name} form`}
        value={value.kind}
        disabled={readOnly}
        onChange={(e) => onChange(emptyOf(e.target.value))}
      >
        <option value="plain">plain</option>
        <option value="secret">secret ref</option>
        <option value="binding">service binding</option>
      </select>

      {value.kind === "plain" ? (
        <input
          className="k-input k-mono"
          aria-label={`${name} value`}
          value={value.value}
          readOnly={readOnly}
          placeholder="info"
          onChange={(e) => onChange(plainEnv(e.target.value))}
        />
      ) : null}

      {value.kind === "secret" ? (
        <>
          <input
            className="k-input k-mono"
            aria-label={`${name} secret name`}
            value={value.secret}
            readOnly={readOnly}
            placeholder="checkout-db"
            onChange={(e) => onChange(secretEnv(e.target.value, value.key))}
          />
          <input
            className="k-input k-mono"
            aria-label={`${name} secret key`}
            value={value.key}
            readOnly={readOnly}
            placeholder="url"
            onChange={(e) => onChange(secretEnv(value.secret, e.target.value))}
          />
        </>
      ) : null}

      {value.kind === "binding" ? (
        <>
          <input
            className="k-input k-mono"
            aria-label={`${name} service`}
            value={value.service}
            readOnly={readOnly}
            placeholder="db"
            onChange={(e) => onChange(bindingEnv(e.target.value, value.key))}
          />
          <input
            className="k-input k-mono"
            aria-label={`${name} service key`}
            value={value.key}
            readOnly={readOnly}
            placeholder="uri"
            onChange={(e) => onChange(bindingEnv(value.service, e.target.value))}
          />
        </>
      ) : null}
    </>
  );
}

function emptyOf(kind: string): EnvValue {
  if (kind === "secret") return secretEnv("", "");
  if (kind === "binding") return bindingEnv("", "");
  return plainEnv("");
}

/**
 * What a row means, in one line under it.
 *
 * The reference forms get a sentence because a reader who has just picked
 * "secret ref" has a question — where does the value go? — and answering it
 * beside the control is what stops them typing the credential into the name
 * field.
 */
export function envValueNote(value: EnvValue): string {
  switch (value.kind) {
    case "plain":
      return "a plain value, stored in the spec as written — never a credential";
    case "secret":
      return "renders as valueFrom.secretKeyRef against a Secret in this environment's namespace; kelson references it and never reads it";
    case "binding":
      return "binds to a data component this project declares — kelson derives the Secret its operator generates";
  }
}
