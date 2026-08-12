package git

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/go-git/go-billy/v5"
	billyutil "github.com/go-git/go-billy/v5/util"

	"github.com/dafrie/kelson/internal/delivery"
)

// ManifestFiles lays a rendered ManifestSet out as files. It is a pure
// function of the set: the same manifests always produce the same file names
// and contents, so a re-render with no spec change produces no diff.
//
// The name carries the renderer's apply order as a zero-padded prefix. Neither
// Flux nor Argo needs that ordering (both sort resources themselves), but it
// makes the directory readable in review and keeps `git diff` stable when a
// resource is added in the middle.
//
// This is packaging, not rendering: the manifest bytes are passed through
// verbatim. An adapter must never modify them (ADR-0001).
func ManifestFiles(set delivery.ManifestSet) []File {
	files := make([]File, 0, len(set.Manifests))
	seen := map[string]int{}
	for i, m := range set.Manifests {
		name := fmt.Sprintf("%03d-%s-%s.yaml", i+1, slugify(m.Kind), slugify(m.Name))
		if n := seen[name]; n > 0 {
			// Same kind+name in two namespaces: keep both, deterministically.
			name = fmt.Sprintf("%03d-%s-%s-%s.yaml", i+1, slugify(m.Kind), slugify(m.Namespace), slugify(m.Name))
		}
		seen[name]++
		files = append(files, File{Path: name, Data: m.YAML})
	}
	return files
}

var slugUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	out := strings.Trim(slugUnsafe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if out == "" {
		return "resource"
	}
	return out
}

// writeFile writes through billy, creating parent directories as needed.
func writeFile(fs billy.Filesystem, p string, data []byte) error {
	if dir := path.Dir(p); dir != "." && dir != "/" {
		if err := fs.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return billyutil.WriteFile(fs, p, data, 0o644)
}
