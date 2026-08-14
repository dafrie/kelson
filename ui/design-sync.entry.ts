/**
 * design-sync bundle entry (claude.ai/design import — see .design-sync/ at the
 * repo root). Exports exactly the presentational component set; the app itself
 * never imports this file, and it is outside tsconfig's include on purpose.
 *
 * Deliberately excluded: main.tsx (mounts the app), pages/, and the
 * backend-wired panels (ComponentsChecklist, NodesSection, CapabilityPanel,
 * DataServices, Previews, SecretsPanel) — they fetch from kelson-server and
 * cannot render as standalone design-system parts.
 */
// Same stylesheet set main.tsx loads, same order: tokens, base, then the page
// chrome that styles the shared controls (k-input, k-button, k-select, …).
import "./src/styles/tokens.css";
import "./src/styles/base.css";
import "./src/pages/pages.css";

export { AppShell } from "./src/components/AppShell";
export { Copyable } from "./src/components/Copyable";
export { Disclosure, YamlBlock } from "./src/components/Disclosure";
export { EnvValueFields, envValueNote } from "./src/components/EnvValueFields";
export { ErrorPanel } from "./src/components/ErrorPanel";
export { KelsonMark } from "./src/components/KelsonMark";
export { LiveIndicator } from "./src/components/LiveIndicator";
export {
  EmptyState,
  ErrorState,
  LoadingState,
  ServerUnreachableState,
} from "./src/components/States";
export { STATUS_KINDS, StatusPill, type StatusKind } from "./src/components/StatusPill";
export { ThemeToggle } from "./src/components/ThemeToggle";
export { PhaseRail } from "./src/deploy/PhaseRail";
export { buildRail, type RailInput } from "./src/deploy/rail";
export { DiffView } from "./src/diff/DiffView";
export type { Diff } from "./src/diff/parse";
