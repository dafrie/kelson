import {
  PromotionStatus,
  type PromotedComponent,
} from "../gen/kelson/v1alpha1/deploy_pb";

/**
 * Reading a `PromoteResponse` for display, and refusing to read more than it
 * says.
 *
 * `DeployService.Promote` answers with one `PromotedComponent` per component it
 * considered — `component`, `from_image`, `to_image`, `status`, `code`,
 * `reason` — and the screen derives from those six and nothing else. Two
 * absences are worth stating, because they are what keeps this file small:
 *
 *   - **No image metadata.** The wire carries a reference string. Whether that
 *     digest exists in a registry, when it was built, or what commit produced
 *     it is not on this wire and is not guessed here; the reference is shown as
 *     the reference it is.
 *   - **No outcome.** A promotion writes a spec and deploys nothing, so nothing
 *     in the response says whether the target environment is healthy, or even
 *     whether it has been deployed since. `DeployService.Status` answers that,
 *     on another screen, after a deploy that has not happened yet.
 */

/**
 * The plan's decision for one component, as a lowercase word.
 *
 * The words are internal/promote's own `Status` values ("pinned", "unchanged",
 * "skipped") rather than a UI vocabulary, so the screen, the CLI's plan table
 * and the MCP tool all say the same thing about the same decision. An enum
 * value this build does not know is `unknown` — a newer server naming a fourth
 * outcome must not have it silently rendered as one of the three.
 */
export type PromotionStatusName = "pinned" | "unchanged" | "skipped" | "unknown";

export function statusName(status: PromotionStatus): PromotionStatusName {
  switch (status) {
    case PromotionStatus.PINNED:
      return "pinned";
    case PromotionStatus.UNCHANGED:
      return "unchanged";
    case PromotionStatus.SKIPPED:
      return "skipped";
    default:
      return "unknown";
  }
}

export interface PromotionCounts {
  pinned: number;
  unchanged: number;
  skipped: number;
  unknown: number;
}

export function countStatuses(
  components: readonly PromotedComponent[],
): PromotionCounts {
  const counts: PromotionCounts = {
    pinned: 0,
    unchanged: 0,
    skipped: 0,
    unknown: 0,
  };
  for (const c of components) counts[statusName(c.status)] += 1;
  return counts;
}

/**
 * The one-line roll-up, in the CLI's words (internal/promote's `Summary`), so
 * the same plan reads the same in both surfaces. A status this build does not
 * know is appended rather than folded into one of the three, because a count
 * that silently absorbs an unrecognised outcome is a count that lies.
 */
export function promotionSummary(counts: PromotionCounts): string {
  const line = `${counts.pinned} pinned, ${counts.unchanged} unchanged, ${counts.skipped} skipped`;
  return counts.unknown === 0
    ? line
    : `${line}, ${counts.unknown} not understood by this build`;
}

/**
 * How wide an image reference is allowed to be on screen, and how much of its
 * tail survives the middle truncation.
 *
 * A promoted image is a digest reference — `ghcr.io/acme/checkout@sha256:` plus
 * sixty-four hex characters — and printing it whole turns every table row into
 * two. Both ends carry meaning and the middle does not: the head is the
 * repository and the leading hex a reader compares at a glance, and the tail is
 * what tells two truncations of the same repository apart. So the middle is
 * what goes.
 */
const IMAGE_MAX = 48;
const IMAGE_TAIL = 8;

/**
 * An image reference at reading length, elided in the middle.
 *
 * Nothing is parsed: a tag, a digest, a bare repository and a reference this
 * function has never seen are all treated as the string they are, because the
 * grammar of an image reference is the registry's business (docs/model.md) and
 * a truncation that assumed a shape would mangle the one that does not have it.
 * The full value always travels with it — every call site puts it on the title
 * and on the clipboard — so the ellipsis hides nothing a hover cannot recover.
 */
export function shortImage(image: string): string {
  if (image.length <= IMAGE_MAX) return image;
  return `${image.slice(0, IMAGE_MAX - IMAGE_TAIL - 1)}…${image.slice(-IMAGE_TAIL)}`;
}
