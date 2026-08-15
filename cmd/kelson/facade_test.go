package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1/kelsonv1alpha1connect"
)

// fakeDeployService is a DeployService double for deploy/rollback/promote/
// history: the command tests drive it over a real HTTP server, the same
// reason internal/api's own tests do (internal/api/fake_test.go) — the
// schema, the codec and the streaming framing are exercised too, not just the
// command's own Go. A nil field falls back to
// UnimplementedDeployServiceHandler, so a test only wires the one RPC it
// exercises.
type fakeDeployService struct {
	kelsonv1alpha1connect.UnimplementedDeployServiceHandler
	deploy   func(context.Context, *kelsonv1alpha1.DeployRequest, *connect.ServerStream[kelsonv1alpha1.DeployResponse]) error
	rollback func(context.Context, *kelsonv1alpha1.RollbackRequest, *connect.ServerStream[kelsonv1alpha1.RollbackResponse]) error
	promote  func(context.Context, *kelsonv1alpha1.PromoteRequest) (*kelsonv1alpha1.PromoteResponse, error)
	history  func(context.Context, *kelsonv1alpha1.HistoryRequest) (*kelsonv1alpha1.HistoryResponse, error)
}

func (f *fakeDeployService) Deploy(ctx context.Context, req *connect.Request[kelsonv1alpha1.DeployRequest], stream *connect.ServerStream[kelsonv1alpha1.DeployResponse]) error {
	if f.deploy == nil {
		return f.UnimplementedDeployServiceHandler.Deploy(ctx, req, stream)
	}
	return f.deploy(ctx, req.Msg, stream)
}

func (f *fakeDeployService) Rollback(ctx context.Context, req *connect.Request[kelsonv1alpha1.RollbackRequest], stream *connect.ServerStream[kelsonv1alpha1.RollbackResponse]) error {
	if f.rollback == nil {
		return f.UnimplementedDeployServiceHandler.Rollback(ctx, req, stream)
	}
	return f.rollback(ctx, req.Msg, stream)
}

func (f *fakeDeployService) Promote(ctx context.Context, req *connect.Request[kelsonv1alpha1.PromoteRequest]) (*connect.Response[kelsonv1alpha1.PromoteResponse], error) {
	if f.promote == nil {
		return f.UnimplementedDeployServiceHandler.Promote(ctx, req)
	}
	res, err := f.promote(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(res), nil
}

func (f *fakeDeployService) History(ctx context.Context, req *connect.Request[kelsonv1alpha1.HistoryRequest]) (*connect.Response[kelsonv1alpha1.HistoryResponse], error) {
	if f.history == nil {
		return f.UnimplementedDeployServiceHandler.History(ctx, req)
	}
	res, err := f.history(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(res), nil
}

// serveFakeDeployService starts an httptest server over the fake and returns
// its base URL, ready to hand to --server. httptest.NewServer speaks
// HTTP/1.1, which is what lets the deploy/rollback streams work with no h2c —
// the same property internal/api/fake_test.go's serve() relies on.
func serveFakeDeployService(t *testing.T, fake *fakeDeployService) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(kelsonv1alpha1connect.NewDeployServiceHandler(fake))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// unreachableServerAddr returns an address nothing listens on: a real
// httptest server, closed immediately. Unlike a made-up host:port this is
// guaranteed free on the machine running the test, and the connection is
// refused immediately rather than needing the dial timeout to expire, so
// tests that assert "fails fast, not a hang" do not have to wait dialTimeout
// out to prove it.
func unreachableServerAddr(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	return srv.URL
}
