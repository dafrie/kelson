package mcp

import (
	"fmt"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
)

// TestPutSpecValidatesByDefault: dry_run defaults to true, which stores
// nothing, and the request that reaches the server says so.
func TestPutSpecValidatesByDefault(t *testing.T) {
	var request *kelsonv1alpha1.PutSpecRequest
	h := start(t, &fakeServer{
		putSpec: func(req *kelsonv1alpha1.PutSpecRequest) (*kelsonv1alpha1.PutSpecResponse, error) {
			request = req
			return &kelsonv1alpha1.PutSpecResponse{Spec: &kelsonv1alpha1.Spec{
				Project: "hello", Environments: []string{"development"},
			}}, nil
		},
	})

	out := h.call(t, "put_spec", map[string]any{
		"project_document":      projectDoc,
		"environment_documents": map[string]any{"development": "apiVersion: kelson.dev/v1alpha1\nkind: Environment\n"},
	})
	mustContain(t, out, "VALID", "Nothing was stored", "dry_run=false", "environments  development")
	if request.GetDryRun() != kelsonv1alpha1.DryRun_DRY_RUN_RENDER {
		t.Errorf("dry run = %v, want DRY_RUN_RENDER by default", request.GetDryRun())
	}
	if string(request.GetDocuments().GetProject()) != projectDoc {
		t.Error("the Project document must reach the server byte-faithfully")
	}
	if _, ok := request.GetDocuments().GetEnvironments()["development"]; !ok {
		t.Errorf("the environment documents did not reach the server: %v", request.GetDocuments().GetEnvironments())
	}
}

// TestPutSpecRendersValidationErrorsVerbatim: a rejected spec is an answer, not
// a transport failure, and every field of the taxonomy travels — an agent
// branches on the code and acts on the remediation.
func TestPutSpecRendersValidationErrorsVerbatim(t *testing.T) {
	h := start(t, &fakeServer{
		putSpec: func(*kelsonv1alpha1.PutSpecRequest) (*kelsonv1alpha1.PutSpecResponse, error) {
			return &kelsonv1alpha1.PutSpecResponse{Errors: []*kelsonv1alpha1.Error{
				{
					Code: "schema/unknown-field", Resource: "Project/hello", Field: "$.spec.components[0].prot",
					Message: "unknown field prot", Remediation: "did you mean port?",
					DocsUrl: "https://kelson.dev/model/errors#schema-unknown-field", Line: 9, Column: 7,
				},
				{Code: "image/unresolved", Application: "worker", Message: "no image and no source", Remediation: "set spec.image or spec.source"},
			}}, nil
		},
	})

	out := h.call(t, "put_spec", map[string]any{"project_document": projectDoc})
	mustContain(t, out,
		"REJECTED with 2 error(s). Nothing was stored.",
		"code: schema/unknown-field",
		"field: $.spec.components[0].prot",
		"remediation: did you mean port?",
		"docs_url: https://kelson.dev/model/errors#schema-unknown-field",
		"position: line 9, column 7",
		"code: image/unresolved",
		"application: worker",
	)
}

// TestPutSpecStores: dry_run=false stores and returns the version the next
// write must carry.
func TestPutSpecStores(t *testing.T) {
	var request *kelsonv1alpha1.PutSpecRequest
	h := start(t, &fakeServer{
		putSpec: func(req *kelsonv1alpha1.PutSpecRequest) (*kelsonv1alpha1.PutSpecResponse, error) {
			request = req
			return &kelsonv1alpha1.PutSpecResponse{Spec: &kelsonv1alpha1.Spec{
				Project: "hello", Version: "12345", Environments: []string{"development", "production"},
			}}, nil
		},
	})

	out := h.call(t, "put_spec", map[string]any{
		"project_document": projectDoc, "dry_run": false, "version": "12344",
	})
	mustContain(t, out, "STORED", "version       12345", "pass it back on the next put_spec", "call deploy to apply it")
	if request.GetDryRun() != kelsonv1alpha1.DryRun_DRY_RUN_NONE {
		t.Errorf("dry run = %v, want DRY_RUN_NONE when dry_run=false", request.GetDryRun())
	}
	if request.GetVersion() != "12344" {
		t.Errorf("version = %q, want the caller's version carried to the store", request.GetVersion())
	}
}

// TestPutSpecVersionConflict: the store's own error arrives as a ConnectRPC
// failure with the structured detail attached, and the tool relays the code and
// the fix rather than paraphrasing them.
func TestPutSpecVersionConflict(t *testing.T) {
	h := start(t, &fakeServer{
		putSpec: func(*kelsonv1alpha1.PutSpecRequest) (*kelsonv1alpha1.PutSpecResponse, error) {
			err := connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("serverstate: version mismatch"))
			detail, derr := connect.NewErrorDetail(&kelsonv1alpha1.Error{
				Code: "store/version-conflict", Resource: "spec/hello",
				Message: "the stored spec changed since this read", Remediation: "re-read the spec and retry with its version",
			})
			if derr != nil {
				return nil, derr
			}
			err.AddDetail(detail)
			return nil, err
		},
	})

	out := h.callErr(t, "put_spec", map[string]any{"project_document": projectDoc, "dry_run": false})
	mustContain(t, out,
		"kelson.v1alpha1.SpecService.PutSpec failed: failed_precondition",
		"code: store/version-conflict",
		"remediation: re-read the spec and retry with its version",
	)
}

// TestPutSpecNeedsAProjectDocument: the one input without which nothing can be
// validated is checked before a call is made.
func TestPutSpecNeedsAProjectDocument(t *testing.T) {
	h := start(t, &fakeServer{})
	out := h.callErr(t, "put_spec", map[string]any{"project_document": ""})
	mustContain(t, out, "put_spec needs project_document")
	if calls := h.fake.procedures(); len(calls) != 0 {
		t.Errorf("nothing should reach the server; calls = %v", calls)
	}
}
