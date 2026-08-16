//go:build e2e

// The server smoke: the pod the rest of this suite never needed is scheduled,
// serving, allowed to write, and allowed to watch.
//
// It exists because three chart-vs-server skews shipped green in one day — a
// flag the binary had dropped (`--keep`), a missing `create` on projects, a
// missing `watch` on environments — and every one was invisible here while the
// chart was installed with replicaCount=0. spine.sh now runs the server pod
// from an image built in this harness; this test drives the three paths those
// bugs lived on, through the rendered chart's own Service, RBAC and args:
//
//   - the pod is serving at all (a refused flag is a pod that never was),
//   - PutSpec creates the FIRST project (server-side apply needs `create`),
//   - the event stream opens (the store's watch needs `watch`).
//
// It speaks plain HTTP against the ConnectRPC endpoints rather than importing
// the generated client, for the suite's standing reason: the command plane's
// depguard forbids a Kubernetes client here, and a smoke that speaks the same
// wire a browser does proves the deployment, not the codegen.
package e2e

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const smokeProject = "smoke"

// forwardServer opens a kubectl port-forward to the chart's own Service — the
// same door a browser walks through — and returns the local base URL. The
// pattern is registryProxy's: port 0, announce-line parsing, output drained
// for the life of the process, killed on cleanup.
func forwardServer(t *testing.T) string {
	t.Helper()
	logs := &safeBuffer{}
	cmd := exec.Command("kubectl", "-n", "kelson-system", "port-forward", "svc/kelson", ":8420")
	cmd.Env = childEnv()
	cmd.Stderr = logs
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("opening a pipe to kubectl port-forward: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting kubectl port-forward to svc/kelson: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	ports := make(chan string, 1)
	go func() {
		defer func() { _ = stdout.Close() }()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			_, _ = logs.Write([]byte(line + "\n"))
			if rest, ok := strings.CutPrefix(line, "Forwarding from 127.0.0.1:"); ok {
				port, _, _ := strings.Cut(rest, " ")
				select {
				case ports <- port:
				default:
				}
			}
		}
	}()

	select {
	case port := <-ports:
		return "http://127.0.0.1:" + port
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("kubectl port-forward to svc/kelson never announced a local port in 60s\n%s", logs.String())
		return ""
	}
}

func smokeDocuments() map[string]any {
	project := fmt.Sprintf(`apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: %s}
spec:
  image: ghcr.io/acme/smoke:1
  components:
    - {name: web, port: 8080}
`, smokeProject)
	environment := fmt.Sprintf(`apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: e2e}
spec:
  project: %s
`, smokeProject)
	return map[string]any{
		"project": base64.StdEncoding.EncodeToString([]byte(project)),
		"environments": map[string]string{
			"e2e": base64.StdEncoding.EncodeToString([]byte(environment)),
		},
	}
}

func TestServerSmoke(t *testing.T) {
	if !enabled {
		t.Skipf("%s is not 1: set %s=1 with a reachable cluster to run the end-to-end suite", enableEnv, enableEnv)
	}
	base := forwardServer(t)

	post := func(procedure string, body any) (*http.Response, []byte) {
		t.Helper()
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshalling the %s request: %v", procedure, err)
		}
		resp, err := http.Post(base+procedure, "application/json", bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("POST %s: %v", procedure, err)
		}
		defer func() { _ = resp.Body.Close() }()
		answer, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("reading the %s response: %v", procedure, err)
		}
		return resp, answer
	}

	// The first project. Force, because a re-run of this suite finds its own
	// leftovers, and the smoke is about the write path, not idempotency.
	resp, answer := post("/kelson.v1alpha1.SpecService/PutSpec", map[string]any{
		"documents": smokeDocuments(),
		"force":     true,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PutSpec answered %d — the create path is broken (a missing RBAC verb reads "+
			"exactly like this): %s", resp.StatusCode, answer)
	}
	var put struct {
		Spec   struct{ Project string }
		Errors []struct{ Code, Message string }
	}
	if err := json.Unmarshal(answer, &put); err != nil {
		t.Fatalf("PutSpec answered non-JSON: %v\n%s", err, answer)
	}
	if len(put.Errors) > 0 {
		t.Fatalf("PutSpec refused the smoke spec: %+v", put.Errors)
	}
	if put.Spec.Project != smokeProject {
		t.Fatalf("PutSpec stored %q, want %q", put.Spec.Project, smokeProject)
	}
	t.Cleanup(func() {
		_, _ = post("/kelson.v1alpha1.SpecService/DeleteSpec", map[string]any{
			"project": smokeProject, "force": true,
		})
	})

	// Read it back through the same door a browser uses.
	resp, answer = post("/kelson.v1alpha1.SpecService/GetSpec", map[string]any{"project": smokeProject})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GetSpec answered %d: %s", resp.StatusCode, answer)
	}

	// The event stream. Server-streaming Connect: enveloped frames, and an RBAC
	// refusal arrives as an immediate end-of-stream frame carrying the error —
	// a healthy watch simply stays open with nothing to say. So: an error frame
	// fails, silence passes.
	watchSmokeStream(t, base)
}

// watchSmokeStream opens EventService.Watch scoped to the smoke project and
// reads for a few seconds. Connect envelopes are one flag byte and a
// four-byte big-endian length; flag bit 0x02 marks the end-of-stream frame,
// whose JSON body carries the error if the stream refused to start.
func watchSmokeStream(t *testing.T, base string) {
	t.Helper()
	request, err := json.Marshal(map[string]any{
		"scopes": []map[string]string{{"project": smokeProject}},
	})
	if err != nil {
		t.Fatalf("marshalling the Watch request: %v", err)
	}
	envelope := make([]byte, 5+len(request))
	binary.BigEndian.PutUint32(envelope[1:5], uint32(len(request)))
	copy(envelope[5:], request)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(base+"/kelson.v1alpha1.EventService/Watch",
		"application/connect+json", bytes.NewReader(envelope))
	if err != nil {
		// The client timeout firing while the stream sits healthily open is the
		// passing case; anything else is a transport failure.
		if strings.Contains(err.Error(), "Client.Timeout") || strings.Contains(err.Error(), "context deadline") {
			return
		}
		t.Fatalf("opening the Watch stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Watch answered %d: %s", resp.StatusCode, body)
	}

	header := make([]byte, 5)
	for {
		if _, err := io.ReadFull(resp.Body, header); err != nil {
			// Timing out with no end-of-stream frame means the watch is open and
			// quiet, which is the healthy state for a project nothing deploys.
			return
		}
		frame := make([]byte, binary.BigEndian.Uint32(header[1:5]))
		if _, err := io.ReadFull(resp.Body, frame); err != nil {
			return
		}
		if header[0]&0x02 == 0 {
			continue // an event; a live stream is at least as good as a quiet one
		}
		var end struct {
			Error *struct{ Code, Message string }
		}
		if err := json.Unmarshal(frame, &end); err == nil && end.Error != nil {
			t.Fatalf("the Watch stream refused to start: %s: %s — a missing `watch` verb on "+
				"environments reads exactly like this", end.Error.Code, end.Error.Message)
		}
		return
	}
}
