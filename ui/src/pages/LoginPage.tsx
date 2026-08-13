import { useEffect, useId, useState, type FormEvent } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";

import { safeReturnPath, useAuth } from "../api/auth";
import { KelsonMark } from "../components/KelsonMark";
import "./LoginPage.css";

/**
 * The login screen.
 *
 * One shared password, and a username that is a display name rather than an
 * identity (#84's interim cut, docs/server.md). The form still asks for both,
 * because the owner asked for a normal login and a password-only box is not
 * one — but the footnote says what the username is and is not, so nobody
 * concludes their account is separate from anyone else's.
 *
 * Two paths lead here and they need different words. A cold boot against a
 * server that wants a password is ordinary. An expiry mid-work — the server
 * restarted, and its signing key restarted with it — is a surprise, and the
 * page says so and keeps the return path so the login lands back where the work
 * was.
 */
export function LoginPage() {
  const { state, signIn } = useAuth();
  const navigate = useNavigate();
  const [params] = useSearchParams();
  const usernameId = useId();
  const passwordId = useId();

  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | undefined>(undefined);
  const [busy, setBusy] = useState(false);

  const returnTo = safeReturnPath(params.get("next"));
  const expired = params.get("expired") === "1";

  // A session that already exists — or a server with no password at all — must
  // not be made to look at a login form. This covers the back button as well as
  // the moment just after a successful sign-in.
  const settled = state.status === "authenticated" || state.status === "disabled";
  useEffect(() => {
    if (settled) navigate(returnTo, { replace: true });
  }, [settled, navigate, returnTo]);

  async function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError(undefined);
    setBusy(true);
    try {
      await signIn(username, password);
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err));
      setBusy(false);
    }
  }

  return (
    <div className="k-login">
      <div className="k-login__brand">
        <KelsonMark size={30} strokeWidth={9} />
        <span className="k-login__wordmark">kelson</span>
      </div>

      <form className="k-login__form" onSubmit={onSubmit}>
        {expired ? (
          <p className="k-login__note k-login__note--expired" role="status">
            Your session expired. Sign in to pick up where you left off.
          </p>
        ) : null}

        <div className="k-field">
          <label className="k-login__label" htmlFor={usernameId}>
            Username
          </label>
          <input
            id={usernameId}
            className="k-input"
            name="username"
            autoComplete="username"
            autoFocus
            value={username}
            onChange={(e) => setUsername(e.target.value)}
          />
        </div>

        <div className="k-field">
          <label className="k-login__label" htmlFor={passwordId}>
            Password
          </label>
          <input
            id={passwordId}
            className="k-input"
            name="password"
            type="password"
            autoComplete="current-password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
        </div>

        {error ? (
          <p className="k-login__error" role="alert">
            {error}
          </p>
        ) : null}

        <button
          className="k-button k-button--primary k-button--wide"
          type="submit"
          disabled={busy || username.trim() === "" || password === ""}
        >
          {busy ? "Signing in…" : "Sign in"}
        </button>
      </form>

      <p className="k-login__foot">
        One shared password guards this server. The username is a display name —
        it labels your session, it is not an account.
      </p>
    </div>
  );
}
