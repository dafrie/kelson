# kelson brand assets

Mark geometry: 120-unit grid, 8-unit stroke, round caps. Hull arc r=38 on centre (60,56);
keel drops to y=100; the kelson beam spans 60 units at y=84.

| File | Use |
| --- | --- |
| kelson-mark.svg | Primary mark, #0FA36B |
| kelson-mark-black.svg | Single-colour dark, #111318 |
| kelson-mark-white.svg | Reversed on dark or brand fill |
| kelson-mark-currentcolor.svg | Inline in HTML, inherits text colour |
| kelson-lockup.svg | Horizontal lockup, light background |
| kelson-lockup-reversed.svg | Horizontal lockup, dark background |
| kelson-favicon-32.svg | 32px: stroke 11, shortened keel, widened beam |
| kelson-favicon-16.svg | 16px: stroke 14, keel dropped, hull + beam only |

Colours

- Mark / primary: #0FA36B
- Dark UI surface: #0b0f13, panels #10171f, borders #1d2732
- Text: #e6ebf0 primary, #9aa7b4 secondary, #6f7b89 muted
- States: synced #0FA36B, reconciling #4EA3FF, degraded #E0A944, failed #E2543A, suspended #6F7B89

Type

- Space Grotesk 400/500/600 — UI and headings, -2% to -3% tracking on display sizes
- IBM Plex Mono 400/500 — code, shas, labels, metadata

Clear space

x = 30 grid units (stroke x 3.75) on all four sides, measured from the mark's painted bounds.
Below 24px use the favicon geometry, not the scaled primary mark.

Lockup note: the lockup SVGs use a <text> element and need Space Grotesk available.
Convert text to outlines before shipping them where the font is not loaded.
