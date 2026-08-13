package promote

import (
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/delivery"
)

// The manifests below are the shapes internal/renderer records: the component
// label on metadata, the container named after the component, and the two
// pod-bearing kinds nested differently.

const deploymentYAML = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: checkout-production
  labels:
    app.kubernetes.io/managed-by: kelson
    kelson.dev/application: web
    kelson.dev/environment: production
    kelson.dev/project: checkout
spec:
  replicas: 3
  template:
    metadata:
      labels:
        kelson.dev/application: web
    spec:
      serviceAccountName: web
      containers:
        - name: web
          image: ghcr.io/acme/checkout@sha256:aaaa
          ports:
            - containerPort: 8080
`

const cronJobYAML = `apiVersion: batch/v1
kind: CronJob
metadata:
  name: digest
  namespace: checkout-production
  labels:
    kelson.dev/application: digest
spec:
  schedule: 30 6 * * 1-5
  jobTemplate:
    spec:
      template:
        metadata:
          labels:
            kelson.dev/application: digest
        spec:
          serviceAccountName: digest
          restartPolicy: Never
          containers:
            - name: digest
              image: ghcr.io/acme/checkout@sha256:bbbb
`

// A managed data service: rendered by kelson, owned by an operator, and
// deliberately carrying no component label (internal/renderer/dataservice.go).
const postgresYAML = `apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: db
  namespace: checkout-production
  labels:
    kelson.dev/project: checkout
spec:
  instances: 3
  imageName: ghcr.io/cloudnative-pg/postgresql:16.4
`

const serviceYAML = `apiVersion: v1
kind: Service
metadata:
  name: web
  namespace: checkout-production
  labels:
    kelson.dev/application: web
spec:
  ports:
    - port: 8080
`

func manifests(bodies ...string) []delivery.Manifest {
	out := make([]delivery.Manifest, 0, len(bodies))
	for _, body := range bodies {
		var m delivery.Manifest
		switch {
		case strings.Contains(body, "kind: Deployment"):
			m.Kind = "Deployment"
		case strings.Contains(body, "kind: CronJob"):
			m.Kind = "CronJob"
		case strings.Contains(body, "kind: Service"):
			m.Kind = "Service"
		default:
			m.Kind = "Cluster"
		}
		m.YAML = []byte(body)
		out = append(out, m)
	}
	return out
}

// TestDeployedReadsBothPodBearingKinds.
func TestDeployedReadsBothPodBearingKinds(t *testing.T) {
	got, err := Deployed(manifests(deploymentYAML, cronJobYAML, serviceYAML))
	if err != nil {
		t.Fatalf("Deployed: %v", err)
	}
	want := map[string]string{
		"web":    "ghcr.io/acme/checkout@sha256:aaaa",
		"digest": "ghcr.io/acme/checkout@sha256:bbbb",
	}
	if len(got) != len(want) {
		t.Fatalf("Deployed returned %v, want %v", got, want)
	}
	for name, image := range want {
		if got[name] != image {
			t.Errorf("component %s: got %q, want %q", name, got[name], image)
		}
	}
}

// TestDeployedSkipsDataComponents: a managed data service carries an image in
// its CR, and promoting it would be promoting the operator's business
// (ADR-0005). It has no component label, so it never reaches the result.
func TestDeployedSkipsDataComponents(t *testing.T) {
	got, err := Deployed(manifests(postgresYAML))
	if err != nil {
		t.Fatalf("Deployed: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a data component produced an image to promote: %v", got)
	}
}

// TestDeployedIgnoresResourcesWithNoPodTemplate.
func TestDeployedIgnoresResourcesWithNoPodTemplate(t *testing.T) {
	got, err := Deployed(manifests(serviceYAML))
	if err != nil {
		t.Fatalf("Deployed: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a Service produced an image: %v", got)
	}
}

// TestDeployedAcceptsARenamedSingleContainer: an overlay may rename the
// container, and one unambiguous container is still an answer.
func TestDeployedAcceptsARenamedSingleContainer(t *testing.T) {
	renamed := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  labels:
    kelson.dev/application: web
spec:
  template:
    spec:
      containers:
        - name: app
          image: ghcr.io/acme/checkout@sha256:cccc
`
	got, err := Deployed(manifests(renamed))
	if err != nil {
		t.Fatalf("Deployed: %v", err)
	}
	if got["web"] != "ghcr.io/acme/checkout@sha256:cccc" {
		t.Errorf("got %v", got)
	}
}

// TestDeployedRefusesToGuessBetweenSidecars: two containers and neither named
// after the component is ambiguous, and ambiguity is an absence — Plan turns it
// into a skip with a reason rather than a guess.
func TestDeployedRefusesToGuessBetweenSidecars(t *testing.T) {
	sidecars := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  labels:
    kelson.dev/application: web
spec:
  template:
    spec:
      containers:
        - name: app
          image: ghcr.io/acme/checkout@sha256:cccc
        - name: proxy
          image: ghcr.io/acme/proxy@sha256:dddd
`
	got, err := Deployed(manifests(sidecars))
	if err != nil {
		t.Fatalf("Deployed: %v", err)
	}
	if _, ok := got["web"]; ok {
		t.Errorf("an ambiguous pod produced an image: %v", got)
	}
}

// TestDeployedReportsUnreadableManifests: a recorded revision that does not
// parse is a real failure, not an empty answer.
func TestDeployedReportsUnreadableManifests(t *testing.T) {
	broken := []delivery.Manifest{{Kind: "Deployment", Name: "web", YAML: []byte("apiVersion: apps/v1\n  kind: [\n")}}
	if _, err := Deployed(broken); err == nil {
		t.Fatal("a malformed recorded manifest was read as no images")
	}
}
