// GitConnectionService: the forge credentials kelson holds, over the API
// (ADR-0033).
//
// A `GitConnection` is a CRD in `kelson-system` naming a forge, a host and the
// Secret that holds the material: the CR carries identifiers and references,
// the Secret carries the credential (ADR-0033 decision 1). This service is that
// object over the wire plus one live probe — what the connection list, the
// connection health surface and the create-from-git path read, without a
// kubeconfig and without the CLI.
//
// # No RPC in this file can carry a credential value
//
// `secret_ref` is a Secret *name* wherever it appears — in the request that
// creates a connection and in every connection this service reports back — and
// there is no field here a token, a private key or a webhook secret could
// arrive in or leave through, in either direction. That is ADR-0009's rule
// stated as a schema property rather than as a handler's promise (ADR-0033
// decision 1): a client that wanted the material could not decode it, and a
// future handler that tried to send it would have nowhere to put it.
//
// What travels instead is references and provider-reported fact: which forge,
// which host, which Secret, and then what the *provider* said when the server
// last asked it — the account the credential acts as, how many repositories it
// can see, whether the forge answered at all.
//
// The material a connection references is written before the connection names
// it, by SecretService or by `kubectl`. The short-lived installation tokens of
// ADR-0033 decision 2 never appear here either: they are minted per operation,
// server-side, registered with internal/redact the moment they exist, and
// consumed by the build pod and the forge client — never by a client of this
// schema.
//
// # GitHub App connections are not created by this service
//
// CreateConnection makes token connections and nothing else. An app connection
// is the product of GitHub's app-manifest flow (ADR-0033 decision 2), which is
// a browser redirect to GitHub, a one-time code posted back to
// `<server>/forge/github/manifest/callback`, and an exchange that yields the
// app ID, the private key and the webhook secret — after which the server
// writes the Secret and the CR itself. The installation ID arrives later still,
// on the `installation` webhook event.
//
// None of that is a shape an RPC has: the credential is minted by GitHub and
// handed to the *server*, and a CreateConnection that accepted an app ID and a
// private key would be exactly the credential-carrying request the section
// above says does not exist. So GIT_AUTH_KIND_GITHUB_APP is a value this
// service reports and never a value it accepts, and the HTTP callback pair is
// the only way an app connection comes into being.
//
// # Repository listing is on the wire, and it refuses rather than lies
//
// Browsing repositories is `RepoBrowser`, an *optional* capability in ADR-0033
// decision 3: the GitHub adapter has it and a `generic` token connection does
// not. An earlier cut of this file left the listing off the wire entirely,
// because "an RPC every connection answered would have to lie for the ones that
// cannot — an empty list is indistinguishable from 'this forge has no
// browser'". That objection is about the *answer*, not about the RPC, and the
// two calls below settle it by making the capability part of the vocabulary:
// ListConnectionRepositories and ListConnectionBranches are served by a
// connection whose provider implements the browser, and refused by one whose
// provider does not — with the structured code `connection/capability-unsupported`
// rather than with an empty list. A client can therefore tell "nothing here"
// from "this forge cannot be asked", which is the whole of what was missing.
//
// The refusal is not an error state of the connection. A `generic` token
// connection that cannot list repositories still mints credentials and clones
// private ones, which is the only capability a deploy needs (ADR-0033
// decision 3: "absence degrades the UI, never the deploy"), so the refusal says
// what the connection *can* do and points at the pasted-URL path, which works
// for every forge and every auth kind. A client that meets it falls back to
// that field rather than treating the connection as broken.
//
// `repositories` on GitConnection stays what it was: a count the provider
// reported at the last probe, not a page of this listing.
//
// # Deliberately omitted, and why
//
// **No UpdateConnection.** Rotating a credential is writing the Secret the
// connection already names, which is SecretService's call (or `kubectl`'s) and
// leaves the connection unchanged — the connection points at a name, and the
// name did not move. Changing the Secret a connection points at, or its host,
// changes what the connection *is* and which repositories it reaches; delete
// and create says so, where a field-level edit would let a connection quietly
// become a different one under projects already resolving through it
// (ADR-0033 decision 4).
//
// **No credential read-back, and no GetConnectionSecret.** ADR-0009's masked
// read-back applies to forge credentials unchanged; ADR-0033 amends what kelson
// *does* with them, not what it will show. Reading the material is
// `kubectl get secret` and the cluster's own RBAC and audit trail, exactly as
// secret.proto records for the same question.

// Code generated by protoc-gen-go. DO NOT EDIT.
// versions:
// 	protoc-gen-go v1.36.12
// 	protoc        (unknown)
// source: kelson/v1alpha1/gitconnection.proto

package kelsonv1alpha1

import (
	protoreflect "google.golang.org/protobuf/reflect/protoreflect"
	protoimpl "google.golang.org/protobuf/runtime/protoimpl"
	reflect "reflect"
	sync "sync"
	unsafe "unsafe"
)

const (
	// Verify that this generated code is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(20 - protoimpl.MinVersion)
	// Verify that runtime/protoimpl is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(protoimpl.MaxVersion - 20)
)

// GitAuthKind is which of ADR-0033 decision 2's two auth shapes a connection
// carries. The CR spells it as exactly-one-of `auth.githubApp` / `auth.token`;
// on the wire it is a discriminator plus the fields each shape populates,
// because a client renders "GitHub App · acme (42 repositories)" from the same
// message whichever shape answered.
type GitAuthKind int32

const (
	GitAuthKind_GIT_AUTH_KIND_UNSPECIFIED GitAuthKind = 0
	// A per-instance GitHub App created by the manifest flow. Short-lived
	// installation tokens are minted from its key per operation; the key itself
	// never leaves the instance that minted it.
	GitAuthKind_GIT_AUTH_KIND_GITHUB_APP GitAuthKind = 1
	// A PAT, project token or deploy token in a Secret — the universal fallback,
	// and the only path for a forge whose adapter has not landed.
	GitAuthKind_GIT_AUTH_KIND_TOKEN GitAuthKind = 2
)

// Enum value maps for GitAuthKind.
var (
	GitAuthKind_name = map[int32]string{
		0: "GIT_AUTH_KIND_UNSPECIFIED",
		1: "GIT_AUTH_KIND_GITHUB_APP",
		2: "GIT_AUTH_KIND_TOKEN",
	}
	GitAuthKind_value = map[string]int32{
		"GIT_AUTH_KIND_UNSPECIFIED": 0,
		"GIT_AUTH_KIND_GITHUB_APP":  1,
		"GIT_AUTH_KIND_TOKEN":       2,
	}
)

func (x GitAuthKind) Enum() *GitAuthKind {
	p := new(GitAuthKind)
	*p = x
	return p
}

func (x GitAuthKind) String() string {
	return protoimpl.X.EnumStringOf(x.Descriptor(), protoreflect.EnumNumber(x))
}

func (GitAuthKind) Descriptor() protoreflect.EnumDescriptor {
	return file_kelson_v1alpha1_gitconnection_proto_enumTypes[0].Descriptor()
}

func (GitAuthKind) Type() protoreflect.EnumType {
	return &file_kelson_v1alpha1_gitconnection_proto_enumTypes[0]
}

func (x GitAuthKind) Number() protoreflect.EnumNumber {
	return protoreflect.EnumNumber(x)
}

// Deprecated: Use GitAuthKind.Descriptor instead.
func (GitAuthKind) EnumDescriptor() ([]byte, []int) {
	return file_kelson_v1alpha1_gitconnection_proto_rawDescGZIP(), []int{0}
}

// GitOwnerKind is the discriminator of ADR-0033 decision 6's ownership
// reference.
type GitOwnerKind int32

const (
	GitOwnerKind_GIT_OWNER_KIND_UNSPECIFIED GitOwnerKind = 0
	// Owned by the instance: the day-one shape, and the only one enforced today.
	GitOwnerKind_GIT_OWNER_KIND_INSTANCE GitOwnerKind = 1
	// Reserved for tenancy (#231). Stored, shown and validated; not a boundary.
	GitOwnerKind_GIT_OWNER_KIND_USER GitOwnerKind = 2
	GitOwnerKind_GIT_OWNER_KIND_TEAM GitOwnerKind = 3
)

// Enum value maps for GitOwnerKind.
var (
	GitOwnerKind_name = map[int32]string{
		0: "GIT_OWNER_KIND_UNSPECIFIED",
		1: "GIT_OWNER_KIND_INSTANCE",
		2: "GIT_OWNER_KIND_USER",
		3: "GIT_OWNER_KIND_TEAM",
	}
	GitOwnerKind_value = map[string]int32{
		"GIT_OWNER_KIND_UNSPECIFIED": 0,
		"GIT_OWNER_KIND_INSTANCE":    1,
		"GIT_OWNER_KIND_USER":        2,
		"GIT_OWNER_KIND_TEAM":        3,
	}
)

func (x GitOwnerKind) Enum() *GitOwnerKind {
	p := new(GitOwnerKind)
	*p = x
	return p
}

func (x GitOwnerKind) String() string {
	return protoimpl.X.EnumStringOf(x.Descriptor(), protoreflect.EnumNumber(x))
}

func (GitOwnerKind) Descriptor() protoreflect.EnumDescriptor {
	return file_kelson_v1alpha1_gitconnection_proto_enumTypes[1].Descriptor()
}

func (GitOwnerKind) Type() protoreflect.EnumType {
	return &file_kelson_v1alpha1_gitconnection_proto_enumTypes[1]
}

func (x GitOwnerKind) Number() protoreflect.EnumNumber {
	return protoreflect.EnumNumber(x)
}

// Deprecated: Use GitOwnerKind.Descriptor instead.
func (GitOwnerKind) EnumDescriptor() ([]byte, []int) {
	return file_kelson_v1alpha1_gitconnection_proto_rawDescGZIP(), []int{1}
}

// GitOwner is who may edit, rotate and delete a connection — ADR-0033
// decision 6's model, recorded now so tenancy attaches to it rather than
// migrating it.
//
// # Display-only until #231
//
// The rule this field encodes is "use is granted by visibility, mutation by
// ownership": any project that can see a connection may build and preview
// through it, and editing it belongs to its owner and to instance admins. There
// is no principal to enforce that against yet — the interim shared password
// (ADR-0013 §3) says a caller may reach the server and never says who it is —
// so today every connection is effectively instance-scoped whatever this says.
//
// A client must not render it as a boundary. Showing "owned by alice" beside a
// connection every operator can delete is authorization theatre, which is the
// exact failure ADR-0031 refuses; ADR-0033 decision 6 asks for copy that says
// "visible to everyone on this instance" until it is not. Enforcement arrives
// with the subject it needs (#231), and the semantics do not change when it
// does — that is what fixing them here bought.
type GitOwner struct {
	state protoimpl.MessageState `protogen:"open.v1"`
	Kind  GitOwnerKind           `protobuf:"varint,1,opt,name=kind,proto3,enum=kelson.v1alpha1.GitOwnerKind" json:"kind,omitempty"`
	// Set when kind is USER or TEAM; empty for INSTANCE. Reserved until #231.
	Name          string `protobuf:"bytes,2,opt,name=name,proto3" json:"name,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *GitOwner) Reset() {
	*x = GitOwner{}
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[0]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *GitOwner) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*GitOwner) ProtoMessage() {}

func (x *GitOwner) ProtoReflect() protoreflect.Message {
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[0]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use GitOwner.ProtoReflect.Descriptor instead.
func (*GitOwner) Descriptor() ([]byte, []int) {
	return file_kelson_v1alpha1_gitconnection_proto_rawDescGZIP(), []int{0}
}

func (x *GitOwner) GetKind() GitOwnerKind {
	if x != nil {
		return x.Kind
	}
	return GitOwnerKind_GIT_OWNER_KIND_UNSPECIFIED
}

func (x *GitOwner) GetName() string {
	if x != nil {
		return x.Name
	}
	return ""
}

// GitConnection is one connection as the server holds it: references and
// provider-reported facts, and nothing else.
//
// The first block is what the CR declares. The second is what the controller
// observed by asking the forge — populated only once a probe has run, and
// `message` is what says which case an empty answer is.
type GitConnection struct {
	state protoimpl.MessageState `protogen:"open.v1"`
	// The CR's metadata.name in kelson-system, and the string
	// `Project.spec.source.connection` names when it disambiguates explicitly
	// (ADR-0033 decision 4). A DNS-1123 label.
	Name string `protobuf:"bytes,1,opt,name=name,proto3" json:"name,omitempty"`
	// Which adapter serves this connection: "github" today, "generic" for a bare
	// token against any host. A string rather than an enum, matching
	// PreviewSettings.provider: the vocabulary of record is internal/forge's
	// provider registry, and it grows by writing an adapter, not by editing this
	// file (ADR-0033 decision 3).
	Provider string `protobuf:"bytes,2,opt,name=provider,proto3" json:"provider,omitempty"`
	// The forge base URL, e.g. https://github.com. Self-hosted GHE, GitLab and
	// Forgejo instances set their own; it is also half of the host-then-owner
	// match that resolves a project's source to a connection.
	Host     string      `protobuf:"bytes,3,opt,name=host,proto3" json:"host,omitempty"`
	Owner    *GitOwner   `protobuf:"bytes,4,opt,name=owner,proto3" json:"owner,omitempty"`
	AuthKind GitAuthKind `protobuf:"varint,5,opt,name=auth_kind,json=authKind,proto3,enum=kelson.v1alpha1.GitAuthKind" json:"auth_kind,omitempty"`
	// The GitHub App's numeric identifiers, zero unless auth_kind is
	// GITHUB_APP. installation_id stays zero between the manifest callback and
	// the `installation` webhook event that reports it — a connection created but
	// not yet installed on any repository, which is a real state a client shows
	// rather than an error.
	AppId          int64 `protobuf:"varint,6,opt,name=app_id,json=appId,proto3" json:"app_id,omitempty"`
	InstallationId int64 `protobuf:"varint,7,opt,name=installation_id,json=installationId,proto3" json:"installation_id,omitempty"`
	// Name of the Secret in kelson-system holding this connection's material:
	// `privateKey`/`webhookSecret` for an app, `token`/`username` for a token.
	// A reference, never a value (ADR-0009, ADR-0033 decision 1).
	SecretRef string `protobuf:"bytes,8,opt,name=secret_ref,json=secretRef,proto3" json:"secret_ref,omitempty"`
	// Who the credential acts as, as the provider reported it — the app's
	// installation account, or the token's own login. Empty when no probe has
	// succeeded yet. It is the field that answers "is this the account I think it
	// is" without anyone reading the credential to find out.
	Account string `protobuf:"bytes,9,opt,name=account,proto3" json:"account,omitempty"`
	// How many repositories the credential can see, as the provider reported it.
	// Zero for an installation with no repositories selected and zero for a
	// connection never probed; `message` is what separates them.
	Repositories int32 `protobuf:"varint,10,opt,name=repositories,proto3" json:"repositories,omitempty"`
	// The CR's Ready condition: the connection is well-formed and its Secret
	// exists with the keys its auth kind needs.
	Ready bool `protobuf:"varint,11,opt,name=ready,proto3" json:"ready,omitempty"`
	// The CR's Reachable condition: the last probe authenticated against the
	// forge and got an answer.
	Reachable bool `protobuf:"varint,12,opt,name=reachable,proto3" json:"reachable,omitempty"`
	// Why the conditions say what they say, in prose — a missing Secret, a
	// revoked installation, an expired token, or "not yet observed" for a
	// connection no probe has reached. Both booleans are false before the first
	// probe as well as after a failed one, so a client that renders health
	// without this field cannot tell "broken" from "not looked at yet"
	// (ADR-0033: a new build-failure cause is only worth having if it is
	// diagnosable).
	Message       string `protobuf:"bytes,13,opt,name=message,proto3" json:"message,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *GitConnection) Reset() {
	*x = GitConnection{}
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[1]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *GitConnection) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*GitConnection) ProtoMessage() {}

func (x *GitConnection) ProtoReflect() protoreflect.Message {
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[1]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use GitConnection.ProtoReflect.Descriptor instead.
func (*GitConnection) Descriptor() ([]byte, []int) {
	return file_kelson_v1alpha1_gitconnection_proto_rawDescGZIP(), []int{1}
}

func (x *GitConnection) GetName() string {
	if x != nil {
		return x.Name
	}
	return ""
}

func (x *GitConnection) GetProvider() string {
	if x != nil {
		return x.Provider
	}
	return ""
}

func (x *GitConnection) GetHost() string {
	if x != nil {
		return x.Host
	}
	return ""
}

func (x *GitConnection) GetOwner() *GitOwner {
	if x != nil {
		return x.Owner
	}
	return nil
}

func (x *GitConnection) GetAuthKind() GitAuthKind {
	if x != nil {
		return x.AuthKind
	}
	return GitAuthKind_GIT_AUTH_KIND_UNSPECIFIED
}

func (x *GitConnection) GetAppId() int64 {
	if x != nil {
		return x.AppId
	}
	return 0
}

func (x *GitConnection) GetInstallationId() int64 {
	if x != nil {
		return x.InstallationId
	}
	return 0
}

func (x *GitConnection) GetSecretRef() string {
	if x != nil {
		return x.SecretRef
	}
	return ""
}

func (x *GitConnection) GetAccount() string {
	if x != nil {
		return x.Account
	}
	return ""
}

func (x *GitConnection) GetRepositories() int32 {
	if x != nil {
		return x.Repositories
	}
	return 0
}

func (x *GitConnection) GetReady() bool {
	if x != nil {
		return x.Ready
	}
	return false
}

func (x *GitConnection) GetReachable() bool {
	if x != nil {
		return x.Reachable
	}
	return false
}

func (x *GitConnection) GetMessage() string {
	if x != nil {
		return x.Message
	}
	return ""
}

type ListConnectionsRequest struct {
	state         protoimpl.MessageState `protogen:"open.v1"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *ListConnectionsRequest) Reset() {
	*x = ListConnectionsRequest{}
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[2]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *ListConnectionsRequest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*ListConnectionsRequest) ProtoMessage() {}

func (x *ListConnectionsRequest) ProtoReflect() protoreflect.Message {
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[2]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use ListConnectionsRequest.ProtoReflect.Descriptor instead.
func (*ListConnectionsRequest) Descriptor() ([]byte, []int) {
	return file_kelson_v1alpha1_gitconnection_proto_rawDescGZIP(), []int{2}
}

type ListConnectionsResponse struct {
	state protoimpl.MessageState `protogen:"open.v1"`
	// Every connection the instance holds, sorted by name. Not filtered by
	// ownership: ADR-0033 decision 6 grants *use* by visibility, so a project can
	// resolve through any of them and a client that hid some would be describing
	// a different instance than the one that builds.
	Connections   []*GitConnection `protobuf:"bytes,1,rep,name=connections,proto3" json:"connections,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *ListConnectionsResponse) Reset() {
	*x = ListConnectionsResponse{}
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[3]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *ListConnectionsResponse) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*ListConnectionsResponse) ProtoMessage() {}

func (x *ListConnectionsResponse) ProtoReflect() protoreflect.Message {
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[3]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use ListConnectionsResponse.ProtoReflect.Descriptor instead.
func (*ListConnectionsResponse) Descriptor() ([]byte, []int) {
	return file_kelson_v1alpha1_gitconnection_proto_rawDescGZIP(), []int{3}
}

func (x *ListConnectionsResponse) GetConnections() []*GitConnection {
	if x != nil {
		return x.Connections
	}
	return nil
}

type GetConnectionRequest struct {
	state         protoimpl.MessageState `protogen:"open.v1"`
	Name          string                 `protobuf:"bytes,1,opt,name=name,proto3" json:"name,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *GetConnectionRequest) Reset() {
	*x = GetConnectionRequest{}
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[4]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *GetConnectionRequest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*GetConnectionRequest) ProtoMessage() {}

func (x *GetConnectionRequest) ProtoReflect() protoreflect.Message {
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[4]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use GetConnectionRequest.ProtoReflect.Descriptor instead.
func (*GetConnectionRequest) Descriptor() ([]byte, []int) {
	return file_kelson_v1alpha1_gitconnection_proto_rawDescGZIP(), []int{4}
}

func (x *GetConnectionRequest) GetName() string {
	if x != nil {
		return x.Name
	}
	return ""
}

type GetConnectionResponse struct {
	state         protoimpl.MessageState `protogen:"open.v1"`
	Connection    *GitConnection         `protobuf:"bytes,1,opt,name=connection,proto3" json:"connection,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *GetConnectionResponse) Reset() {
	*x = GetConnectionResponse{}
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[5]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *GetConnectionResponse) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*GetConnectionResponse) ProtoMessage() {}

func (x *GetConnectionResponse) ProtoReflect() protoreflect.Message {
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[5]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use GetConnectionResponse.ProtoReflect.Descriptor instead.
func (*GetConnectionResponse) Descriptor() ([]byte, []int) {
	return file_kelson_v1alpha1_gitconnection_proto_rawDescGZIP(), []int{5}
}

func (x *GetConnectionResponse) GetConnection() *GitConnection {
	if x != nil {
		return x.Connection
	}
	return nil
}

// CreateConnectionRequest creates a token-auth connection. There is no app
// variant of this message; see this file's header for why the manifest flow's
// HTTP callback owns that path instead.
type CreateConnectionRequest struct {
	state protoimpl.MessageState `protogen:"open.v1"`
	// The connection's name, a DNS-1123 label, and the CR's metadata.name.
	Name string `protobuf:"bytes,1,opt,name=name,proto3" json:"name,omitempty"`
	// "github" for github.com or GHE with a PAT, "generic" for any other host.
	Provider string `protobuf:"bytes,2,opt,name=provider,proto3" json:"provider,omitempty"`
	// The forge base URL. Required: host is what resolves a project's source to
	// this connection, and a default would silently claim github.com.
	Host string `protobuf:"bytes,3,opt,name=host,proto3" json:"host,omitempty"`
	// Name of an existing Secret in kelson-system holding `token` (and
	// optionally `username`). The caller creates it out of band or through
	// SecretService, and this request carries the name it chose — never the
	// token. A connection naming a Secret that does not exist is created and
	// reports Ready false with the reason, rather than being refused: the two
	// objects have separate lifecycles and either order must work.
	SecretRef string `protobuf:"bytes,4,opt,name=secret_ref,json=secretRef,proto3" json:"secret_ref,omitempty"`
	// Defaults to INSTANCE when unset, which is the only kind enforced today.
	Owner *GitOwner `protobuf:"bytes,5,opt,name=owner,proto3" json:"owner,omitempty"`
	// RENDER validates the request and touches nothing. SERVER is a real
	// Kubernetes server-side dry-run apply: admission runs against the live
	// object and nothing is persisted.
	DryRun DryRun `protobuf:"varint,6,opt,name=dry_run,json=dryRun,proto3,enum=kelson.v1alpha1.DryRun" json:"dry_run,omitempty"`
	// Unlike SetSecret, which explains at length why it carries no key, this
	// request does (#69). Both of secret.proto's reasons fail here: a create is
	// not convergent the way a merge-apply is — a replay after a timeout would
	// otherwise answer "already exists" where the first call answered "created",
	// which is a different outcome for the same operation — and there *is*
	// somewhere to record the key, because the GitConnection CR is kelson's own
	// object to annotate, exactly as the spec store annotates the ConfigMap it
	// writes.
	IdempotencyKey string `protobuf:"bytes,7,opt,name=idempotency_key,json=idempotencyKey,proto3" json:"idempotency_key,omitempty"`
	unknownFields  protoimpl.UnknownFields
	sizeCache      protoimpl.SizeCache
}

func (x *CreateConnectionRequest) Reset() {
	*x = CreateConnectionRequest{}
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[6]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *CreateConnectionRequest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*CreateConnectionRequest) ProtoMessage() {}

func (x *CreateConnectionRequest) ProtoReflect() protoreflect.Message {
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[6]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use CreateConnectionRequest.ProtoReflect.Descriptor instead.
func (*CreateConnectionRequest) Descriptor() ([]byte, []int) {
	return file_kelson_v1alpha1_gitconnection_proto_rawDescGZIP(), []int{6}
}

func (x *CreateConnectionRequest) GetName() string {
	if x != nil {
		return x.Name
	}
	return ""
}

func (x *CreateConnectionRequest) GetProvider() string {
	if x != nil {
		return x.Provider
	}
	return ""
}

func (x *CreateConnectionRequest) GetHost() string {
	if x != nil {
		return x.Host
	}
	return ""
}

func (x *CreateConnectionRequest) GetSecretRef() string {
	if x != nil {
		return x.SecretRef
	}
	return ""
}

func (x *CreateConnectionRequest) GetOwner() *GitOwner {
	if x != nil {
		return x.Owner
	}
	return nil
}

func (x *CreateConnectionRequest) GetDryRun() DryRun {
	if x != nil {
		return x.DryRun
	}
	return DryRun_DRY_RUN_UNSPECIFIED
}

func (x *CreateConnectionRequest) GetIdempotencyKey() string {
	if x != nil {
		return x.IdempotencyKey
	}
	return ""
}

type CreateConnectionResponse struct {
	state      protoimpl.MessageState `protogen:"open.v1"`
	Connection *GitConnection         `protobuf:"bytes,1,opt,name=connection,proto3" json:"connection,omitempty"`
	// True when nothing was persisted because the request was a dry run.
	DryRun        bool `protobuf:"varint,2,opt,name=dry_run,json=dryRun,proto3" json:"dry_run,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *CreateConnectionResponse) Reset() {
	*x = CreateConnectionResponse{}
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[7]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *CreateConnectionResponse) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*CreateConnectionResponse) ProtoMessage() {}

func (x *CreateConnectionResponse) ProtoReflect() protoreflect.Message {
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[7]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use CreateConnectionResponse.ProtoReflect.Descriptor instead.
func (*CreateConnectionResponse) Descriptor() ([]byte, []int) {
	return file_kelson_v1alpha1_gitconnection_proto_rawDescGZIP(), []int{7}
}

func (x *CreateConnectionResponse) GetConnection() *GitConnection {
	if x != nil {
		return x.Connection
	}
	return nil
}

func (x *CreateConnectionResponse) GetDryRun() bool {
	if x != nil {
		return x.DryRun
	}
	return false
}

type DeleteConnectionRequest struct {
	state protoimpl.MessageState `protogen:"open.v1"`
	Name  string                 `protobuf:"bytes,1,opt,name=name,proto3" json:"name,omitempty"`
	// SERVER asks the API server to validate the delete and discard it. RENDER
	// validates the request alone.
	DryRun         DryRun `protobuf:"varint,2,opt,name=dry_run,json=dryRun,proto3,enum=kelson.v1alpha1.DryRun" json:"dry_run,omitempty"`
	IdempotencyKey string `protobuf:"bytes,3,opt,name=idempotency_key,json=idempotencyKey,proto3" json:"idempotency_key,omitempty"`
	unknownFields  protoimpl.UnknownFields
	sizeCache      protoimpl.SizeCache
}

func (x *DeleteConnectionRequest) Reset() {
	*x = DeleteConnectionRequest{}
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[8]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *DeleteConnectionRequest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*DeleteConnectionRequest) ProtoMessage() {}

func (x *DeleteConnectionRequest) ProtoReflect() protoreflect.Message {
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[8]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use DeleteConnectionRequest.ProtoReflect.Descriptor instead.
func (*DeleteConnectionRequest) Descriptor() ([]byte, []int) {
	return file_kelson_v1alpha1_gitconnection_proto_rawDescGZIP(), []int{8}
}

func (x *DeleteConnectionRequest) GetName() string {
	if x != nil {
		return x.Name
	}
	return ""
}

func (x *DeleteConnectionRequest) GetDryRun() DryRun {
	if x != nil {
		return x.DryRun
	}
	return DryRun_DRY_RUN_UNSPECIFIED
}

func (x *DeleteConnectionRequest) GetIdempotencyKey() string {
	if x != nil {
		return x.IdempotencyKey
	}
	return ""
}

type DeleteConnectionResponse struct {
	state protoimpl.MessageState `protogen:"open.v1"`
	// False when the request was a dry run, which removes nothing.
	Deleted bool `protobuf:"varint,1,opt,name=deleted,proto3" json:"deleted,omitempty"`
	// Projects that resolve their source through this connection, by name. The
	// delete is not blocked by them — a connection is deleted because it is
	// wrong, and refusing until every project is edited would make a leaked
	// credential harder to revoke than to keep — but the builds that will start
	// failing are named rather than discovered later (ADR-0033: "one more thing
	// between a user and a build").
	AffectedProjects []string `protobuf:"bytes,2,rep,name=affected_projects,json=affectedProjects,proto3" json:"affected_projects,omitempty"`
	unknownFields    protoimpl.UnknownFields
	sizeCache        protoimpl.SizeCache
}

func (x *DeleteConnectionResponse) Reset() {
	*x = DeleteConnectionResponse{}
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[9]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *DeleteConnectionResponse) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*DeleteConnectionResponse) ProtoMessage() {}

func (x *DeleteConnectionResponse) ProtoReflect() protoreflect.Message {
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[9]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use DeleteConnectionResponse.ProtoReflect.Descriptor instead.
func (*DeleteConnectionResponse) Descriptor() ([]byte, []int) {
	return file_kelson_v1alpha1_gitconnection_proto_rawDescGZIP(), []int{9}
}

func (x *DeleteConnectionResponse) GetDeleted() bool {
	if x != nil {
		return x.Deleted
	}
	return false
}

func (x *DeleteConnectionResponse) GetAffectedProjects() []string {
	if x != nil {
		return x.AffectedProjects
	}
	return nil
}

type TestConnectionRequest struct {
	state         protoimpl.MessageState `protogen:"open.v1"`
	Name          string                 `protobuf:"bytes,1,opt,name=name,proto3" json:"name,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *TestConnectionRequest) Reset() {
	*x = TestConnectionRequest{}
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[10]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *TestConnectionRequest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*TestConnectionRequest) ProtoMessage() {}

func (x *TestConnectionRequest) ProtoReflect() protoreflect.Message {
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[10]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use TestConnectionRequest.ProtoReflect.Descriptor instead.
func (*TestConnectionRequest) Descriptor() ([]byte, []int) {
	return file_kelson_v1alpha1_gitconnection_proto_rawDescGZIP(), []int{10}
}

func (x *TestConnectionRequest) GetName() string {
	if x != nil {
		return x.Name
	}
	return ""
}

// TestConnectionResponse is one probe's answer. It is the same question the
// controller's Reachable condition answers, asked on demand.
type TestConnectionResponse struct {
	state protoimpl.MessageState `protogen:"open.v1"`
	// Whether the forge authenticated the credential and answered.
	Reachable bool `protobuf:"varint,1,opt,name=reachable,proto3" json:"reachable,omitempty"`
	// Who the credential acts as, per the provider. Empty when unreachable.
	Account string `protobuf:"bytes,2,opt,name=account,proto3" json:"account,omitempty"`
	// How many repositories the credential can see, per the provider. Zero when
	// unreachable, and zero for a real installation with nothing selected.
	Repositories int32 `protobuf:"varint,3,opt,name=repositories,proto3" json:"repositories,omitempty"`
	// The forge's refusal in prose when unreachable, and a one-line summary of
	// what answered when it is. This is the field an operator reads to tell a
	// revoked installation from a network path that does not exist.
	Message       string `protobuf:"bytes,4,opt,name=message,proto3" json:"message,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *TestConnectionResponse) Reset() {
	*x = TestConnectionResponse{}
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[11]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *TestConnectionResponse) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*TestConnectionResponse) ProtoMessage() {}

func (x *TestConnectionResponse) ProtoReflect() protoreflect.Message {
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[11]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use TestConnectionResponse.ProtoReflect.Descriptor instead.
func (*TestConnectionResponse) Descriptor() ([]byte, []int) {
	return file_kelson_v1alpha1_gitconnection_proto_rawDescGZIP(), []int{11}
}

func (x *TestConnectionResponse) GetReachable() bool {
	if x != nil {
		return x.Reachable
	}
	return false
}

func (x *TestConnectionResponse) GetAccount() string {
	if x != nil {
		return x.Account
	}
	return ""
}

func (x *TestConnectionResponse) GetRepositories() int32 {
	if x != nil {
		return x.Repositories
	}
	return 0
}

func (x *TestConnectionResponse) GetMessage() string {
	if x != nil {
		return x.Message
	}
	return ""
}

// GitRepository is one repository a connection can see, reduced to what a
// picker needs: a row to show, and the fields that fill in a Project's
// `spec.source`. It mirrors internal/forge's own Repo, which is deliberately
// this small — whatever else a forge reports about a repository is that forge's
// business and stops at the adapter.
type GitRepository struct {
	state protoimpl.MessageState `protogen:"open.v1"`
	// "owner/name" as the forge spells it, which is the key ListConnectionBranches
	// takes back.
	FullName string `protobuf:"bytes,1,opt,name=full_name,json=fullName,proto3" json:"full_name,omitempty"`
	// The browser URL, and the value that goes into `Project.spec.source.git`.
	// It is the forge's own, not one assembled from host and full_name: a
	// self-hosted instance serving repositories under a path prefix would have
	// the assembled one point at nothing.
	HtmlUrl string `protobuf:"bytes,2,opt,name=html_url,json=htmlUrl,proto3" json:"html_url,omitempty"`
	// The branch a clone lands on when nothing asks for another — what a picker
	// preselects, and what leaving `spec.source.ref` empty resolves to.
	DefaultBranch string `protobuf:"bytes,3,opt,name=default_branch,json=defaultBranch,proto3" json:"default_branch,omitempty"`
	// Whether reading it needs the credential at all. A picker shows it as a
	// badge; nothing else in kelson branches on it, because a connection that
	// can see a repository can clone it whichever this says.
	Private       bool `protobuf:"varint,4,opt,name=private,proto3" json:"private,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *GitRepository) Reset() {
	*x = GitRepository{}
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[12]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *GitRepository) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*GitRepository) ProtoMessage() {}

func (x *GitRepository) ProtoReflect() protoreflect.Message {
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[12]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use GitRepository.ProtoReflect.Descriptor instead.
func (*GitRepository) Descriptor() ([]byte, []int) {
	return file_kelson_v1alpha1_gitconnection_proto_rawDescGZIP(), []int{12}
}

func (x *GitRepository) GetFullName() string {
	if x != nil {
		return x.FullName
	}
	return ""
}

func (x *GitRepository) GetHtmlUrl() string {
	if x != nil {
		return x.HtmlUrl
	}
	return ""
}

func (x *GitRepository) GetDefaultBranch() string {
	if x != nil {
		return x.DefaultBranch
	}
	return ""
}

func (x *GitRepository) GetPrivate() bool {
	if x != nil {
		return x.Private
	}
	return false
}

// ListConnectionRepositoriesRequest names the connection to browse. It is
// `connection` and not `name` because the answer is about repositories rather
// than about the connection — the same word Project.spec.source.connection uses
// for the same reference.
type ListConnectionRepositoriesRequest struct {
	state         protoimpl.MessageState `protogen:"open.v1"`
	Connection    string                 `protobuf:"bytes,1,opt,name=connection,proto3" json:"connection,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *ListConnectionRepositoriesRequest) Reset() {
	*x = ListConnectionRepositoriesRequest{}
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[13]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *ListConnectionRepositoriesRequest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*ListConnectionRepositoriesRequest) ProtoMessage() {}

func (x *ListConnectionRepositoriesRequest) ProtoReflect() protoreflect.Message {
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[13]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use ListConnectionRepositoriesRequest.ProtoReflect.Descriptor instead.
func (*ListConnectionRepositoriesRequest) Descriptor() ([]byte, []int) {
	return file_kelson_v1alpha1_gitconnection_proto_rawDescGZIP(), []int{13}
}

func (x *ListConnectionRepositoriesRequest) GetConnection() string {
	if x != nil {
		return x.Connection
	}
	return ""
}

type ListConnectionRepositoriesResponse struct {
	state protoimpl.MessageState `protogen:"open.v1"`
	// Every repository the credential can see, in the order the provider
	// reported them. An installation lists exactly the repositories it was
	// granted; a token lists everything its owner can reach, which is a wider and
	// less deliberate set — ADR-0033 decision 2's argument for the app, visible
	// here as the difference between a short list and a long one.
	Repositories  []*GitRepository `protobuf:"bytes,1,rep,name=repositories,proto3" json:"repositories,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *ListConnectionRepositoriesResponse) Reset() {
	*x = ListConnectionRepositoriesResponse{}
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[14]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *ListConnectionRepositoriesResponse) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*ListConnectionRepositoriesResponse) ProtoMessage() {}

func (x *ListConnectionRepositoriesResponse) ProtoReflect() protoreflect.Message {
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[14]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use ListConnectionRepositoriesResponse.ProtoReflect.Descriptor instead.
func (*ListConnectionRepositoriesResponse) Descriptor() ([]byte, []int) {
	return file_kelson_v1alpha1_gitconnection_proto_rawDescGZIP(), []int{14}
}

func (x *ListConnectionRepositoriesResponse) GetRepositories() []*GitRepository {
	if x != nil {
		return x.Repositories
	}
	return nil
}

type ListConnectionBranchesRequest struct {
	state      protoimpl.MessageState `protogen:"open.v1"`
	Connection string                 `protobuf:"bytes,1,opt,name=connection,proto3" json:"connection,omitempty"`
	// "owner/name", as GitRepository.full_name reported it.
	Repository    string `protobuf:"bytes,2,opt,name=repository,proto3" json:"repository,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *ListConnectionBranchesRequest) Reset() {
	*x = ListConnectionBranchesRequest{}
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[15]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *ListConnectionBranchesRequest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*ListConnectionBranchesRequest) ProtoMessage() {}

func (x *ListConnectionBranchesRequest) ProtoReflect() protoreflect.Message {
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[15]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use ListConnectionBranchesRequest.ProtoReflect.Descriptor instead.
func (*ListConnectionBranchesRequest) Descriptor() ([]byte, []int) {
	return file_kelson_v1alpha1_gitconnection_proto_rawDescGZIP(), []int{15}
}

func (x *ListConnectionBranchesRequest) GetConnection() string {
	if x != nil {
		return x.Connection
	}
	return ""
}

func (x *ListConnectionBranchesRequest) GetRepository() string {
	if x != nil {
		return x.Repository
	}
	return ""
}

type ListConnectionBranchesResponse struct {
	state protoimpl.MessageState `protogen:"open.v1"`
	// Branch names only. A picker needs a name to write into `spec.source.ref`
	// and the ref resolver needs nothing from here at all, so the commit each
	// branch points at is deliberately absent: it would be stale by the time it
	// was read, and reading it is what BuildService does at build time.
	Branches      []string `protobuf:"bytes,1,rep,name=branches,proto3" json:"branches,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *ListConnectionBranchesResponse) Reset() {
	*x = ListConnectionBranchesResponse{}
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[16]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *ListConnectionBranchesResponse) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*ListConnectionBranchesResponse) ProtoMessage() {}

func (x *ListConnectionBranchesResponse) ProtoReflect() protoreflect.Message {
	mi := &file_kelson_v1alpha1_gitconnection_proto_msgTypes[16]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use ListConnectionBranchesResponse.ProtoReflect.Descriptor instead.
func (*ListConnectionBranchesResponse) Descriptor() ([]byte, []int) {
	return file_kelson_v1alpha1_gitconnection_proto_rawDescGZIP(), []int{16}
}

func (x *ListConnectionBranchesResponse) GetBranches() []string {
	if x != nil {
		return x.Branches
	}
	return nil
}

var File_kelson_v1alpha1_gitconnection_proto protoreflect.FileDescriptor

const file_kelson_v1alpha1_gitconnection_proto_rawDesc = "" +
	"\n" +
	"#kelson/v1alpha1/gitconnection.proto\x12\x0fkelson.v1alpha1\x1a\x1ckelson/v1alpha1/common.proto\"Q\n" +
	"\bGitOwner\x121\n" +
	"\x04kind\x18\x01 \x01(\x0e2\x1d.kelson.v1alpha1.GitOwnerKindR\x04kind\x12\x12\n" +
	"\x04name\x18\x02 \x01(\tR\x04name\"\xaa\x03\n" +
	"\rGitConnection\x12\x12\n" +
	"\x04name\x18\x01 \x01(\tR\x04name\x12\x1a\n" +
	"\bprovider\x18\x02 \x01(\tR\bprovider\x12\x12\n" +
	"\x04host\x18\x03 \x01(\tR\x04host\x12/\n" +
	"\x05owner\x18\x04 \x01(\v2\x19.kelson.v1alpha1.GitOwnerR\x05owner\x129\n" +
	"\tauth_kind\x18\x05 \x01(\x0e2\x1c.kelson.v1alpha1.GitAuthKindR\bauthKind\x12\x15\n" +
	"\x06app_id\x18\x06 \x01(\x03R\x05appId\x12'\n" +
	"\x0finstallation_id\x18\a \x01(\x03R\x0einstallationId\x12\x1d\n" +
	"\n" +
	"secret_ref\x18\b \x01(\tR\tsecretRef\x12\x18\n" +
	"\aaccount\x18\t \x01(\tR\aaccount\x12\"\n" +
	"\frepositories\x18\n" +
	" \x01(\x05R\frepositories\x12\x14\n" +
	"\x05ready\x18\v \x01(\bR\x05ready\x12\x1c\n" +
	"\treachable\x18\f \x01(\bR\treachable\x12\x18\n" +
	"\amessage\x18\r \x01(\tR\amessage\"\x18\n" +
	"\x16ListConnectionsRequest\"[\n" +
	"\x17ListConnectionsResponse\x12@\n" +
	"\vconnections\x18\x01 \x03(\v2\x1e.kelson.v1alpha1.GitConnectionR\vconnections\"*\n" +
	"\x14GetConnectionRequest\x12\x12\n" +
	"\x04name\x18\x01 \x01(\tR\x04name\"W\n" +
	"\x15GetConnectionResponse\x12>\n" +
	"\n" +
	"connection\x18\x01 \x01(\v2\x1e.kelson.v1alpha1.GitConnectionR\n" +
	"connection\"\x88\x02\n" +
	"\x17CreateConnectionRequest\x12\x12\n" +
	"\x04name\x18\x01 \x01(\tR\x04name\x12\x1a\n" +
	"\bprovider\x18\x02 \x01(\tR\bprovider\x12\x12\n" +
	"\x04host\x18\x03 \x01(\tR\x04host\x12\x1d\n" +
	"\n" +
	"secret_ref\x18\x04 \x01(\tR\tsecretRef\x12/\n" +
	"\x05owner\x18\x05 \x01(\v2\x19.kelson.v1alpha1.GitOwnerR\x05owner\x120\n" +
	"\adry_run\x18\x06 \x01(\x0e2\x17.kelson.v1alpha1.DryRunR\x06dryRun\x12'\n" +
	"\x0fidempotency_key\x18\a \x01(\tR\x0eidempotencyKey\"s\n" +
	"\x18CreateConnectionResponse\x12>\n" +
	"\n" +
	"connection\x18\x01 \x01(\v2\x1e.kelson.v1alpha1.GitConnectionR\n" +
	"connection\x12\x17\n" +
	"\adry_run\x18\x02 \x01(\bR\x06dryRun\"\x88\x01\n" +
	"\x17DeleteConnectionRequest\x12\x12\n" +
	"\x04name\x18\x01 \x01(\tR\x04name\x120\n" +
	"\adry_run\x18\x02 \x01(\x0e2\x17.kelson.v1alpha1.DryRunR\x06dryRun\x12'\n" +
	"\x0fidempotency_key\x18\x03 \x01(\tR\x0eidempotencyKey\"a\n" +
	"\x18DeleteConnectionResponse\x12\x18\n" +
	"\adeleted\x18\x01 \x01(\bR\adeleted\x12+\n" +
	"\x11affected_projects\x18\x02 \x03(\tR\x10affectedProjects\"+\n" +
	"\x15TestConnectionRequest\x12\x12\n" +
	"\x04name\x18\x01 \x01(\tR\x04name\"\x8e\x01\n" +
	"\x16TestConnectionResponse\x12\x1c\n" +
	"\treachable\x18\x01 \x01(\bR\treachable\x12\x18\n" +
	"\aaccount\x18\x02 \x01(\tR\aaccount\x12\"\n" +
	"\frepositories\x18\x03 \x01(\x05R\frepositories\x12\x18\n" +
	"\amessage\x18\x04 \x01(\tR\amessage\"\x88\x01\n" +
	"\rGitRepository\x12\x1b\n" +
	"\tfull_name\x18\x01 \x01(\tR\bfullName\x12\x19\n" +
	"\bhtml_url\x18\x02 \x01(\tR\ahtmlUrl\x12%\n" +
	"\x0edefault_branch\x18\x03 \x01(\tR\rdefaultBranch\x12\x18\n" +
	"\aprivate\x18\x04 \x01(\bR\aprivate\"C\n" +
	"!ListConnectionRepositoriesRequest\x12\x1e\n" +
	"\n" +
	"connection\x18\x01 \x01(\tR\n" +
	"connection\"h\n" +
	"\"ListConnectionRepositoriesResponse\x12B\n" +
	"\frepositories\x18\x01 \x03(\v2\x1e.kelson.v1alpha1.GitRepositoryR\frepositories\"_\n" +
	"\x1dListConnectionBranchesRequest\x12\x1e\n" +
	"\n" +
	"connection\x18\x01 \x01(\tR\n" +
	"connection\x12\x1e\n" +
	"\n" +
	"repository\x18\x02 \x01(\tR\n" +
	"repository\"<\n" +
	"\x1eListConnectionBranchesResponse\x12\x1a\n" +
	"\bbranches\x18\x01 \x03(\tR\bbranches*c\n" +
	"\vGitAuthKind\x12\x1d\n" +
	"\x19GIT_AUTH_KIND_UNSPECIFIED\x10\x00\x12\x1c\n" +
	"\x18GIT_AUTH_KIND_GITHUB_APP\x10\x01\x12\x17\n" +
	"\x13GIT_AUTH_KIND_TOKEN\x10\x02*}\n" +
	"\fGitOwnerKind\x12\x1e\n" +
	"\x1aGIT_OWNER_KIND_UNSPECIFIED\x10\x00\x12\x1b\n" +
	"\x17GIT_OWNER_KIND_INSTANCE\x10\x01\x12\x17\n" +
	"\x13GIT_OWNER_KIND_USER\x10\x02\x12\x17\n" +
	"\x13GIT_OWNER_KIND_TEAM\x10\x032\x94\x06\n" +
	"\x14GitConnectionService\x12d\n" +
	"\x0fListConnections\x12'.kelson.v1alpha1.ListConnectionsRequest\x1a(.kelson.v1alpha1.ListConnectionsResponse\x12^\n" +
	"\rGetConnection\x12%.kelson.v1alpha1.GetConnectionRequest\x1a&.kelson.v1alpha1.GetConnectionResponse\x12g\n" +
	"\x10CreateConnection\x12(.kelson.v1alpha1.CreateConnectionRequest\x1a).kelson.v1alpha1.CreateConnectionResponse\x12g\n" +
	"\x10DeleteConnection\x12(.kelson.v1alpha1.DeleteConnectionRequest\x1a).kelson.v1alpha1.DeleteConnectionResponse\x12a\n" +
	"\x0eTestConnection\x12&.kelson.v1alpha1.TestConnectionRequest\x1a'.kelson.v1alpha1.TestConnectionResponse\x12\x85\x01\n" +
	"\x1aListConnectionRepositories\x122.kelson.v1alpha1.ListConnectionRepositoriesRequest\x1a3.kelson.v1alpha1.ListConnectionRepositoriesResponse\x12y\n" +
	"\x16ListConnectionBranches\x12..kelson.v1alpha1.ListConnectionBranchesRequest\x1a/.kelson.v1alpha1.ListConnectionBranchesResponseBJZHgithub.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1;kelsonv1alpha1b\x06proto3"

var (
	file_kelson_v1alpha1_gitconnection_proto_rawDescOnce sync.Once
	file_kelson_v1alpha1_gitconnection_proto_rawDescData []byte
)

func file_kelson_v1alpha1_gitconnection_proto_rawDescGZIP() []byte {
	file_kelson_v1alpha1_gitconnection_proto_rawDescOnce.Do(func() {
		file_kelson_v1alpha1_gitconnection_proto_rawDescData = protoimpl.X.CompressGZIP(unsafe.Slice(unsafe.StringData(file_kelson_v1alpha1_gitconnection_proto_rawDesc), len(file_kelson_v1alpha1_gitconnection_proto_rawDesc)))
	})
	return file_kelson_v1alpha1_gitconnection_proto_rawDescData
}

var file_kelson_v1alpha1_gitconnection_proto_enumTypes = make([]protoimpl.EnumInfo, 2)
var file_kelson_v1alpha1_gitconnection_proto_msgTypes = make([]protoimpl.MessageInfo, 17)
var file_kelson_v1alpha1_gitconnection_proto_goTypes = []any{
	(GitAuthKind)(0),                           // 0: kelson.v1alpha1.GitAuthKind
	(GitOwnerKind)(0),                          // 1: kelson.v1alpha1.GitOwnerKind
	(*GitOwner)(nil),                           // 2: kelson.v1alpha1.GitOwner
	(*GitConnection)(nil),                      // 3: kelson.v1alpha1.GitConnection
	(*ListConnectionsRequest)(nil),             // 4: kelson.v1alpha1.ListConnectionsRequest
	(*ListConnectionsResponse)(nil),            // 5: kelson.v1alpha1.ListConnectionsResponse
	(*GetConnectionRequest)(nil),               // 6: kelson.v1alpha1.GetConnectionRequest
	(*GetConnectionResponse)(nil),              // 7: kelson.v1alpha1.GetConnectionResponse
	(*CreateConnectionRequest)(nil),            // 8: kelson.v1alpha1.CreateConnectionRequest
	(*CreateConnectionResponse)(nil),           // 9: kelson.v1alpha1.CreateConnectionResponse
	(*DeleteConnectionRequest)(nil),            // 10: kelson.v1alpha1.DeleteConnectionRequest
	(*DeleteConnectionResponse)(nil),           // 11: kelson.v1alpha1.DeleteConnectionResponse
	(*TestConnectionRequest)(nil),              // 12: kelson.v1alpha1.TestConnectionRequest
	(*TestConnectionResponse)(nil),             // 13: kelson.v1alpha1.TestConnectionResponse
	(*GitRepository)(nil),                      // 14: kelson.v1alpha1.GitRepository
	(*ListConnectionRepositoriesRequest)(nil),  // 15: kelson.v1alpha1.ListConnectionRepositoriesRequest
	(*ListConnectionRepositoriesResponse)(nil), // 16: kelson.v1alpha1.ListConnectionRepositoriesResponse
	(*ListConnectionBranchesRequest)(nil),      // 17: kelson.v1alpha1.ListConnectionBranchesRequest
	(*ListConnectionBranchesResponse)(nil),     // 18: kelson.v1alpha1.ListConnectionBranchesResponse
	(DryRun)(0),                                // 19: kelson.v1alpha1.DryRun
}
var file_kelson_v1alpha1_gitconnection_proto_depIdxs = []int32{
	1,  // 0: kelson.v1alpha1.GitOwner.kind:type_name -> kelson.v1alpha1.GitOwnerKind
	2,  // 1: kelson.v1alpha1.GitConnection.owner:type_name -> kelson.v1alpha1.GitOwner
	0,  // 2: kelson.v1alpha1.GitConnection.auth_kind:type_name -> kelson.v1alpha1.GitAuthKind
	3,  // 3: kelson.v1alpha1.ListConnectionsResponse.connections:type_name -> kelson.v1alpha1.GitConnection
	3,  // 4: kelson.v1alpha1.GetConnectionResponse.connection:type_name -> kelson.v1alpha1.GitConnection
	2,  // 5: kelson.v1alpha1.CreateConnectionRequest.owner:type_name -> kelson.v1alpha1.GitOwner
	19, // 6: kelson.v1alpha1.CreateConnectionRequest.dry_run:type_name -> kelson.v1alpha1.DryRun
	3,  // 7: kelson.v1alpha1.CreateConnectionResponse.connection:type_name -> kelson.v1alpha1.GitConnection
	19, // 8: kelson.v1alpha1.DeleteConnectionRequest.dry_run:type_name -> kelson.v1alpha1.DryRun
	14, // 9: kelson.v1alpha1.ListConnectionRepositoriesResponse.repositories:type_name -> kelson.v1alpha1.GitRepository
	4,  // 10: kelson.v1alpha1.GitConnectionService.ListConnections:input_type -> kelson.v1alpha1.ListConnectionsRequest
	6,  // 11: kelson.v1alpha1.GitConnectionService.GetConnection:input_type -> kelson.v1alpha1.GetConnectionRequest
	8,  // 12: kelson.v1alpha1.GitConnectionService.CreateConnection:input_type -> kelson.v1alpha1.CreateConnectionRequest
	10, // 13: kelson.v1alpha1.GitConnectionService.DeleteConnection:input_type -> kelson.v1alpha1.DeleteConnectionRequest
	12, // 14: kelson.v1alpha1.GitConnectionService.TestConnection:input_type -> kelson.v1alpha1.TestConnectionRequest
	15, // 15: kelson.v1alpha1.GitConnectionService.ListConnectionRepositories:input_type -> kelson.v1alpha1.ListConnectionRepositoriesRequest
	17, // 16: kelson.v1alpha1.GitConnectionService.ListConnectionBranches:input_type -> kelson.v1alpha1.ListConnectionBranchesRequest
	5,  // 17: kelson.v1alpha1.GitConnectionService.ListConnections:output_type -> kelson.v1alpha1.ListConnectionsResponse
	7,  // 18: kelson.v1alpha1.GitConnectionService.GetConnection:output_type -> kelson.v1alpha1.GetConnectionResponse
	9,  // 19: kelson.v1alpha1.GitConnectionService.CreateConnection:output_type -> kelson.v1alpha1.CreateConnectionResponse
	11, // 20: kelson.v1alpha1.GitConnectionService.DeleteConnection:output_type -> kelson.v1alpha1.DeleteConnectionResponse
	13, // 21: kelson.v1alpha1.GitConnectionService.TestConnection:output_type -> kelson.v1alpha1.TestConnectionResponse
	16, // 22: kelson.v1alpha1.GitConnectionService.ListConnectionRepositories:output_type -> kelson.v1alpha1.ListConnectionRepositoriesResponse
	18, // 23: kelson.v1alpha1.GitConnectionService.ListConnectionBranches:output_type -> kelson.v1alpha1.ListConnectionBranchesResponse
	17, // [17:24] is the sub-list for method output_type
	10, // [10:17] is the sub-list for method input_type
	10, // [10:10] is the sub-list for extension type_name
	10, // [10:10] is the sub-list for extension extendee
	0,  // [0:10] is the sub-list for field type_name
}

func init() { file_kelson_v1alpha1_gitconnection_proto_init() }
func file_kelson_v1alpha1_gitconnection_proto_init() {
	if File_kelson_v1alpha1_gitconnection_proto != nil {
		return
	}
	file_kelson_v1alpha1_common_proto_init()
	type x struct{}
	out := protoimpl.TypeBuilder{
		File: protoimpl.DescBuilder{
			GoPackagePath: reflect.TypeOf(x{}).PkgPath(),
			RawDescriptor: unsafe.Slice(unsafe.StringData(file_kelson_v1alpha1_gitconnection_proto_rawDesc), len(file_kelson_v1alpha1_gitconnection_proto_rawDesc)),
			NumEnums:      2,
			NumMessages:   17,
			NumExtensions: 0,
			NumServices:   1,
		},
		GoTypes:           file_kelson_v1alpha1_gitconnection_proto_goTypes,
		DependencyIndexes: file_kelson_v1alpha1_gitconnection_proto_depIdxs,
		EnumInfos:         file_kelson_v1alpha1_gitconnection_proto_enumTypes,
		MessageInfos:      file_kelson_v1alpha1_gitconnection_proto_msgTypes,
	}.Build()
	File_kelson_v1alpha1_gitconnection_proto = out.File
	file_kelson_v1alpha1_gitconnection_proto_goTypes = nil
	file_kelson_v1alpha1_gitconnection_proto_depIdxs = nil
}
