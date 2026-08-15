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

// Code generated by protoc-gen-connect-go. DO NOT EDIT.
//
// Source: kelson/v1alpha1/gitconnection.proto

package kelsonv1alpha1connect

import (
	connect "connectrpc.com/connect"
	context "context"
	errors "errors"
	v1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	http "net/http"
	strings "strings"
)

// This is a compile-time assertion to ensure that this generated file and the connect package are
// compatible. If you get a compiler error that this constant is not defined, this code was
// generated with a version of connect newer than the one compiled into your binary. You can fix the
// problem by either regenerating this code with an older version of connect or updating the connect
// version compiled into your binary.
const _ = connect.IsAtLeastVersion1_13_0

const (
	// GitConnectionServiceName is the fully-qualified name of the GitConnectionService service.
	GitConnectionServiceName = "kelson.v1alpha1.GitConnectionService"
)

// These constants are the fully-qualified names of the RPCs defined in this package. They're
// exposed at runtime as Spec.Procedure and as the final two segments of the HTTP route.
//
// Note that these are different from the fully-qualified method names used by
// google.golang.org/protobuf/reflect/protoreflect. To convert from these constants to
// reflection-formatted method names, remove the leading slash and convert the remaining slash to a
// period.
const (
	// GitConnectionServiceListConnectionsProcedure is the fully-qualified name of the
	// GitConnectionService's ListConnections RPC.
	GitConnectionServiceListConnectionsProcedure = "/kelson.v1alpha1.GitConnectionService/ListConnections"
	// GitConnectionServiceGetConnectionProcedure is the fully-qualified name of the
	// GitConnectionService's GetConnection RPC.
	GitConnectionServiceGetConnectionProcedure = "/kelson.v1alpha1.GitConnectionService/GetConnection"
	// GitConnectionServiceCreateConnectionProcedure is the fully-qualified name of the
	// GitConnectionService's CreateConnection RPC.
	GitConnectionServiceCreateConnectionProcedure = "/kelson.v1alpha1.GitConnectionService/CreateConnection"
	// GitConnectionServiceDeleteConnectionProcedure is the fully-qualified name of the
	// GitConnectionService's DeleteConnection RPC.
	GitConnectionServiceDeleteConnectionProcedure = "/kelson.v1alpha1.GitConnectionService/DeleteConnection"
	// GitConnectionServiceTestConnectionProcedure is the fully-qualified name of the
	// GitConnectionService's TestConnection RPC.
	GitConnectionServiceTestConnectionProcedure = "/kelson.v1alpha1.GitConnectionService/TestConnection"
	// GitConnectionServiceListConnectionRepositoriesProcedure is the fully-qualified name of the
	// GitConnectionService's ListConnectionRepositories RPC.
	GitConnectionServiceListConnectionRepositoriesProcedure = "/kelson.v1alpha1.GitConnectionService/ListConnectionRepositories"
	// GitConnectionServiceListConnectionBranchesProcedure is the fully-qualified name of the
	// GitConnectionService's ListConnectionBranches RPC.
	GitConnectionServiceListConnectionBranchesProcedure = "/kelson.v1alpha1.GitConnectionService/ListConnectionBranches"
)

// GitConnectionServiceClient is a client for the kelson.v1alpha1.GitConnectionService service.
type GitConnectionServiceClient interface {
	// ListConnections reports every connection: what this cluster can pull from,
	// which is `kubectl get gitconnections` for a caller that has no kubeconfig.
	ListConnections(context.Context, *connect.Request[v1alpha1.ListConnectionsRequest]) (*connect.Response[v1alpha1.ListConnectionsResponse], error)
	// GetConnection reports one, by name.
	GetConnection(context.Context, *connect.Request[v1alpha1.GetConnectionRequest]) (*connect.Response[v1alpha1.GetConnectionResponse], error)
	// CreateConnection creates a token-auth connection. App connections come from
	// the manifest flow's HTTP callback and not from here (ADR-0033 decision 2).
	CreateConnection(context.Context, *connect.Request[v1alpha1.CreateConnectionRequest]) (*connect.Response[v1alpha1.CreateConnectionResponse], error)
	// DeleteConnection removes the connection. It does not remove the Secret it
	// references: kelson did not create that Secret for a token connection and
	// deleting somebody else's object on the way out is not this call's to do.
	// An app connection's Secret *was* kelson's to write, and reclaiming it is
	// the manifest flow's own decision to record, not this RPC's to assume.
	DeleteConnection(context.Context, *connect.Request[v1alpha1.DeleteConnectionRequest]) (*connect.Response[v1alpha1.DeleteConnectionResponse], error)
	// TestConnection probes the forge, live, server-side, using the Secret the
	// connection references — an authenticated call to the provider's API, made
	// now rather than read from a condition written at some earlier reconcile.
	//
	// It reads nothing back to the caller that the credential itself would
	// reveal: the answer is reachability, the account name and a count, which is
	// the same triple the status subresource carries. The credential is used and
	// never returned, and any token minted to make the call is registered with
	// internal/redact before it is used.
	TestConnection(context.Context, *connect.Request[v1alpha1.TestConnectionRequest]) (*connect.Response[v1alpha1.TestConnectionResponse], error)
	// ListConnectionRepositories reports the repositories this connection's
	// credential can see — the New Project repository picker's read, made
	// server-side with the Secret the connection references.
	//
	// # It is capability-gated, and the gate is part of the answer
	//
	// Repository browsing is `RepoBrowser`, optional in ADR-0033 decision 3. A
	// connection whose provider implements it is served; one whose provider does
	// not is refused with `connection/capability-unsupported`, naming what the
	// connection *can* do and saying that pasting the repository URL works for
	// every connection there is. It is not an empty list, because an empty list
	// is what a real installation with nothing selected looks like, and it is not
	// a fault of the connection: a `generic` token connection that cannot be
	// browsed still clones private repositories, which is the capability a deploy
	// needs.
	//
	// Read-only, and it changes nothing: no status is written, and no credential
	// reaches the response — GitRepository has no field one would fit in. Any
	// token minted to make the call is registered with internal/redact before it
	// is used, exactly as TestConnection's is.
	ListConnectionRepositories(context.Context, *connect.Request[v1alpha1.ListConnectionRepositoriesRequest]) (*connect.Response[v1alpha1.ListConnectionRepositoriesResponse], error)
	// ListConnectionBranches reports one repository's branches, for the picker's
	// second step. Same capability gate and same refusal as
	// ListConnectionRepositories — they are the two halves of one `RepoBrowser`,
	// and a connection that answered the first will answer this one — and the
	// same read-only posture.
	//
	// The repository is named as "owner/name" rather than as a URL: it is the key
	// the listing above already reported, and re-deriving it from a URL here
	// would be a second parser disagreeing with the first.
	ListConnectionBranches(context.Context, *connect.Request[v1alpha1.ListConnectionBranchesRequest]) (*connect.Response[v1alpha1.ListConnectionBranchesResponse], error)
}

// NewGitConnectionServiceClient constructs a client for the kelson.v1alpha1.GitConnectionService
// service. By default, it uses the Connect protocol with the binary Protobuf Codec, asks for
// gzipped responses, and sends uncompressed requests. To use the gRPC or gRPC-Web protocols, supply
// the connect.WithGRPC() or connect.WithGRPCWeb() options.
//
// The URL supplied here should be the base URL for the Connect or gRPC server (for example,
// http://api.acme.com or https://acme.com/grpc).
func NewGitConnectionServiceClient(httpClient connect.HTTPClient, baseURL string, opts ...connect.ClientOption) GitConnectionServiceClient {
	baseURL = strings.TrimRight(baseURL, "/")
	gitConnectionServiceMethods := v1alpha1.File_kelson_v1alpha1_gitconnection_proto.Services().ByName("GitConnectionService").Methods()
	return &gitConnectionServiceClient{
		listConnections: connect.NewClient[v1alpha1.ListConnectionsRequest, v1alpha1.ListConnectionsResponse](
			httpClient,
			baseURL+GitConnectionServiceListConnectionsProcedure,
			connect.WithSchema(gitConnectionServiceMethods.ByName("ListConnections")),
			connect.WithClientOptions(opts...),
		),
		getConnection: connect.NewClient[v1alpha1.GetConnectionRequest, v1alpha1.GetConnectionResponse](
			httpClient,
			baseURL+GitConnectionServiceGetConnectionProcedure,
			connect.WithSchema(gitConnectionServiceMethods.ByName("GetConnection")),
			connect.WithClientOptions(opts...),
		),
		createConnection: connect.NewClient[v1alpha1.CreateConnectionRequest, v1alpha1.CreateConnectionResponse](
			httpClient,
			baseURL+GitConnectionServiceCreateConnectionProcedure,
			connect.WithSchema(gitConnectionServiceMethods.ByName("CreateConnection")),
			connect.WithClientOptions(opts...),
		),
		deleteConnection: connect.NewClient[v1alpha1.DeleteConnectionRequest, v1alpha1.DeleteConnectionResponse](
			httpClient,
			baseURL+GitConnectionServiceDeleteConnectionProcedure,
			connect.WithSchema(gitConnectionServiceMethods.ByName("DeleteConnection")),
			connect.WithClientOptions(opts...),
		),
		testConnection: connect.NewClient[v1alpha1.TestConnectionRequest, v1alpha1.TestConnectionResponse](
			httpClient,
			baseURL+GitConnectionServiceTestConnectionProcedure,
			connect.WithSchema(gitConnectionServiceMethods.ByName("TestConnection")),
			connect.WithClientOptions(opts...),
		),
		listConnectionRepositories: connect.NewClient[v1alpha1.ListConnectionRepositoriesRequest, v1alpha1.ListConnectionRepositoriesResponse](
			httpClient,
			baseURL+GitConnectionServiceListConnectionRepositoriesProcedure,
			connect.WithSchema(gitConnectionServiceMethods.ByName("ListConnectionRepositories")),
			connect.WithClientOptions(opts...),
		),
		listConnectionBranches: connect.NewClient[v1alpha1.ListConnectionBranchesRequest, v1alpha1.ListConnectionBranchesResponse](
			httpClient,
			baseURL+GitConnectionServiceListConnectionBranchesProcedure,
			connect.WithSchema(gitConnectionServiceMethods.ByName("ListConnectionBranches")),
			connect.WithClientOptions(opts...),
		),
	}
}

// gitConnectionServiceClient implements GitConnectionServiceClient.
type gitConnectionServiceClient struct {
	listConnections            *connect.Client[v1alpha1.ListConnectionsRequest, v1alpha1.ListConnectionsResponse]
	getConnection              *connect.Client[v1alpha1.GetConnectionRequest, v1alpha1.GetConnectionResponse]
	createConnection           *connect.Client[v1alpha1.CreateConnectionRequest, v1alpha1.CreateConnectionResponse]
	deleteConnection           *connect.Client[v1alpha1.DeleteConnectionRequest, v1alpha1.DeleteConnectionResponse]
	testConnection             *connect.Client[v1alpha1.TestConnectionRequest, v1alpha1.TestConnectionResponse]
	listConnectionRepositories *connect.Client[v1alpha1.ListConnectionRepositoriesRequest, v1alpha1.ListConnectionRepositoriesResponse]
	listConnectionBranches     *connect.Client[v1alpha1.ListConnectionBranchesRequest, v1alpha1.ListConnectionBranchesResponse]
}

// ListConnections calls kelson.v1alpha1.GitConnectionService.ListConnections.
func (c *gitConnectionServiceClient) ListConnections(ctx context.Context, req *connect.Request[v1alpha1.ListConnectionsRequest]) (*connect.Response[v1alpha1.ListConnectionsResponse], error) {
	return c.listConnections.CallUnary(ctx, req)
}

// GetConnection calls kelson.v1alpha1.GitConnectionService.GetConnection.
func (c *gitConnectionServiceClient) GetConnection(ctx context.Context, req *connect.Request[v1alpha1.GetConnectionRequest]) (*connect.Response[v1alpha1.GetConnectionResponse], error) {
	return c.getConnection.CallUnary(ctx, req)
}

// CreateConnection calls kelson.v1alpha1.GitConnectionService.CreateConnection.
func (c *gitConnectionServiceClient) CreateConnection(ctx context.Context, req *connect.Request[v1alpha1.CreateConnectionRequest]) (*connect.Response[v1alpha1.CreateConnectionResponse], error) {
	return c.createConnection.CallUnary(ctx, req)
}

// DeleteConnection calls kelson.v1alpha1.GitConnectionService.DeleteConnection.
func (c *gitConnectionServiceClient) DeleteConnection(ctx context.Context, req *connect.Request[v1alpha1.DeleteConnectionRequest]) (*connect.Response[v1alpha1.DeleteConnectionResponse], error) {
	return c.deleteConnection.CallUnary(ctx, req)
}

// TestConnection calls kelson.v1alpha1.GitConnectionService.TestConnection.
func (c *gitConnectionServiceClient) TestConnection(ctx context.Context, req *connect.Request[v1alpha1.TestConnectionRequest]) (*connect.Response[v1alpha1.TestConnectionResponse], error) {
	return c.testConnection.CallUnary(ctx, req)
}

// ListConnectionRepositories calls kelson.v1alpha1.GitConnectionService.ListConnectionRepositories.
func (c *gitConnectionServiceClient) ListConnectionRepositories(ctx context.Context, req *connect.Request[v1alpha1.ListConnectionRepositoriesRequest]) (*connect.Response[v1alpha1.ListConnectionRepositoriesResponse], error) {
	return c.listConnectionRepositories.CallUnary(ctx, req)
}

// ListConnectionBranches calls kelson.v1alpha1.GitConnectionService.ListConnectionBranches.
func (c *gitConnectionServiceClient) ListConnectionBranches(ctx context.Context, req *connect.Request[v1alpha1.ListConnectionBranchesRequest]) (*connect.Response[v1alpha1.ListConnectionBranchesResponse], error) {
	return c.listConnectionBranches.CallUnary(ctx, req)
}

// GitConnectionServiceHandler is an implementation of the kelson.v1alpha1.GitConnectionService
// service.
type GitConnectionServiceHandler interface {
	// ListConnections reports every connection: what this cluster can pull from,
	// which is `kubectl get gitconnections` for a caller that has no kubeconfig.
	ListConnections(context.Context, *connect.Request[v1alpha1.ListConnectionsRequest]) (*connect.Response[v1alpha1.ListConnectionsResponse], error)
	// GetConnection reports one, by name.
	GetConnection(context.Context, *connect.Request[v1alpha1.GetConnectionRequest]) (*connect.Response[v1alpha1.GetConnectionResponse], error)
	// CreateConnection creates a token-auth connection. App connections come from
	// the manifest flow's HTTP callback and not from here (ADR-0033 decision 2).
	CreateConnection(context.Context, *connect.Request[v1alpha1.CreateConnectionRequest]) (*connect.Response[v1alpha1.CreateConnectionResponse], error)
	// DeleteConnection removes the connection. It does not remove the Secret it
	// references: kelson did not create that Secret for a token connection and
	// deleting somebody else's object on the way out is not this call's to do.
	// An app connection's Secret *was* kelson's to write, and reclaiming it is
	// the manifest flow's own decision to record, not this RPC's to assume.
	DeleteConnection(context.Context, *connect.Request[v1alpha1.DeleteConnectionRequest]) (*connect.Response[v1alpha1.DeleteConnectionResponse], error)
	// TestConnection probes the forge, live, server-side, using the Secret the
	// connection references — an authenticated call to the provider's API, made
	// now rather than read from a condition written at some earlier reconcile.
	//
	// It reads nothing back to the caller that the credential itself would
	// reveal: the answer is reachability, the account name and a count, which is
	// the same triple the status subresource carries. The credential is used and
	// never returned, and any token minted to make the call is registered with
	// internal/redact before it is used.
	TestConnection(context.Context, *connect.Request[v1alpha1.TestConnectionRequest]) (*connect.Response[v1alpha1.TestConnectionResponse], error)
	// ListConnectionRepositories reports the repositories this connection's
	// credential can see — the New Project repository picker's read, made
	// server-side with the Secret the connection references.
	//
	// # It is capability-gated, and the gate is part of the answer
	//
	// Repository browsing is `RepoBrowser`, optional in ADR-0033 decision 3. A
	// connection whose provider implements it is served; one whose provider does
	// not is refused with `connection/capability-unsupported`, naming what the
	// connection *can* do and saying that pasting the repository URL works for
	// every connection there is. It is not an empty list, because an empty list
	// is what a real installation with nothing selected looks like, and it is not
	// a fault of the connection: a `generic` token connection that cannot be
	// browsed still clones private repositories, which is the capability a deploy
	// needs.
	//
	// Read-only, and it changes nothing: no status is written, and no credential
	// reaches the response — GitRepository has no field one would fit in. Any
	// token minted to make the call is registered with internal/redact before it
	// is used, exactly as TestConnection's is.
	ListConnectionRepositories(context.Context, *connect.Request[v1alpha1.ListConnectionRepositoriesRequest]) (*connect.Response[v1alpha1.ListConnectionRepositoriesResponse], error)
	// ListConnectionBranches reports one repository's branches, for the picker's
	// second step. Same capability gate and same refusal as
	// ListConnectionRepositories — they are the two halves of one `RepoBrowser`,
	// and a connection that answered the first will answer this one — and the
	// same read-only posture.
	//
	// The repository is named as "owner/name" rather than as a URL: it is the key
	// the listing above already reported, and re-deriving it from a URL here
	// would be a second parser disagreeing with the first.
	ListConnectionBranches(context.Context, *connect.Request[v1alpha1.ListConnectionBranchesRequest]) (*connect.Response[v1alpha1.ListConnectionBranchesResponse], error)
}

// NewGitConnectionServiceHandler builds an HTTP handler from the service implementation. It returns
// the path on which to mount the handler and the handler itself.
//
// By default, handlers support the Connect, gRPC, and gRPC-Web protocols with the binary Protobuf
// and JSON codecs. They also support gzip compression.
func NewGitConnectionServiceHandler(svc GitConnectionServiceHandler, opts ...connect.HandlerOption) (string, http.Handler) {
	gitConnectionServiceMethods := v1alpha1.File_kelson_v1alpha1_gitconnection_proto.Services().ByName("GitConnectionService").Methods()
	gitConnectionServiceListConnectionsHandler := connect.NewUnaryHandler(
		GitConnectionServiceListConnectionsProcedure,
		svc.ListConnections,
		connect.WithSchema(gitConnectionServiceMethods.ByName("ListConnections")),
		connect.WithHandlerOptions(opts...),
	)
	gitConnectionServiceGetConnectionHandler := connect.NewUnaryHandler(
		GitConnectionServiceGetConnectionProcedure,
		svc.GetConnection,
		connect.WithSchema(gitConnectionServiceMethods.ByName("GetConnection")),
		connect.WithHandlerOptions(opts...),
	)
	gitConnectionServiceCreateConnectionHandler := connect.NewUnaryHandler(
		GitConnectionServiceCreateConnectionProcedure,
		svc.CreateConnection,
		connect.WithSchema(gitConnectionServiceMethods.ByName("CreateConnection")),
		connect.WithHandlerOptions(opts...),
	)
	gitConnectionServiceDeleteConnectionHandler := connect.NewUnaryHandler(
		GitConnectionServiceDeleteConnectionProcedure,
		svc.DeleteConnection,
		connect.WithSchema(gitConnectionServiceMethods.ByName("DeleteConnection")),
		connect.WithHandlerOptions(opts...),
	)
	gitConnectionServiceTestConnectionHandler := connect.NewUnaryHandler(
		GitConnectionServiceTestConnectionProcedure,
		svc.TestConnection,
		connect.WithSchema(gitConnectionServiceMethods.ByName("TestConnection")),
		connect.WithHandlerOptions(opts...),
	)
	gitConnectionServiceListConnectionRepositoriesHandler := connect.NewUnaryHandler(
		GitConnectionServiceListConnectionRepositoriesProcedure,
		svc.ListConnectionRepositories,
		connect.WithSchema(gitConnectionServiceMethods.ByName("ListConnectionRepositories")),
		connect.WithHandlerOptions(opts...),
	)
	gitConnectionServiceListConnectionBranchesHandler := connect.NewUnaryHandler(
		GitConnectionServiceListConnectionBranchesProcedure,
		svc.ListConnectionBranches,
		connect.WithSchema(gitConnectionServiceMethods.ByName("ListConnectionBranches")),
		connect.WithHandlerOptions(opts...),
	)
	return "/kelson.v1alpha1.GitConnectionService/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case GitConnectionServiceListConnectionsProcedure:
			gitConnectionServiceListConnectionsHandler.ServeHTTP(w, r)
		case GitConnectionServiceGetConnectionProcedure:
			gitConnectionServiceGetConnectionHandler.ServeHTTP(w, r)
		case GitConnectionServiceCreateConnectionProcedure:
			gitConnectionServiceCreateConnectionHandler.ServeHTTP(w, r)
		case GitConnectionServiceDeleteConnectionProcedure:
			gitConnectionServiceDeleteConnectionHandler.ServeHTTP(w, r)
		case GitConnectionServiceTestConnectionProcedure:
			gitConnectionServiceTestConnectionHandler.ServeHTTP(w, r)
		case GitConnectionServiceListConnectionRepositoriesProcedure:
			gitConnectionServiceListConnectionRepositoriesHandler.ServeHTTP(w, r)
		case GitConnectionServiceListConnectionBranchesProcedure:
			gitConnectionServiceListConnectionBranchesHandler.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

// UnimplementedGitConnectionServiceHandler returns CodeUnimplemented from all methods.
type UnimplementedGitConnectionServiceHandler struct{}

func (UnimplementedGitConnectionServiceHandler) ListConnections(context.Context, *connect.Request[v1alpha1.ListConnectionsRequest]) (*connect.Response[v1alpha1.ListConnectionsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("kelson.v1alpha1.GitConnectionService.ListConnections is not implemented"))
}

func (UnimplementedGitConnectionServiceHandler) GetConnection(context.Context, *connect.Request[v1alpha1.GetConnectionRequest]) (*connect.Response[v1alpha1.GetConnectionResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("kelson.v1alpha1.GitConnectionService.GetConnection is not implemented"))
}

func (UnimplementedGitConnectionServiceHandler) CreateConnection(context.Context, *connect.Request[v1alpha1.CreateConnectionRequest]) (*connect.Response[v1alpha1.CreateConnectionResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("kelson.v1alpha1.GitConnectionService.CreateConnection is not implemented"))
}

func (UnimplementedGitConnectionServiceHandler) DeleteConnection(context.Context, *connect.Request[v1alpha1.DeleteConnectionRequest]) (*connect.Response[v1alpha1.DeleteConnectionResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("kelson.v1alpha1.GitConnectionService.DeleteConnection is not implemented"))
}

func (UnimplementedGitConnectionServiceHandler) TestConnection(context.Context, *connect.Request[v1alpha1.TestConnectionRequest]) (*connect.Response[v1alpha1.TestConnectionResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("kelson.v1alpha1.GitConnectionService.TestConnection is not implemented"))
}

func (UnimplementedGitConnectionServiceHandler) ListConnectionRepositories(context.Context, *connect.Request[v1alpha1.ListConnectionRepositoriesRequest]) (*connect.Response[v1alpha1.ListConnectionRepositoriesResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("kelson.v1alpha1.GitConnectionService.ListConnectionRepositories is not implemented"))
}

func (UnimplementedGitConnectionServiceHandler) ListConnectionBranches(context.Context, *connect.Request[v1alpha1.ListConnectionBranchesRequest]) (*connect.Response[v1alpha1.ListConnectionBranchesResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("kelson.v1alpha1.GitConnectionService.ListConnectionBranches is not implemented"))
}
