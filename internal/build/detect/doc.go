// Package detect chooses the build strategy for a source tree and, crucially,
// reports WHY it chose it (issue #50, ADR-0010).
//
// Detection is the "auto" resolution path behind build.strategy. The
// precedence is fixed by ADR-0010 and is not re-litigated here:
//
//  1. an explicit build.strategy in the spec always wins — none means
//     "use image:, build nothing";
//  2. a Dockerfile in the repository (or at build.dockerfile) selects
//     dockerfile;
//  3. otherwise buildpacks.
//
// The result is a Detection, not a bare strategy id. The strategy, a typed
// Reason code, the Evidence path that decided it and a Message built from those
// parts are returned together, so an agent can answer "why this strategy?"
// from typed fields alone instead of parsing prose.
//
// Detection reads an fs.FS: the caller hands it the build context as a root.
// That context need not be a repository root, so a monorepo sub-tree is just a
// smaller root. Using fs.FS keeps tests free of temp directories and I/O
// (fstest.MapFS) and keeps this package free of the ambient filesystem.
package detect
