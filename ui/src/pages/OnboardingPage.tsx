import { Link } from "react-router-dom";

import { useAsync, useClients } from "../api/data";
import { ComponentsChecklist } from "../components/ComponentsChecklist";

/**
 * The onboarding screen: what this cluster already provides, what kelson
 * offers to add, and what to do next (issue #60, ADR-0021).
 *
 * It is deliberately the same checklist the Cluster page carries — one
 * component, one install flow, one truth — with the framing a first run
 * needs: a progress line, and the pointer to creating the first project once
 * routing exists. ADR-0003's rule holds on screen exactly as it does in the
 * CLI: detection decides, presence refuses, and nothing installs without a
 * previewed plan and an explicit click.
 */
export function OnboardingPage() {
  const clients = useClients();
  const list = useAsync(
    (signal) => clients.install.listComponents({}, { signal }),
    [clients],
  );
  const components = list.data?.components ?? [];
  const present = components.filter((c) => c.presence === "yes").length;

  return (
    <>
      <div className="k-page-head">
        <h1>Set up this cluster</h1>
      </div>
      <div className="k-page-sub">
        <span>
          kelson detects what your cluster already runs and offers to install
          what is missing — never both
          {components.length > 0
            ? ` · ${present} of ${components.length} components present`
            : ""}
        </span>
      </div>

      <section className="k-section">
        <div className="k-eyebrow">Platform components</div>
        <div className="k-section__body">
          <ComponentsChecklist />
        </div>
      </section>

      <section className="k-section">
        <div className="k-eyebrow">Then</div>
        <div className="k-section__body k-panel">
          <p>
            With a Gateway API implementation installed, projects that declare
            domains render and route. <Link to="/projects/new">Create your
            first project</Link>, or review the cluster as kelson reads it on
            the <Link to="/cluster">Cluster</Link> page.
          </p>
        </div>
      </section>
    </>
  );
}
