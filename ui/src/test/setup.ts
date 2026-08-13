import { afterEach } from "vitest";
import { cleanup } from "@testing-library/react";

// Testing Library only registers its own auto-cleanup when a global `afterEach`
// exists. Vitest is configured with `globals: false` (tests import what they
// use), so the teardown is wired here instead — without it every render leaks
// into the next test's document and role queries start matching twice.
afterEach(cleanup);
