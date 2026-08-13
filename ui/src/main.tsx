import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";

// Fonts are self-hosted. The mockups load Space Grotesk and IBM Plex Mono from
// the Google Fonts CDN; an app that ships inside a cluster — possibly an
// air-gapped one — must not depend on a third-party origin to render its own
// type, and must not leak a request per page load to one either.
import "@fontsource/space-grotesk/400.css";
import "@fontsource/space-grotesk/500.css";
import "@fontsource/space-grotesk/600.css";
import "@fontsource/ibm-plex-mono/400.css";
import "@fontsource/ibm-plex-mono/500.css";

import "./styles/tokens.css";
import "./styles/base.css";
import { App } from "./App";

const root = document.getElementById("root");
if (!root) {
  throw new Error("index.html is missing #root");
}

createRoot(root).render(
  <StrictMode>
    <BrowserRouter>
      <App />
    </BrowserRouter>
  </StrictMode>,
);
