package renderer

import (
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/model"
)

// releaseFixture is the bound fixture (web → postgres) in direct mode, with a
// release command on the web component. Direct mode is not incidental: it is
// the only mode the field renders in (ADR-0019).
func releaseFixture() *model.Resolved {
	r := boundFixture(model.PresetSmall)
	r.Environment.Mode = model.DeliveryDirect
	r.Components[0].Release = &model.ResolvedRelease{
		Command:        []string{"./manage.py", "migrate"},
		TimeoutSeconds: 900,
	}
	return r
}

func manifestNamed(t *testing.T, ms []Manifest, kind, name string) Manifest {
	t.Helper()
	for _, m := range ms {
		if m.Kind == kind && m.Name == name {
			return m
		}
	}
	t.Fatalf("no %s/%s in the rendered set: %v", kind, name, kinds(ms))
	return Manifest{}
}

func manifestText(t *testing.T, m Manifest) string {
	t.Helper()
	b, err := m.YAML()
	if err != nil {
		t.Fatalf("encoding %s/%s: %v", m.Kind, m.Name, err)
	}
	return string(b)
}

// TestReleaseJobOrdering pins the sequencing contract of issue #104: the
// migration is applied after everything it talks to and before everything that
// must not roll until it has finished. Order is all a rendered set can say; the
// waiting is the direct adapter's.
func TestReleaseJobOrdering(t *testing.T) {
	ms, err := Render(releaseFixture(), cnpgProfile(), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	got := kinds(ms)
	want := []string{
		"Namespace/checkout-prod",
		"Cluster/checkout-production-db",
		"ServiceAccount/web",
		"Job/release-web-",
		"Service/web",
		"Deployment/web",
	}
	for i, w := range want {
		if i >= len(got) || !strings.HasPrefix(got[i], w) {
			t.Fatalf("resource %d is %q, want one starting with %q\nfull set: %v", i, at(got, i), w, got)
		}
	}
}

// TestReleaseJobCarriesTheComponentsEnvironment is the property the whole
// feature rests on: the migration runs with what the application runs with —
// the same bindings, the same secret references, the same image.
func TestReleaseJobCarriesTheComponentsEnvironment(t *testing.T) {
	resolved := releaseFixture()
	resolved.Components[0].Env["SECRET_KEY"] = model.EnvValue{
		Secret: &model.SecretRef{Name: "web-secrets", Key: "django"},
	}
	ms, err := Render(resolved, cnpgProfile(), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	job := manifestText(t, releaseJob(t, ms))
	for _, want := range []string{
		"image: ghcr.io/acme/checkout:1.2.3",
		"name: checkout-production-db-app", // the binding's secretKeyRef
		"name: web-secrets",                // the author's own secret reference
		"restartPolicy: Never",
		"backoffLimit: 2",
		"activeDeadlineSeconds: 900",
		"serviceAccountName: web",
		"kelson.dev/release-hook: \"true\"",
	} {
		if !strings.Contains(job, want) {
			t.Errorf("release Job does not carry %q:\n%s", want, job)
		}
	}
	for _, unwanted := range []string{"livenessProbe", "readinessProbe", "containerPort"} {
		if strings.Contains(job, unwanted) {
			t.Errorf("release Job carries %q, which belongs to a serving workload:\n%s", unwanted, job)
		}
	}
}

// TestReleaseJobNameIsStableAcrossRenders and its sibling below are the
// idempotency contract: re-applying an unchanged revision must address the Job
// that already ran (so a completed migration does not run twice), and a changed
// revision must address a new one (so a deploy does run its migrations).
func TestReleaseJobNameIsStableAcrossRenders(t *testing.T) {
	var first string
	for i := 0; i < 8; i++ {
		ms, err := Render(releaseFixture(), cnpgProfile(), nil)
		if err != nil {
			t.Fatalf("Render %d failed: %v", i, err)
		}
		name := releaseJob(t, ms).Name
		if i == 0 {
			first = name
			continue
		}
		if name != first {
			t.Fatalf("render %d named the release Job %q, want %q", i, name, first)
		}
	}
}

func TestReleaseJobNameChangesWithTheRevision(t *testing.T) {
	base, err := Render(releaseFixture(), cnpgProfile(), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	baseName := releaseJob(t, base).Name

	for _, tc := range []struct {
		what   string
		mutate func(*model.Resolved)
	}{
		{"a new image", func(r *model.Resolved) { r.Components[0].Image = "ghcr.io/acme/checkout:9.9.9" }},
		{"a new release command", func(r *model.Resolved) { r.Components[0].Release.Command = []string{"rake", "db:migrate"} }},
		{"a new env value", func(r *model.Resolved) {
			r.Components[0].Env["LOG_LEVEL"] = model.EnvValue{Literal: "debug"}
		}},
	} {
		t.Run(tc.what, func(t *testing.T) {
			resolved := releaseFixture()
			tc.mutate(resolved)
			ms, err := Render(resolved, cnpgProfile(), nil)
			if err != nil {
				t.Fatalf("Render failed: %v", err)
			}
			if name := releaseJob(t, ms).Name; name == baseName {
				t.Fatalf("%s left the release Job named %q; a new revision must run its migrations", tc.what, name)
			}
		})
	}
}

// TestReleaseJobBindsPerEnvironment pins what promotion means for a migration.
// Promoting moves an image pin and nothing else, so deploying the target
// environment runs ITS release Job against ITS database: the two environments
// resolve the same binding to two different Secrets, and staging's data is
// never in reach of production's deploy.
func TestReleaseJobBindsPerEnvironment(t *testing.T) {
	secrets := map[string]string{}
	for _, env := range []struct{ name, namespace string }{
		{"staging", "checkout-staging"},
		{"production", "checkout-prod"},
	} {
		resolved := releaseFixture()
		resolved.Environment.Name = env.name
		resolved.Environment.Namespace = env.namespace
		ms, err := Render(resolved, cnpgProfile(), nil)
		if err != nil {
			t.Fatalf("Render(%s) failed: %v", env.name, err)
		}
		job := releaseJob(t, ms)
		if job.Namespace != env.namespace {
			t.Errorf("%s: release Job is in namespace %q, want %q", env.name, job.Namespace, env.namespace)
		}
		secrets[env.name] = bindingSecret(t, manifestText(t, job))
	}
	if secrets["staging"] == secrets["production"] {
		t.Fatalf("both environments' release Jobs read the credentials Secret %q; "+
			"a migration must run against its own environment's database", secrets["staging"])
	}
	for env, want := range map[string]string{
		"staging":    "checkout-staging-db-app",
		"production": "checkout-production-db-app",
	} {
		if secrets[env] != want {
			t.Errorf("%s: release Job binds to Secret %q, want %q", env, secrets[env], want)
		}
	}
}

// TestReleaseHookMovesTheServiceAccountRatherThanDuplicatingIt guards the one
// structural consequence of emitting the Job early: the pod names a
// ServiceAccount, so the ServiceAccount has to come with it — exactly once.
func TestReleaseHookMovesTheServiceAccountRatherThanDuplicatingIt(t *testing.T) {
	ms, err := Render(releaseFixture(), cnpgProfile(), nil)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	accounts, jobIndex := 0, -1
	saIndex := -1
	for i, m := range ms {
		switch {
		case m.Kind == "ServiceAccount" && m.Name == "web":
			accounts++
			saIndex = i
		case m.Kind == "Job":
			jobIndex = i
		}
	}
	if accounts != 1 {
		t.Fatalf("web has %d ServiceAccounts in the set, want exactly 1: %v", accounts, kinds(ms))
	}
	if saIndex > jobIndex {
		t.Fatalf("the ServiceAccount is applied after the Job that names it: %v", kinds(ms))
	}
	// The component without a release hook keeps its own, where it always was.
	manifestNamed(t, ms, "ServiceAccount", "worker")
}

// TestReleaseRequiresDirectMode is the honest gate (ADR-0019). Every other
// delivery mode is refused by name rather than rendering a Job that would run
// beside the rollout instead of before it.
func TestReleaseRequiresDirectMode(t *testing.T) {
	for _, mode := range []model.DeliveryMode{model.DeliveryFlux, model.DeliveryArgoCD, ""} {
		t.Run(string(mode)+"-mode", func(t *testing.T) {
			resolved := releaseFixture()
			resolved.Environment.Mode = mode
			_, err := Render(resolved, cnpgProfile(), nil)
			if err == nil {
				t.Fatalf("delivery mode %q rendered a release Job; it must be refused", mode)
			}
			errs, ok := err.(Errors)
			if !ok {
				t.Fatalf("Render returned %T, want renderer.Errors", err)
			}
			if errs[0].Code != ErrReleaseRequiresDirect {
				t.Fatalf("first error is %s, want %s: %v", errs[0].Code, ErrReleaseRequiresDirect, errs)
			}
			if errs[0].Application != "web" {
				t.Errorf("the refusal names application %q, want web", errs[0].Application)
			}
			if !strings.Contains(errs[0].Remediation, "delivery.mode: direct") {
				t.Errorf("the remediation does not name the field that fixes it: %s", errs[0].Remediation)
			}
		})
	}
}

// TestReleaseJobNameTooLong refuses rather than truncating: two components
// whose truncated Jobs collided would run one migration under the other's
// identity.
func TestReleaseJobNameTooLong(t *testing.T) {
	resolved := releaseFixture()
	resolved.Components[0].Name = strings.Repeat("a", 55)
	resolved.Components[0].Env = map[string]model.EnvValue{}
	_, err := Render(resolved, cnpgProfile(), nil)
	if err == nil {
		t.Fatal("a component name that overflows the Job name rendered anyway")
	}
	errs, ok := err.(Errors)
	if !ok || errs[0].Code != ErrReleaseName {
		t.Fatalf("Render returned %v, want %s", err, ErrReleaseName)
	}
}

// releaseJob returns the one Job in a rendered set.
func releaseJob(t *testing.T, ms []Manifest) Manifest {
	t.Helper()
	for _, m := range ms {
		if m.Kind == "Job" {
			return m
		}
	}
	t.Fatalf("no release Job in the rendered set: %v", kinds(ms))
	return Manifest{}
}

// bindingSecret pulls the secretKeyRef name the DATABASE_URL binding resolved
// to out of a rendered Job.
func bindingSecret(t *testing.T, job string) string {
	t.Helper()
	lines := strings.Split(job, "\n")
	for i, l := range lines {
		if !strings.Contains(l, "name: DATABASE_URL") {
			continue
		}
		for _, follow := range lines[i:min(i+5, len(lines))] {
			if name, ok := strings.CutPrefix(strings.TrimSpace(follow), "name: "); ok && name != "DATABASE_URL" {
				return name
			}
		}
	}
	t.Fatalf("no DATABASE_URL secretKeyRef in the rendered Job:\n%s", job)
	return ""
}

func at(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return "(missing)"
}
