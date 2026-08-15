import { Link, NavLink, Outlet } from "react-router-dom";
import { useAuth } from "../api/auth";
import { KelsonMark } from "./KelsonMark";
import { ThemeToggle } from "./ThemeToggle";
import "./AppShell.css";

/**
 * The application shell: 56px top bar, brand, primary nav, centred content.
 *
 * The structure follows docs/design/Kelson Dashboard.dc.html, which
 * docs/design/README.md is explicit is reference and not a spec. Two things
 * are deliberately not copied from it:
 *
 *   - Its nav lists Apps / Agents / Sources / Events / Settings. Only the first
 *     of those is a real destination today — and it is called Projects here,
 *     because that is what the model calls it (docs/model.md, ADR-0014) — plus
 *     Cluster. A nav item that goes nowhere is a lie about what the product
 *     does, so new items land when their screens do.
 *   - Its header carries an environment chip and an avatar. There is still no
 *     environment selector. The avatar arrived with the interim login (#84,
 *     docs/server.md) and holds the session's display name — one letter,
 *     because a shared password has no account behind it to have a picture —
 *     and it is absent entirely on a server with no password, which is the
 *     default and the whole of today's behaviour.
 */

// There is no hosted docs site: website/ builds on every PR but
// .github/workflows/docs.yml only deploys on manual dispatch, and Pages is not
// enabled on this plan. The repository tree is the honest destination.
const DOCS_URL = "https://github.com/dafrie/kelson/tree/main/docs";

const NAV = [
  { to: "/projects", label: "Projects" },
  { to: "/cluster", label: "Cluster" },
  // The mockup's "Sources", arrived: the forges this instance can pull from
  // (ADR-0033). It sits beside Cluster and Setup because it describes the
  // instance rather than a project, and every project's source resolves
  // through it.
  { to: "/connections", label: "Connections" },
  { to: "/setup", label: "Setup" },
] as const;

function SignedIn() {
  const { state, signOut } = useAuth();
  if (state.status !== "authenticated") return null;
  // The server refuses an empty username, so the initial always exists; the
  // fallback is for a bearer-authenticated caller, which has no display name.
  const initial = state.username.trim().charAt(0) || "?";
  return (
    <div className="k-user">
      <span className="k-user__avatar" aria-hidden="true">
        {initial}
      </span>
      <span className="k-user__name" title={state.username}>
        {state.username}
      </span>
      <button
        className="k-user__out"
        type="button"
        onClick={() => void signOut()}
      >
        Sign out
      </button>
    </div>
  );
}

export function AppShell() {
  return (
    <>
      <header className="k-header">
        {/* Link, not NavLink: the brand is a way home, not a nav item, and it
            must not claim aria-current when /projects happens to be open. */}
        <Link to="/projects" className="k-header__brand">
          <KelsonMark />
          <span className="k-header__wordmark">kelson</span>
        </Link>

        <nav className="k-nav" aria-label="Primary">
          {NAV.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              className={({ isActive }) =>
                isActive ? "k-nav__item k-nav__item--active" : "k-nav__item"
              }
            >
              {item.label}
            </NavLink>
          ))}
        </nav>

        <div className="k-header__right">
          <a
            className="k-header__docs"
            href={DOCS_URL}
            target="_blank"
            rel="noreferrer"
          >
            Docs
          </a>
          <ThemeToggle />
          <SignedIn />
        </div>
      </header>

      <main className="k-main">
        <Outlet />
      </main>
    </>
  );
}
