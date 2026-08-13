package registry

import (
	"fmt"
	"regexp"
	"strings"
)

// Repository derives the repository a Project's build pushes to, from an
// operator-supplied registry prefix and the project name:
//
//	Repository("ghcr.io/acme", "shop") → "ghcr.io/acme/shop"
//
// # Why the prefix is not in the spec
//
// Where images are pushed is infrastructure configuration, not application
// description: the same spec deploys to a cluster whose registry is ghcr.io
// and to one whose registry is a cluster-internal localhost:5000, and neither
// belongs in a file the application team owns. ADR-0010 already forbids build
// configuration in the spec; this is the same boundary seen from the registry
// side, which is why the prefix arrives as a flag/env (--registry,
// KELSON_REGISTRY) instead.
//
// # Why one repository per Project, not per Application
//
// The source is project-level (Project.spec.source) and so is the build. Model
// rule P3 resolves an Application's image to its own image: if it has one,
// otherwise the Project's — so every application that does not name its own
// image shares exactly one built image. One build, one repository, one digest
// pinned into every workload that shares it.
//
// The returned repository is fully qualified (the registry host is always
// explicit, see Ref.String) and carries neither tag nor digest: it is what
// build.Request.Image takes, with Tag and the pushed digest applied on top.
func Repository(prefix, project string) (string, error) {
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		return "", fmt.Errorf("registry: a destination registry is required, e.g. ghcr.io/acme (--registry or KELSON_REGISTRY)")
	}
	project = strings.TrimSpace(project)
	if project == "" {
		return "", fmt.Errorf("registry: a project name is required to derive the destination repository")
	}
	// A tag or digest on the prefix would not fail — it would silently become
	// part of the namespace and push somewhere nobody asked for. Reject it.
	if strings.Contains(prefix, "@") {
		return "", fmt.Errorf("registry: destination registry %q must not carry a digest; pass the repository prefix only, e.g. ghcr.io/acme", prefix)
	}
	if err := validatePrefix(prefix); err != nil {
		return "", err
	}
	if !repoComponent.MatchString(project) {
		return "", fmt.Errorf("registry: project name %q is not a valid repository path component", project)
	}

	ref, err := Parse(prefix + "/" + project)
	if err != nil {
		return "", err
	}
	return ref.String(), nil
}

// repoComponent is the distribution grammar for one repository path component:
// lowercase alphanumerics, separated by a period, one or two underscores, or
// one or more dashes. It is what rejects "ACME", "acme:v1" and an empty
// segment before they turn into a push to the wrong place.
var repoComponent = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*$`)

// registryHost is a host with an optional port. It is deliberately loose about
// what a hostname may contain (any DNS label, or an IP) and strict about the
// port, because a mistyped port is the failure that produces a confusing
// connection error rather than a clear one.
var registryHost = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9.-]*[a-zA-Z0-9])?(:[0-9]{1,5})?$`)

// validatePrefix checks the registry prefix component by component, using the
// same "is the first component a host?" rule Parse uses so the two cannot
// disagree about where the host ends and the namespace begins.
func validatePrefix(prefix string) error {
	// Parse sees prefix+"/"+project, so its first component is always followed
	// by a slash — the host test here is applied the same way, with or without
	// a slash inside the prefix itself ("localhost:5000" is a whole prefix).
	first, rest, _ := strings.Cut(prefix, "/")
	path := prefix
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		if !registryHost.MatchString(first) {
			return fmt.Errorf("registry: %q is not a valid registry host in destination %q", first, prefix)
		}
		path = rest
	}
	if path == "" {
		return nil // the prefix named a registry host and nothing else
	}
	for _, component := range strings.Split(path, "/") {
		if !repoComponent.MatchString(component) {
			return fmt.Errorf("registry: %q is not a valid repository path component in destination %q", component, prefix)
		}
	}
	return nil
}
