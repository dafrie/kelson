/**
 * The kelson mark, inline.
 *
 * Geometry is docs/design/assets/kelson-mark.svg verbatim — 120-unit grid,
 * hull arc r=38 on centre (60,56), keel to y=100, beam spanning 60 units at
 * y=84. It is a component rather than an `<img>` so the stroke colour can
 * follow context; the default is the brand green the asset ships with.
 *
 * The "use favicon geometry below 24px" rule in docs/design/assets/README.md
 * does not bite here: the header renders it at 21px, but that rule is about
 * raster favicons at 16/32px, and stroke weight is compensated at call sites
 * the way the dashboard mockup does (stroke 10 at 21px).
 */
export function KelsonMark({
  size = 21,
  color = "var(--kelson-green)",
  strokeWidth = 10,
}: {
  size?: number;
  color?: string;
  strokeWidth?: number;
}) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 120 120"
      fill="none"
      stroke={color}
      strokeWidth={strokeWidth}
      strokeLinecap="round"
      aria-hidden="true"
      focusable="false"
    >
      <path d="M22 26 L22 56 A38 38 0 0 0 98 56 L98 26" />
      <path d="M60 74 L60 100" />
      <path d="M30 84 L90 84" />
    </svg>
  );
}
