package model

// GitSource is a repository the instance offers to every project (ADR-0035
// decision 2). It is the fourth document kind and the second one that is not an
// authoring document: like a GitConnection it is control-plane configuration,
// nothing about it is rendered, and the renderer never sees one.
//
// It carries exactly what a Project's `sources:` entry carries — where the code
// is, at which ref, through which connection — with the name coming from
// metadata.name, which is what a component binds to. A project-local source of
// the same name shadows it: a global name is a convenience, not a claim
// (ADR-0035 decision 3).
//
// It is deliberately dumber than a GitConnection: data, not credentials. There
// is no status beyond validation, because the question a status would answer —
// can this actually be reached — belongs to the connection that serves it, which
// is where the credential is and where `Reachable` already lives (ADR-0033
// decision 1).
type GitSource struct {
	TypeMeta `yaml:",inline"`
	Metadata ObjectMeta    `yaml:"metadata" json:"metadata" jsonschema:"required"`
	Spec     GitSourceSpec `yaml:"spec" json:"spec" jsonschema:"required"`
}

type GitSourceSpec struct {
	// Git is the repository URL, exactly as a Project's own source carries it.
	Git string `yaml:"git" json:"git" jsonschema:"required,format=uri,description=git URL of the repository this source offers"`

	// Ref is the branch, tag or commit this source is read at. It is per source
	// rather than per project, which is the whole of what a source adds over a
	// repository URL (ADR-0035 decision 1).
	Ref string `yaml:"ref,omitempty" json:"ref,omitempty" jsonschema:"default=main"`

	// Connection names the GitConnection kelson authenticates to this
	// repository with, and means here what it means on a Project's source: empty
	// is the common case, because the connection is chosen by matching this
	// URL's host against the connections the instance holds (ADR-0033
	// decision 4).
	Connection string `yaml:"connection,omitempty" json:"connection,omitempty" jsonschema:"description=name of the GitConnection to authenticate with; resolved by host match against the instance's connections when omitted"`

	// Owner is who may edit this source: the same discriminated reference
	// ADR-0033 decision 6 fixed for connections, reused rather than re-designed.
	// Nil means the instance owns it, which is the only answer enforced today —
	// use is granted by visibility and mutation by ownership, and until
	// principals exist (#231) there is no subject to enforce mutation against.
	Owner *ConnectionOwner `yaml:"owner,omitempty" json:"owner,omitempty"`
}

// AsSource is this global source as the resolver takes it: a [Source] named by
// the document's own metadata.name.
//
// The name lives in metadata rather than in the spec because that is where a
// custom resource's name lives, and a second copy inside the spec would be a
// name that could disagree with the one `kubectl get` prints. It is converted
// here, once, so every plane that lists GitSources hands [Resolve] the same
// thing (ADR-0035 decision 3).
func (g *GitSource) AsSource() Source {
	return Source{
		Name:       g.Metadata.Name,
		Git:        g.Spec.Git,
		Ref:        g.Spec.Ref,
		Connection: g.Spec.Connection,
	}
}

// IsInstanceOwned reports whether this source belongs to the instance rather
// than to a principal. A nil owner is instance-owned, for the reason a
// connection's is: an instance-wide source is the day-one experience — one
// person declares the platform monorepo and every project may build from it.
func (s GitSourceSpec) IsInstanceOwned() bool {
	return s.Owner == nil || s.Owner.Kind == OwnerInstance
}
