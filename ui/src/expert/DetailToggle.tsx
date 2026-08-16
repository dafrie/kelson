import "./expert.css";
import { setDetail, useDetail } from "./preference";

/**
 * The Kubernetes-detail control, in the header beside the theme (#260).
 *
 * It sits there because that is where display preferences belong: the two
 * things in this bar that change how the page *looks* to this browser and
 * nothing about what the server does. Putting it on a page — a per-screen
 * "advanced" switch — would have made it a mode of that screen, which is
 * exactly what it is not.
 *
 * Shaped like `ThemeToggle` and for the same reason: monochrome, borderless,
 * no fill, 11.5px. A large switch in a 56px bar would announce a mode of
 * operation and outweigh the nav, and the Console's rule is that saturated
 * colour is status and nothing else. On is marked by the ink coming up and a
 * hairline ring arriving — the same "quiet tier" grammar the status badge uses
 * for a state that is notable without being wrong.
 *
 * The visible text is `k8s`, which is the shortest true name for what the
 * preference adds and is a token rather than a sentence. The accessible name
 * and the tooltip carry the whole of it, including what pressing it will do,
 * because a three-character label cannot.
 *
 * `aria-pressed` rather than a checkbox: it is a control with two states that
 * takes effect immediately, not a form field awaiting a submit.
 */
export function DetailToggle() {
  const detail = useDetail();
  const label = detail
    ? "Kubernetes detail: on. Hide it."
    : "Kubernetes detail: off. Show it.";

  return (
    <button
      type="button"
      className="k-detail-toggle"
      onClick={() => setDetail(!detail)}
      aria-pressed={detail}
      aria-label={label}
      title={label}
    >
      k8s
    </button>
  );
}
