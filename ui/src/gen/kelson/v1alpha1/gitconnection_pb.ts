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

// @generated by protoc-gen-es v2.13.0 with parameter "target=ts"
// @generated from file kelson/v1alpha1/gitconnection.proto (package kelson.v1alpha1, syntax proto3)
/* eslint-disable */

import type { GenEnum, GenFile, GenMessage, GenService } from "@bufbuild/protobuf/codegenv2";
import { enumDesc, fileDesc, messageDesc, serviceDesc } from "@bufbuild/protobuf/codegenv2";
import type { DryRun } from "./common_pb";
import { file_kelson_v1alpha1_common } from "./common_pb";
import type { Message } from "@bufbuild/protobuf";

/**
 * Describes the file kelson/v1alpha1/gitconnection.proto.
 */
export const file_kelson_v1alpha1_gitconnection: GenFile = /*@__PURE__*/
  fileDesc("CiNrZWxzb24vdjFhbHBoYTEvZ2l0Y29ubmVjdGlvbi5wcm90bxIPa2Vsc29uLnYxYWxwaGExIkUKCEdpdE93bmVyEisKBGtpbmQYASABKA4yHS5rZWxzb24udjFhbHBoYTEuR2l0T3duZXJLaW5kEgwKBG5hbWUYAiABKAkirwIKDUdpdENvbm5lY3Rpb24SDAoEbmFtZRgBIAEoCRIQCghwcm92aWRlchgCIAEoCRIMCgRob3N0GAMgASgJEigKBW93bmVyGAQgASgLMhkua2Vsc29uLnYxYWxwaGExLkdpdE93bmVyEi8KCWF1dGhfa2luZBgFIAEoDjIcLmtlbHNvbi52MWFscGhhMS5HaXRBdXRoS2luZBIOCgZhcHBfaWQYBiABKAMSFwoPaW5zdGFsbGF0aW9uX2lkGAcgASgDEhIKCnNlY3JldF9yZWYYCCABKAkSDwoHYWNjb3VudBgJIAEoCRIUCgxyZXBvc2l0b3JpZXMYCiABKAUSDQoFcmVhZHkYCyABKAgSEQoJcmVhY2hhYmxlGAwgASgIEg8KB21lc3NhZ2UYDSABKAkiGAoWTGlzdENvbm5lY3Rpb25zUmVxdWVzdCJOChdMaXN0Q29ubmVjdGlvbnNSZXNwb25zZRIzCgtjb25uZWN0aW9ucxgBIAMoCzIeLmtlbHNvbi52MWFscGhhMS5HaXRDb25uZWN0aW9uIiQKFEdldENvbm5lY3Rpb25SZXF1ZXN0EgwKBG5hbWUYASABKAkiSwoVR2V0Q29ubmVjdGlvblJlc3BvbnNlEjIKCmNvbm5lY3Rpb24YASABKAsyHi5rZWxzb24udjFhbHBoYTEuR2l0Q29ubmVjdGlvbiLIAQoXQ3JlYXRlQ29ubmVjdGlvblJlcXVlc3QSDAoEbmFtZRgBIAEoCRIQCghwcm92aWRlchgCIAEoCRIMCgRob3N0GAMgASgJEhIKCnNlY3JldF9yZWYYBCABKAkSKAoFb3duZXIYBSABKAsyGS5rZWxzb24udjFhbHBoYTEuR2l0T3duZXISKAoHZHJ5X3J1bhgGIAEoDjIXLmtlbHNvbi52MWFscGhhMS5EcnlSdW4SFwoPaWRlbXBvdGVuY3lfa2V5GAcgASgJIl8KGENyZWF0ZUNvbm5lY3Rpb25SZXNwb25zZRIyCgpjb25uZWN0aW9uGAEgASgLMh4ua2Vsc29uLnYxYWxwaGExLkdpdENvbm5lY3Rpb24SDwoHZHJ5X3J1bhgCIAEoCCJqChdEZWxldGVDb25uZWN0aW9uUmVxdWVzdBIMCgRuYW1lGAEgASgJEigKB2RyeV9ydW4YAiABKA4yFy5rZWxzb24udjFhbHBoYTEuRHJ5UnVuEhcKD2lkZW1wb3RlbmN5X2tleRgDIAEoCSJGChhEZWxldGVDb25uZWN0aW9uUmVzcG9uc2USDwoHZGVsZXRlZBgBIAEoCBIZChFhZmZlY3RlZF9wcm9qZWN0cxgCIAMoCSIlChVUZXN0Q29ubmVjdGlvblJlcXVlc3QSDAoEbmFtZRgBIAEoCSJjChZUZXN0Q29ubmVjdGlvblJlc3BvbnNlEhEKCXJlYWNoYWJsZRgBIAEoCBIPCgdhY2NvdW50GAIgASgJEhQKDHJlcG9zaXRvcmllcxgDIAEoBRIPCgdtZXNzYWdlGAQgASgJIl0KDUdpdFJlcG9zaXRvcnkSEQoJZnVsbF9uYW1lGAEgASgJEhAKCGh0bWxfdXJsGAIgASgJEhYKDmRlZmF1bHRfYnJhbmNoGAMgASgJEg8KB3ByaXZhdGUYBCABKAgiNwohTGlzdENvbm5lY3Rpb25SZXBvc2l0b3JpZXNSZXF1ZXN0EhIKCmNvbm5lY3Rpb24YASABKAkiWgoiTGlzdENvbm5lY3Rpb25SZXBvc2l0b3JpZXNSZXNwb25zZRI0CgxyZXBvc2l0b3JpZXMYASADKAsyHi5rZWxzb24udjFhbHBoYTEuR2l0UmVwb3NpdG9yeSJHCh1MaXN0Q29ubmVjdGlvbkJyYW5jaGVzUmVxdWVzdBISCgpjb25uZWN0aW9uGAEgASgJEhIKCnJlcG9zaXRvcnkYAiABKAkiMgoeTGlzdENvbm5lY3Rpb25CcmFuY2hlc1Jlc3BvbnNlEhAKCGJyYW5jaGVzGAEgAygJKmMKC0dpdEF1dGhLaW5kEh0KGUdJVF9BVVRIX0tJTkRfVU5TUEVDSUZJRUQQABIcChhHSVRfQVVUSF9LSU5EX0dJVEhVQl9BUFAQARIXChNHSVRfQVVUSF9LSU5EX1RPS0VOEAIqfQoMR2l0T3duZXJLaW5kEh4KGkdJVF9PV05FUl9LSU5EX1VOU1BFQ0lGSUVEEAASGwoXR0lUX09XTkVSX0tJTkRfSU5TVEFOQ0UQARIXChNHSVRfT1dORVJfS0lORF9VU0VSEAISFwoTR0lUX09XTkVSX0tJTkRfVEVBTRADMpQGChRHaXRDb25uZWN0aW9uU2VydmljZRJkCg9MaXN0Q29ubmVjdGlvbnMSJy5rZWxzb24udjFhbHBoYTEuTGlzdENvbm5lY3Rpb25zUmVxdWVzdBooLmtlbHNvbi52MWFscGhhMS5MaXN0Q29ubmVjdGlvbnNSZXNwb25zZRJeCg1HZXRDb25uZWN0aW9uEiUua2Vsc29uLnYxYWxwaGExLkdldENvbm5lY3Rpb25SZXF1ZXN0GiYua2Vsc29uLnYxYWxwaGExLkdldENvbm5lY3Rpb25SZXNwb25zZRJnChBDcmVhdGVDb25uZWN0aW9uEigua2Vsc29uLnYxYWxwaGExLkNyZWF0ZUNvbm5lY3Rpb25SZXF1ZXN0Gikua2Vsc29uLnYxYWxwaGExLkNyZWF0ZUNvbm5lY3Rpb25SZXNwb25zZRJnChBEZWxldGVDb25uZWN0aW9uEigua2Vsc29uLnYxYWxwaGExLkRlbGV0ZUNvbm5lY3Rpb25SZXF1ZXN0Gikua2Vsc29uLnYxYWxwaGExLkRlbGV0ZUNvbm5lY3Rpb25SZXNwb25zZRJhCg5UZXN0Q29ubmVjdGlvbhImLmtlbHNvbi52MWFscGhhMS5UZXN0Q29ubmVjdGlvblJlcXVlc3QaJy5rZWxzb24udjFhbHBoYTEuVGVzdENvbm5lY3Rpb25SZXNwb25zZRKFAQoaTGlzdENvbm5lY3Rpb25SZXBvc2l0b3JpZXMSMi5rZWxzb24udjFhbHBoYTEuTGlzdENvbm5lY3Rpb25SZXBvc2l0b3JpZXNSZXF1ZXN0GjMua2Vsc29uLnYxYWxwaGExLkxpc3RDb25uZWN0aW9uUmVwb3NpdG9yaWVzUmVzcG9uc2USeQoWTGlzdENvbm5lY3Rpb25CcmFuY2hlcxIuLmtlbHNvbi52MWFscGhhMS5MaXN0Q29ubmVjdGlvbkJyYW5jaGVzUmVxdWVzdBovLmtlbHNvbi52MWFscGhhMS5MaXN0Q29ubmVjdGlvbkJyYW5jaGVzUmVzcG9uc2VCSlpIZ2l0aHViLmNvbS9kYWZyaWUva2Vsc29uL2ludGVybmFsL2FwaS9nZW4va2Vsc29uL3YxYWxwaGExO2tlbHNvbnYxYWxwaGExYgZwcm90bzM", [file_kelson_v1alpha1_common]);

/**
 * GitOwner is who may edit, rotate and delete a connection — ADR-0033
 * decision 6's model, recorded now so tenancy attaches to it rather than
 * migrating it.
 *
 * # Display-only until #231
 *
 * The rule this field encodes is "use is granted by visibility, mutation by
 * ownership": any project that can see a connection may build and preview
 * through it, and editing it belongs to its owner and to instance admins. There
 * is no principal to enforce that against yet — the interim shared password
 * (ADR-0013 §3) says a caller may reach the server and never says who it is —
 * so today every connection is effectively instance-scoped whatever this says.
 *
 * A client must not render it as a boundary. Showing "owned by alice" beside a
 * connection every operator can delete is authorization theatre, which is the
 * exact failure ADR-0031 refuses; ADR-0033 decision 6 asks for copy that says
 * "visible to everyone on this instance" until it is not. Enforcement arrives
 * with the subject it needs (#231), and the semantics do not change when it
 * does — that is what fixing them here bought.
 *
 * @generated from message kelson.v1alpha1.GitOwner
 */
export type GitOwner = Message<"kelson.v1alpha1.GitOwner"> & {
  /**
   * @generated from field: kelson.v1alpha1.GitOwnerKind kind = 1;
   */
  kind: GitOwnerKind;

  /**
   * Set when kind is USER or TEAM; empty for INSTANCE. Reserved until #231.
   *
   * @generated from field: string name = 2;
   */
  name: string;
};

/**
 * Describes the message kelson.v1alpha1.GitOwner.
 * Use `create(GitOwnerSchema)` to create a new message.
 */
export const GitOwnerSchema: GenMessage<GitOwner> = /*@__PURE__*/
  messageDesc(file_kelson_v1alpha1_gitconnection, 0);

/**
 * GitConnection is one connection as the server holds it: references and
 * provider-reported facts, and nothing else.
 *
 * The first block is what the CR declares. The second is what the controller
 * observed by asking the forge — populated only once a probe has run, and
 * `message` is what says which case an empty answer is.
 *
 * @generated from message kelson.v1alpha1.GitConnection
 */
export type GitConnection = Message<"kelson.v1alpha1.GitConnection"> & {
  /**
   * The CR's metadata.name in kelson-system, and the string
   * `Project.spec.source.connection` names when it disambiguates explicitly
   * (ADR-0033 decision 4). A DNS-1123 label.
   *
   * @generated from field: string name = 1;
   */
  name: string;

  /**
   * Which adapter serves this connection: "github" today, "generic" for a bare
   * token against any host. A string rather than an enum, matching
   * PreviewSettings.provider: the vocabulary of record is internal/forge's
   * provider registry, and it grows by writing an adapter, not by editing this
   * file (ADR-0033 decision 3).
   *
   * @generated from field: string provider = 2;
   */
  provider: string;

  /**
   * The forge base URL, e.g. https://github.com. Self-hosted GHE, GitLab and
   * Forgejo instances set their own; it is also half of the host-then-owner
   * match that resolves a project's source to a connection.
   *
   * @generated from field: string host = 3;
   */
  host: string;

  /**
   * @generated from field: kelson.v1alpha1.GitOwner owner = 4;
   */
  owner?: GitOwner | undefined;

  /**
   * @generated from field: kelson.v1alpha1.GitAuthKind auth_kind = 5;
   */
  authKind: GitAuthKind;

  /**
   * The GitHub App's numeric identifiers, zero unless auth_kind is
   * GITHUB_APP. installation_id stays zero between the manifest callback and
   * the `installation` webhook event that reports it — a connection created but
   * not yet installed on any repository, which is a real state a client shows
   * rather than an error.
   *
   * @generated from field: int64 app_id = 6;
   */
  appId: bigint;

  /**
   * @generated from field: int64 installation_id = 7;
   */
  installationId: bigint;

  /**
   * Name of the Secret in kelson-system holding this connection's material:
   * `privateKey`/`webhookSecret` for an app, `token`/`username` for a token.
   * A reference, never a value (ADR-0009, ADR-0033 decision 1).
   *
   * @generated from field: string secret_ref = 8;
   */
  secretRef: string;

  /**
   * Who the credential acts as, as the provider reported it — the app's
   * installation account, or the token's own login. Empty when no probe has
   * succeeded yet. It is the field that answers "is this the account I think it
   * is" without anyone reading the credential to find out.
   *
   * @generated from field: string account = 9;
   */
  account: string;

  /**
   * How many repositories the credential can see, as the provider reported it.
   * Zero for an installation with no repositories selected and zero for a
   * connection never probed; `message` is what separates them.
   *
   * @generated from field: int32 repositories = 10;
   */
  repositories: number;

  /**
   * The CR's Ready condition: the connection is well-formed and its Secret
   * exists with the keys its auth kind needs.
   *
   * @generated from field: bool ready = 11;
   */
  ready: boolean;

  /**
   * The CR's Reachable condition: the last probe authenticated against the
   * forge and got an answer.
   *
   * @generated from field: bool reachable = 12;
   */
  reachable: boolean;

  /**
   * Why the conditions say what they say, in prose — a missing Secret, a
   * revoked installation, an expired token, or "not yet observed" for a
   * connection no probe has reached. Both booleans are false before the first
   * probe as well as after a failed one, so a client that renders health
   * without this field cannot tell "broken" from "not looked at yet"
   * (ADR-0033: a new build-failure cause is only worth having if it is
   * diagnosable).
   *
   * @generated from field: string message = 13;
   */
  message: string;
};

/**
 * Describes the message kelson.v1alpha1.GitConnection.
 * Use `create(GitConnectionSchema)` to create a new message.
 */
export const GitConnectionSchema: GenMessage<GitConnection> = /*@__PURE__*/
  messageDesc(file_kelson_v1alpha1_gitconnection, 1);

/**
 * @generated from message kelson.v1alpha1.ListConnectionsRequest
 */
export type ListConnectionsRequest = Message<"kelson.v1alpha1.ListConnectionsRequest"> & {
};

/**
 * Describes the message kelson.v1alpha1.ListConnectionsRequest.
 * Use `create(ListConnectionsRequestSchema)` to create a new message.
 */
export const ListConnectionsRequestSchema: GenMessage<ListConnectionsRequest> = /*@__PURE__*/
  messageDesc(file_kelson_v1alpha1_gitconnection, 2);

/**
 * @generated from message kelson.v1alpha1.ListConnectionsResponse
 */
export type ListConnectionsResponse = Message<"kelson.v1alpha1.ListConnectionsResponse"> & {
  /**
   * Every connection the instance holds, sorted by name. Not filtered by
   * ownership: ADR-0033 decision 6 grants *use* by visibility, so a project can
   * resolve through any of them and a client that hid some would be describing
   * a different instance than the one that builds.
   *
   * @generated from field: repeated kelson.v1alpha1.GitConnection connections = 1;
   */
  connections: GitConnection[];
};

/**
 * Describes the message kelson.v1alpha1.ListConnectionsResponse.
 * Use `create(ListConnectionsResponseSchema)` to create a new message.
 */
export const ListConnectionsResponseSchema: GenMessage<ListConnectionsResponse> = /*@__PURE__*/
  messageDesc(file_kelson_v1alpha1_gitconnection, 3);

/**
 * @generated from message kelson.v1alpha1.GetConnectionRequest
 */
export type GetConnectionRequest = Message<"kelson.v1alpha1.GetConnectionRequest"> & {
  /**
   * @generated from field: string name = 1;
   */
  name: string;
};

/**
 * Describes the message kelson.v1alpha1.GetConnectionRequest.
 * Use `create(GetConnectionRequestSchema)` to create a new message.
 */
export const GetConnectionRequestSchema: GenMessage<GetConnectionRequest> = /*@__PURE__*/
  messageDesc(file_kelson_v1alpha1_gitconnection, 4);

/**
 * @generated from message kelson.v1alpha1.GetConnectionResponse
 */
export type GetConnectionResponse = Message<"kelson.v1alpha1.GetConnectionResponse"> & {
  /**
   * @generated from field: kelson.v1alpha1.GitConnection connection = 1;
   */
  connection?: GitConnection | undefined;
};

/**
 * Describes the message kelson.v1alpha1.GetConnectionResponse.
 * Use `create(GetConnectionResponseSchema)` to create a new message.
 */
export const GetConnectionResponseSchema: GenMessage<GetConnectionResponse> = /*@__PURE__*/
  messageDesc(file_kelson_v1alpha1_gitconnection, 5);

/**
 * CreateConnectionRequest creates a token-auth connection. There is no app
 * variant of this message; see this file's header for why the manifest flow's
 * HTTP callback owns that path instead.
 *
 * @generated from message kelson.v1alpha1.CreateConnectionRequest
 */
export type CreateConnectionRequest = Message<"kelson.v1alpha1.CreateConnectionRequest"> & {
  /**
   * The connection's name, a DNS-1123 label, and the CR's metadata.name.
   *
   * @generated from field: string name = 1;
   */
  name: string;

  /**
   * "github" for github.com or GHE with a PAT, "generic" for any other host.
   *
   * @generated from field: string provider = 2;
   */
  provider: string;

  /**
   * The forge base URL. Required: host is what resolves a project's source to
   * this connection, and a default would silently claim github.com.
   *
   * @generated from field: string host = 3;
   */
  host: string;

  /**
   * Name of an existing Secret in kelson-system holding `token` (and
   * optionally `username`). The caller creates it out of band or through
   * SecretService, and this request carries the name it chose — never the
   * token. A connection naming a Secret that does not exist is created and
   * reports Ready false with the reason, rather than being refused: the two
   * objects have separate lifecycles and either order must work.
   *
   * @generated from field: string secret_ref = 4;
   */
  secretRef: string;

  /**
   * Defaults to INSTANCE when unset, which is the only kind enforced today.
   *
   * @generated from field: kelson.v1alpha1.GitOwner owner = 5;
   */
  owner?: GitOwner | undefined;

  /**
   * RENDER validates the request and touches nothing. SERVER is a real
   * Kubernetes server-side dry-run apply: admission runs against the live
   * object and nothing is persisted.
   *
   * @generated from field: kelson.v1alpha1.DryRun dry_run = 6;
   */
  dryRun: DryRun;

  /**
   * Unlike SetSecret, which explains at length why it carries no key, this
   * request does (#69). Both of secret.proto's reasons fail here: a create is
   * not convergent the way a merge-apply is — a replay after a timeout would
   * otherwise answer "already exists" where the first call answered "created",
   * which is a different outcome for the same operation — and there *is*
   * somewhere to record the key, because the GitConnection CR is kelson's own
   * object to annotate, exactly as the spec store annotates the ConfigMap it
   * writes.
   *
   * @generated from field: string idempotency_key = 7;
   */
  idempotencyKey: string;
};

/**
 * Describes the message kelson.v1alpha1.CreateConnectionRequest.
 * Use `create(CreateConnectionRequestSchema)` to create a new message.
 */
export const CreateConnectionRequestSchema: GenMessage<CreateConnectionRequest> = /*@__PURE__*/
  messageDesc(file_kelson_v1alpha1_gitconnection, 6);

/**
 * @generated from message kelson.v1alpha1.CreateConnectionResponse
 */
export type CreateConnectionResponse = Message<"kelson.v1alpha1.CreateConnectionResponse"> & {
  /**
   * @generated from field: kelson.v1alpha1.GitConnection connection = 1;
   */
  connection?: GitConnection | undefined;

  /**
   * True when nothing was persisted because the request was a dry run.
   *
   * @generated from field: bool dry_run = 2;
   */
  dryRun: boolean;
};

/**
 * Describes the message kelson.v1alpha1.CreateConnectionResponse.
 * Use `create(CreateConnectionResponseSchema)` to create a new message.
 */
export const CreateConnectionResponseSchema: GenMessage<CreateConnectionResponse> = /*@__PURE__*/
  messageDesc(file_kelson_v1alpha1_gitconnection, 7);

/**
 * @generated from message kelson.v1alpha1.DeleteConnectionRequest
 */
export type DeleteConnectionRequest = Message<"kelson.v1alpha1.DeleteConnectionRequest"> & {
  /**
   * @generated from field: string name = 1;
   */
  name: string;

  /**
   * SERVER asks the API server to validate the delete and discard it. RENDER
   * validates the request alone.
   *
   * @generated from field: kelson.v1alpha1.DryRun dry_run = 2;
   */
  dryRun: DryRun;

  /**
   * @generated from field: string idempotency_key = 3;
   */
  idempotencyKey: string;
};

/**
 * Describes the message kelson.v1alpha1.DeleteConnectionRequest.
 * Use `create(DeleteConnectionRequestSchema)` to create a new message.
 */
export const DeleteConnectionRequestSchema: GenMessage<DeleteConnectionRequest> = /*@__PURE__*/
  messageDesc(file_kelson_v1alpha1_gitconnection, 8);

/**
 * @generated from message kelson.v1alpha1.DeleteConnectionResponse
 */
export type DeleteConnectionResponse = Message<"kelson.v1alpha1.DeleteConnectionResponse"> & {
  /**
   * False when the request was a dry run, which removes nothing.
   *
   * @generated from field: bool deleted = 1;
   */
  deleted: boolean;

  /**
   * Projects that resolve their source through this connection, by name. The
   * delete is not blocked by them — a connection is deleted because it is
   * wrong, and refusing until every project is edited would make a leaked
   * credential harder to revoke than to keep — but the builds that will start
   * failing are named rather than discovered later (ADR-0033: "one more thing
   * between a user and a build").
   *
   * @generated from field: repeated string affected_projects = 2;
   */
  affectedProjects: string[];
};

/**
 * Describes the message kelson.v1alpha1.DeleteConnectionResponse.
 * Use `create(DeleteConnectionResponseSchema)` to create a new message.
 */
export const DeleteConnectionResponseSchema: GenMessage<DeleteConnectionResponse> = /*@__PURE__*/
  messageDesc(file_kelson_v1alpha1_gitconnection, 9);

/**
 * @generated from message kelson.v1alpha1.TestConnectionRequest
 */
export type TestConnectionRequest = Message<"kelson.v1alpha1.TestConnectionRequest"> & {
  /**
   * @generated from field: string name = 1;
   */
  name: string;
};

/**
 * Describes the message kelson.v1alpha1.TestConnectionRequest.
 * Use `create(TestConnectionRequestSchema)` to create a new message.
 */
export const TestConnectionRequestSchema: GenMessage<TestConnectionRequest> = /*@__PURE__*/
  messageDesc(file_kelson_v1alpha1_gitconnection, 10);

/**
 * TestConnectionResponse is one probe's answer. It is the same question the
 * controller's Reachable condition answers, asked on demand.
 *
 * @generated from message kelson.v1alpha1.TestConnectionResponse
 */
export type TestConnectionResponse = Message<"kelson.v1alpha1.TestConnectionResponse"> & {
  /**
   * Whether the forge authenticated the credential and answered.
   *
   * @generated from field: bool reachable = 1;
   */
  reachable: boolean;

  /**
   * Who the credential acts as, per the provider. Empty when unreachable.
   *
   * @generated from field: string account = 2;
   */
  account: string;

  /**
   * How many repositories the credential can see, per the provider. Zero when
   * unreachable, and zero for a real installation with nothing selected.
   *
   * @generated from field: int32 repositories = 3;
   */
  repositories: number;

  /**
   * The forge's refusal in prose when unreachable, and a one-line summary of
   * what answered when it is. This is the field an operator reads to tell a
   * revoked installation from a network path that does not exist.
   *
   * @generated from field: string message = 4;
   */
  message: string;
};

/**
 * Describes the message kelson.v1alpha1.TestConnectionResponse.
 * Use `create(TestConnectionResponseSchema)` to create a new message.
 */
export const TestConnectionResponseSchema: GenMessage<TestConnectionResponse> = /*@__PURE__*/
  messageDesc(file_kelson_v1alpha1_gitconnection, 11);

/**
 * GitRepository is one repository a connection can see, reduced to what a
 * picker needs: a row to show, and the fields that fill in a Project's
 * `spec.source`. It mirrors internal/forge's own Repo, which is deliberately
 * this small — whatever else a forge reports about a repository is that forge's
 * business and stops at the adapter.
 *
 * @generated from message kelson.v1alpha1.GitRepository
 */
export type GitRepository = Message<"kelson.v1alpha1.GitRepository"> & {
  /**
   * "owner/name" as the forge spells it, which is the key ListConnectionBranches
   * takes back.
   *
   * @generated from field: string full_name = 1;
   */
  fullName: string;

  /**
   * The browser URL, and the value that goes into `Project.spec.source.git`.
   * It is the forge's own, not one assembled from host and full_name: a
   * self-hosted instance serving repositories under a path prefix would have
   * the assembled one point at nothing.
   *
   * @generated from field: string html_url = 2;
   */
  htmlUrl: string;

  /**
   * The branch a clone lands on when nothing asks for another — what a picker
   * preselects, and what leaving `spec.source.ref` empty resolves to.
   *
   * @generated from field: string default_branch = 3;
   */
  defaultBranch: string;

  /**
   * Whether reading it needs the credential at all. A picker shows it as a
   * badge; nothing else in kelson branches on it, because a connection that
   * can see a repository can clone it whichever this says.
   *
   * @generated from field: bool private = 4;
   */
  private: boolean;
};

/**
 * Describes the message kelson.v1alpha1.GitRepository.
 * Use `create(GitRepositorySchema)` to create a new message.
 */
export const GitRepositorySchema: GenMessage<GitRepository> = /*@__PURE__*/
  messageDesc(file_kelson_v1alpha1_gitconnection, 12);

/**
 * ListConnectionRepositoriesRequest names the connection to browse. It is
 * `connection` and not `name` because the answer is about repositories rather
 * than about the connection — the same word Project.spec.source.connection uses
 * for the same reference.
 *
 * @generated from message kelson.v1alpha1.ListConnectionRepositoriesRequest
 */
export type ListConnectionRepositoriesRequest = Message<"kelson.v1alpha1.ListConnectionRepositoriesRequest"> & {
  /**
   * @generated from field: string connection = 1;
   */
  connection: string;
};

/**
 * Describes the message kelson.v1alpha1.ListConnectionRepositoriesRequest.
 * Use `create(ListConnectionRepositoriesRequestSchema)` to create a new message.
 */
export const ListConnectionRepositoriesRequestSchema: GenMessage<ListConnectionRepositoriesRequest> = /*@__PURE__*/
  messageDesc(file_kelson_v1alpha1_gitconnection, 13);

/**
 * @generated from message kelson.v1alpha1.ListConnectionRepositoriesResponse
 */
export type ListConnectionRepositoriesResponse = Message<"kelson.v1alpha1.ListConnectionRepositoriesResponse"> & {
  /**
   * Every repository the credential can see, in the order the provider
   * reported them. An installation lists exactly the repositories it was
   * granted; a token lists everything its owner can reach, which is a wider and
   * less deliberate set — ADR-0033 decision 2's argument for the app, visible
   * here as the difference between a short list and a long one.
   *
   * @generated from field: repeated kelson.v1alpha1.GitRepository repositories = 1;
   */
  repositories: GitRepository[];
};

/**
 * Describes the message kelson.v1alpha1.ListConnectionRepositoriesResponse.
 * Use `create(ListConnectionRepositoriesResponseSchema)` to create a new message.
 */
export const ListConnectionRepositoriesResponseSchema: GenMessage<ListConnectionRepositoriesResponse> = /*@__PURE__*/
  messageDesc(file_kelson_v1alpha1_gitconnection, 14);

/**
 * @generated from message kelson.v1alpha1.ListConnectionBranchesRequest
 */
export type ListConnectionBranchesRequest = Message<"kelson.v1alpha1.ListConnectionBranchesRequest"> & {
  /**
   * @generated from field: string connection = 1;
   */
  connection: string;

  /**
   * "owner/name", as GitRepository.full_name reported it.
   *
   * @generated from field: string repository = 2;
   */
  repository: string;
};

/**
 * Describes the message kelson.v1alpha1.ListConnectionBranchesRequest.
 * Use `create(ListConnectionBranchesRequestSchema)` to create a new message.
 */
export const ListConnectionBranchesRequestSchema: GenMessage<ListConnectionBranchesRequest> = /*@__PURE__*/
  messageDesc(file_kelson_v1alpha1_gitconnection, 15);

/**
 * @generated from message kelson.v1alpha1.ListConnectionBranchesResponse
 */
export type ListConnectionBranchesResponse = Message<"kelson.v1alpha1.ListConnectionBranchesResponse"> & {
  /**
   * Branch names only. A picker needs a name to write into `spec.source.ref`
   * and the ref resolver needs nothing from here at all, so the commit each
   * branch points at is deliberately absent: it would be stale by the time it
   * was read, and reading it is what BuildService does at build time.
   *
   * @generated from field: repeated string branches = 1;
   */
  branches: string[];
};

/**
 * Describes the message kelson.v1alpha1.ListConnectionBranchesResponse.
 * Use `create(ListConnectionBranchesResponseSchema)` to create a new message.
 */
export const ListConnectionBranchesResponseSchema: GenMessage<ListConnectionBranchesResponse> = /*@__PURE__*/
  messageDesc(file_kelson_v1alpha1_gitconnection, 16);

/**
 * GitAuthKind is which of ADR-0033 decision 2's two auth shapes a connection
 * carries. The CR spells it as exactly-one-of `auth.githubApp` / `auth.token`;
 * on the wire it is a discriminator plus the fields each shape populates,
 * because a client renders "GitHub App · acme (42 repositories)" from the same
 * message whichever shape answered.
 *
 * @generated from enum kelson.v1alpha1.GitAuthKind
 */
export enum GitAuthKind {
  /**
   * @generated from enum value: GIT_AUTH_KIND_UNSPECIFIED = 0;
   */
  UNSPECIFIED = 0,

  /**
   * A per-instance GitHub App created by the manifest flow. Short-lived
   * installation tokens are minted from its key per operation; the key itself
   * never leaves the instance that minted it.
   *
   * @generated from enum value: GIT_AUTH_KIND_GITHUB_APP = 1;
   */
  GITHUB_APP = 1,

  /**
   * A PAT, project token or deploy token in a Secret — the universal fallback,
   * and the only path for a forge whose adapter has not landed.
   *
   * @generated from enum value: GIT_AUTH_KIND_TOKEN = 2;
   */
  TOKEN = 2,
}

/**
 * Describes the enum kelson.v1alpha1.GitAuthKind.
 */
export const GitAuthKindSchema: GenEnum<GitAuthKind> = /*@__PURE__*/
  enumDesc(file_kelson_v1alpha1_gitconnection, 0);

/**
 * GitOwnerKind is the discriminator of ADR-0033 decision 6's ownership
 * reference.
 *
 * @generated from enum kelson.v1alpha1.GitOwnerKind
 */
export enum GitOwnerKind {
  /**
   * @generated from enum value: GIT_OWNER_KIND_UNSPECIFIED = 0;
   */
  UNSPECIFIED = 0,

  /**
   * Owned by the instance: the day-one shape, and the only one enforced today.
   *
   * @generated from enum value: GIT_OWNER_KIND_INSTANCE = 1;
   */
  INSTANCE = 1,

  /**
   * Reserved for tenancy (#231). Stored, shown and validated; not a boundary.
   *
   * @generated from enum value: GIT_OWNER_KIND_USER = 2;
   */
  USER = 2,

  /**
   * @generated from enum value: GIT_OWNER_KIND_TEAM = 3;
   */
  TEAM = 3,
}

/**
 * Describes the enum kelson.v1alpha1.GitOwnerKind.
 */
export const GitOwnerKindSchema: GenEnum<GitOwnerKind> = /*@__PURE__*/
  enumDesc(file_kelson_v1alpha1_gitconnection, 1);

/**
 * GitConnectionService is gated by the server's authentication like every other
 * service in this schema: the middleware matches on the `/kelson.v1alpha1.`
 * route prefix, so a service added here is covered by construction rather than
 * by remembering to add it (issue #84's interim cut).
 *
 * @generated from service kelson.v1alpha1.GitConnectionService
 */
export const GitConnectionService: GenService<{
  /**
   * ListConnections reports every connection: what this cluster can pull from,
   * which is `kubectl get gitconnections` for a caller that has no kubeconfig.
   *
   * @generated from rpc kelson.v1alpha1.GitConnectionService.ListConnections
   */
  listConnections: {
    methodKind: "unary";
    input: typeof ListConnectionsRequestSchema;
    output: typeof ListConnectionsResponseSchema;
  },
  /**
   * GetConnection reports one, by name.
   *
   * @generated from rpc kelson.v1alpha1.GitConnectionService.GetConnection
   */
  getConnection: {
    methodKind: "unary";
    input: typeof GetConnectionRequestSchema;
    output: typeof GetConnectionResponseSchema;
  },
  /**
   * CreateConnection creates a token-auth connection. App connections come from
   * the manifest flow's HTTP callback and not from here (ADR-0033 decision 2).
   *
   * @generated from rpc kelson.v1alpha1.GitConnectionService.CreateConnection
   */
  createConnection: {
    methodKind: "unary";
    input: typeof CreateConnectionRequestSchema;
    output: typeof CreateConnectionResponseSchema;
  },
  /**
   * DeleteConnection removes the connection. It does not remove the Secret it
   * references: kelson did not create that Secret for a token connection and
   * deleting somebody else's object on the way out is not this call's to do.
   * An app connection's Secret *was* kelson's to write, and reclaiming it is
   * the manifest flow's own decision to record, not this RPC's to assume.
   *
   * @generated from rpc kelson.v1alpha1.GitConnectionService.DeleteConnection
   */
  deleteConnection: {
    methodKind: "unary";
    input: typeof DeleteConnectionRequestSchema;
    output: typeof DeleteConnectionResponseSchema;
  },
  /**
   * TestConnection probes the forge, live, server-side, using the Secret the
   * connection references — an authenticated call to the provider's API, made
   * now rather than read from a condition written at some earlier reconcile.
   *
   * It reads nothing back to the caller that the credential itself would
   * reveal: the answer is reachability, the account name and a count, which is
   * the same triple the status subresource carries. The credential is used and
   * never returned, and any token minted to make the call is registered with
   * internal/redact before it is used.
   *
   * @generated from rpc kelson.v1alpha1.GitConnectionService.TestConnection
   */
  testConnection: {
    methodKind: "unary";
    input: typeof TestConnectionRequestSchema;
    output: typeof TestConnectionResponseSchema;
  },
  /**
   * ListConnectionRepositories reports the repositories this connection's
   * credential can see — the New Project repository picker's read, made
   * server-side with the Secret the connection references.
   *
   * # It is capability-gated, and the gate is part of the answer
   *
   * Repository browsing is `RepoBrowser`, optional in ADR-0033 decision 3. A
   * connection whose provider implements it is served; one whose provider does
   * not is refused with `connection/capability-unsupported`, naming what the
   * connection *can* do and saying that pasting the repository URL works for
   * every connection there is. It is not an empty list, because an empty list
   * is what a real installation with nothing selected looks like, and it is not
   * a fault of the connection: a `generic` token connection that cannot be
   * browsed still clones private repositories, which is the capability a deploy
   * needs.
   *
   * Read-only, and it changes nothing: no status is written, and no credential
   * reaches the response — GitRepository has no field one would fit in. Any
   * token minted to make the call is registered with internal/redact before it
   * is used, exactly as TestConnection's is.
   *
   * @generated from rpc kelson.v1alpha1.GitConnectionService.ListConnectionRepositories
   */
  listConnectionRepositories: {
    methodKind: "unary";
    input: typeof ListConnectionRepositoriesRequestSchema;
    output: typeof ListConnectionRepositoriesResponseSchema;
  },
  /**
   * ListConnectionBranches reports one repository's branches, for the picker's
   * second step. Same capability gate and same refusal as
   * ListConnectionRepositories — they are the two halves of one `RepoBrowser`,
   * and a connection that answered the first will answer this one — and the
   * same read-only posture.
   *
   * The repository is named as "owner/name" rather than as a URL: it is the key
   * the listing above already reported, and re-deriving it from a URL here
   * would be a second parser disagreeing with the first.
   *
   * @generated from rpc kelson.v1alpha1.GitConnectionService.ListConnectionBranches
   */
  listConnectionBranches: {
    methodKind: "unary";
    input: typeof ListConnectionBranchesRequestSchema;
    output: typeof ListConnectionBranchesResponseSchema;
  },
}> = /*@__PURE__*/
  serviceDesc(file_kelson_v1alpha1_gitconnection, 0);

