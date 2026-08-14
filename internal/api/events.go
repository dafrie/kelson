package api

import (
	"context"
	"fmt"
	"sort"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
)

// Watch streams what changed, so an agent and the UI can react instead of
// polling (issue #76).
//
// The handler itself is a mailbox reader: everything that decides *what* an
// event is lives in the broker (broker.go), which observes each scope through
// the same seams DeployService.Status uses. What is decided here is the shape
// of one subscription — which scopes, which types, from which cursor — and the
// one rule that makes the stream trustworthy: a Resync goes out before
// anything else the watcher is owed, because it is the statement that what
// would have come before it is gone.
func (s *Server) Watch(ctx context.Context, req *connect.Request[kelsonv1alpha1.WatchRequest], stream *connect.ServerStream[kelsonv1alpha1.WatchResponse]) error {
	if s.specs == nil {
		return unimplemented("the spec store")
	}
	if s.delivery == nil {
		return unimplemented("the delivery plane")
	}
	scopes, err := s.watchScopes(ctx, req.Msg.GetScopes())
	if err != nil {
		return failRequest(err)
	}
	if len(scopes) == 0 {
		// Nothing is stored, or the named projects declare no environments. An
		// open stream over nothing looks live and never is, so the honest
		// answer is to complete: the client re-watches once it has created
		// something (the same re-watch the schema asks for after a create).
		return nil
	}

	w := s.events.subscribe(scopes, req.Msg.GetTypes(), req.Msg.GetCursor())
	defer s.events.unsubscribe(w)

	for {
		reason, batch := w.take()
		if reason != "" {
			if err := stream.Send(&kelsonv1alpha1.WatchResponse{
				Body: &kelsonv1alpha1.WatchResponse_Resync_{
					Resync: &kelsonv1alpha1.WatchResponse_Resync{Reason: reason},
				},
			}); err != nil {
				return err
			}
		}
		for _, ev := range batch {
			if err := stream.Send(&kelsonv1alpha1.WatchResponse{
				Body: &kelsonv1alpha1.WatchResponse_Event_{Event: ev.wire(s.events.cursor(ev.seq))},
			}); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			// The client went away or the server is shutting down. Neither is a
			// failure of this RPC, and the deferred unsubscribe is what stops
			// the pollers this watcher was keeping alive.
			return nil
		case <-w.notify:
		}
	}
}

// watchScopes resolves the requested scopes to concrete (project, environment)
// pairs, once, at the start of the watch.
//
// Resolving once is the v0 contract the schema states: a project created after
// the watch began is not added to it. The alternative is a re-list on a timer,
// which would make coverage a function of when a poll happened to fire — a
// watch that half-covers is worse than one whose coverage is stated, and an
// agent that creates a project re-watches.
func (s *Server) watchScopes(ctx context.Context, requested []*kelsonv1alpha1.WatchRequest_Scope) ([]Scope, error) {
	seen := map[Scope]bool{}
	var out []Scope
	add := func(sc Scope) {
		if !seen[sc] {
			seen[sc] = true
			out = append(out, sc)
		}
	}

	if len(requested) == 0 {
		stored, err := s.specs.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, spec := range stored {
			for _, env := range spec.Environments {
				add(Scope{Project: spec.Project, Environment: env})
			}
		}
		return sortScopes(out), nil
	}

	for _, sc := range requested {
		project := sc.GetProject()
		if project == "" {
			return nil, fmt.Errorf("api: a watch scope must name a project; send no scopes at all to watch every stored project")
		}
		if env := sc.GetEnvironment(); env != "" {
			add(Scope{Project: project, Environment: env})
			continue
		}
		spec, err := s.specs.Get(ctx, project)
		if err != nil {
			return nil, err
		}
		for _, env := range spec.Environments {
			add(Scope{Project: project, Environment: env})
		}
	}
	return sortScopes(out), nil
}

func sortScopes(scopes []Scope) []Scope {
	sort.Slice(scopes, func(i, j int) bool {
		if scopes[i].Project != scopes[j].Project {
			return scopes[i].Project < scopes[j].Project
		}
		return scopes[i].Environment < scopes[j].Environment
	})
	return scopes
}

// observeScope is the broker's production observer: exactly the pipeline
// Status runs, reduced to the fields events are diffed on.
//
// It renders the stored spec with no image override and no cluster profile,
// which is what a client calling Status without either gets — the UI's own
// call (ui/src/pages/ProjectsPage.tsx). That parity is the point: a scope whose
// Status a client cannot read has no events either, rather than a second,
// quieter definition of what this environment's state is.
func (s *Server) observeScope(ctx context.Context, sc Scope) (Snapshot, error) {
	out, err := s.renderSpec(ctx,
		&kelsonv1alpha1.SpecRef{Spec: &kelsonv1alpha1.SpecRef_Project{Project: sc.Project}},
		sc.Environment, "", nil)
	if err != nil {
		return Snapshot{}, err
	}
	set, err := manifestSet(out)
	if err != nil {
		return Snapshot{}, err
	}
	t := target(out, "")
	adapter, plane, err := s.selectAdapter(ctx, t)
	if err != nil {
		return Snapshot{}, err
	}
	st, err := adapter.Status(ctx, set)
	if err != nil {
		return Snapshot{}, err
	}
	verdicts, err := workloadVerdicts(ctx, plane, set, t.Namespace)
	if err != nil {
		return Snapshot{}, err
	}

	snap := Snapshot{Phase: string(st.Phase), Revision: st.Revision, Cause: st.Cause}
	for _, v := range verdicts {
		snap.Health = append(snap.Health, HealthState{
			Resource: v.GetResource(),
			Code:     v.GetCode(),
			Healthy:  v.GetHealthy(),
			Message:  v.GetMessage(),
		})
	}
	return snap, nil
}
