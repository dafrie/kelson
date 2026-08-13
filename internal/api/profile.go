package api

import (
	"context"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/clusterprofile"
)

// GetProfile captures the server's cluster as a ClusterProfile.
//
// The gaps travel twice on purpose: inside the YAML document, where a later
// render acts on them, and as ProfileGap messages, so a client can show what
// could not be read without parsing the document. What detection could not see
// is data either way, never a warning on a side channel — a caller that ignores
// the gaps must not be able to mistake "could not check" for "absent" (#56).
func (s *Server) GetProfile(ctx context.Context, _ *connect.Request[kelsonv1alpha1.GetProfileRequest]) (*connect.Response[kelsonv1alpha1.GetProfileResponse], error) {
	if s.profile == nil {
		return nil, unimplemented("live profile capture")
	}
	profile, err := s.profile.Capture(ctx)
	if err != nil {
		return nil, fail(connect.CodeUnavailable, err)
	}
	data, err := clusterprofile.Marshal(profile)
	if err != nil {
		return nil, fail(connect.CodeInternal, err)
	}
	gaps := make([]*kelsonv1alpha1.ProfileGap, 0, len(profile.Incomplete))
	for _, g := range profile.Incomplete {
		gaps = append(gaps, &kelsonv1alpha1.ProfileGap{Field: g.Field, Reason: g.Reason})
	}
	return connect.NewResponse(&kelsonv1alpha1.GetProfileResponse{Yaml: data, Gaps: gaps}), nil
}
