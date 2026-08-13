import { Link, NavLink, Outlet } from "react-router-dom";
import { KelsonMark } from "./KelsonMark";
import "./AppShell.css";

/**
 * The application shell: 56px top bar, brand, primary nav, centred content.
 *
 * The structure follows docs/design/Kelson Dashboard.dc.html, which
 * docs/design/README.md is explicit is reference and not a spec. Two things
 * are deliberately not copied from it:
 *
 *   - Its nav lists Apps / Agents / Sources / Events / Settings. Only Apps and
 *     Cluster are real destinations today, and a nav item that goes nowhere is
 *     a lie about what the product does. New items land when their screens do.
 *   - Its header carries an environment chip and an avatar. There is no
 *     environment selector yet and v0 has no authentication at all
 *     (ADR-0013 §3), so there is no user to put in an avatar.
 */

// There is no hosted docs site: website/ builds on every PR but
// .github/workflows/docs.yml only deploys on manual dispatch, and Pages is not
// enabled on this plan. The repository tree is the honest destination.
const DOCS_URL = "https://github.com/dafrie/kelson/tree/main/docs";

const NAV = [
  { to: "/apps", label: "Apps" },
  { to: "/cluster", label: "Cluster" },
] as const;

export function AppShell() {
  return (
    <>
      <header className="k-header">
        {/* Link, not NavLink: the brand is a way home, not a nav item, and it
            must not claim aria-current when /apps happens to be open. */}
        <Link to="/apps" className="k-header__brand">
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
        </div>
      </header>

      <main className="k-main">
        <Outlet />
      </main>
    </>
  );
}
