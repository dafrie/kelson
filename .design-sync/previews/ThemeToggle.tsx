import { ThemeToggle } from "kelson-ui";

/**
 * The one theme control: click cycles light -> dark -> system. Fresh storage
 * boots in system mode, so the "auto" mark shows beside the current icon.
 */
export function Toggle() {
  return <ThemeToggle />;
}
