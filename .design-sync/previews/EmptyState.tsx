import { EmptyState } from "kelson-ui";

/** Empty with a next step spelled out. */
export function WithDetail() {
  return (
    <EmptyState title="No projects yet">
      kelson init writes a starter spec; kelson deploy puts it on the cluster.
    </EmptyState>
  );
}

/** Title alone. */
export function TitleOnly() {
  return <EmptyState title="No deployments in this environment" />;
}
