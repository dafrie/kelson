package explain

import (
	"testing"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/redact"
)

// The revision-diff helper, tested on its own: it is the load-bearing half of
// the acceptance case, and a diff that mis-pairs containers would name the
// wrong variable with full confidence.

func TestDiffEnvReportsARenameAsBothHalves(t *testing.T) {
	previous := readWorkloads([]delivery.Manifest{manifest("web",
		deploymentYAML("web", "img:1", []string{literalEnv("DATABASE_URL", "postgres://db/app")}, false))})
	live := readWorkloads([]delivery.Manifest{manifest("web",
		deploymentYAML("web", "img:2", []string{literalEnv("DB_URL", "postgres://db/app")}, false))})

	got := diffEnv(previous, live)
	if len(got) != 2 {
		t.Fatalf("got %d changes, want a removal and an addition: %+v", len(got), got)
	}
	// Sorted by name inside one container: DATABASE_URL before DB_URL.
	if got[0].Name != "DATABASE_URL" || got[0].Kind != ChangeRemoved {
		t.Errorf("first change = %+v, want DATABASE_URL removed", got[0])
	}
	if got[1].Name != "DB_URL" || got[1].Kind != ChangeAdded {
		t.Errorf("second change = %+v, want DB_URL added", got[1])
	}
	if got[0].Workload != "Deployment/web" || got[0].Container != "web" {
		t.Errorf("a change must name its workload and container, got %+v", got[0])
	}
}

func TestDiffEnvRendersReferenceForms(t *testing.T) {
	previous := readWorkloads([]delivery.Manifest{manifest("web",
		deploymentYAML("web", "img:1", []string{literalEnv("TOKEN", "plain")}, false))})
	live := readWorkloads([]delivery.Manifest{manifest("web",
		deploymentYAML("web", "img:1", []string{secretEnv("TOKEN", "api-creds", "token")}, false))})

	got := diffEnv(previous, live)
	if len(got) != 1 || got[0].Kind != ChangeModified {
		t.Fatalf("got %+v, want one modification", got)
	}
	if got[0].Before != "plain" || got[0].After != "secret api-creds/token" {
		t.Errorf("change = %+v, want the literal on the left and the reference form on the right", got[0])
	}
}

// TestEnvLiteralsPassThroughTheScrubber: env values are a display byte like any
// other, and the process-wide scrubber is what makes #117 a property rather
// than an argument.
func TestEnvLiteralsPassThroughTheScrubber(t *testing.T) {
	const credential = "s3cr3t-value-that-kelson-resolved"
	redact.Register(credential)

	live := readWorkloads([]delivery.Manifest{manifest("web",
		deploymentYAML("web", "img:1", []string{literalEnv("TOKEN", credential)}, false))})
	if len(live) != 1 || len(live[0].Containers) != 1 {
		t.Fatalf("read %+v, want one container", live)
	}
	got := live[0].Containers[0].Env[0].Value
	if got == credential {
		t.Fatalf("a registered credential reached the reader verbatim: %q", got)
	}
	if got != redact.Sentinel {
		t.Errorf("value = %q, want %q", got, redact.Sentinel)
	}
}

// TestDiffImagesIgnoresContainersOnOneSideOnly: a container that only exists in
// one revision is a structural change the env diff already reports variable by
// variable.
func TestDiffImagesIgnoresContainersOnOneSideOnly(t *testing.T) {
	previous := readWorkloads([]delivery.Manifest{manifest("web", deploymentYAML("web", "img:1", nil, false))})
	live := readWorkloads([]delivery.Manifest{
		manifest("web", deploymentYAML("web", "img:2", nil, false)),
		manifest("worker", deploymentYAML("worker", "img:2", nil, false)),
	})

	got := diffImages(previous, live)
	if len(got) != 1 {
		t.Fatalf("got %+v, want only the container present in both", got)
	}
	if got[0].Before != "img:1" || got[0].After != "img:2" {
		t.Errorf("image change = %+v", got[0])
	}
}

// TestReadWorkloadsUnderstandsCronJobsAndInitContainers: the pod template of a
// CronJob is two levels deeper, and an initContainer that cannot start keeps
// the workload down as surely as the main one.
func TestReadWorkloadsUnderstandsCronJobsAndInitContainers(t *testing.T) {
	cron := delivery.Manifest{APIVersion: "batch/v1", Kind: "CronJob", Name: "nightly", Namespace: namespace, YAML: []byte(
		"apiVersion: batch/v1\nkind: CronJob\nmetadata:\n  name: nightly\nspec:\n  jobTemplate:\n    spec:\n" +
			"      template:\n        spec:\n          containers:\n            - name: nightly\n              image: img:1\n" +
			"              env:\n                - name: WINDOW\n                  value: \"24h\"\n" +
			"          initContainers:\n            - name: wait-db\n              image: busybox\n")}

	got := readWorkloads([]delivery.Manifest{cron})
	if len(got) != 1 || len(got[0].Containers) != 2 {
		t.Fatalf("read %+v, want one workload with two containers", got)
	}
	main, ok := got[0].container("nightly")
	if !ok || len(main.Env) != 1 || main.Env[0].Name != "WINDOW" {
		t.Fatalf("main container = %+v", main)
	}
	if want := "spec.jobTemplate.spec.template.spec.containers[0].env[WINDOW]"; main.envPath("WINDOW") != want {
		t.Errorf("env path = %q, want %q", main.envPath("WINDOW"), want)
	}
	init, ok := got[0].container("wait-db")
	if !ok || !init.Init {
		t.Errorf("init container = %+v, want one marked Init", init)
	}
}

// TestReadWorkloadsSkipsWhatItCannotRead: a manifest of a kind this reader does
// not model, or one that does not decode, is skipped — an explanation built
// from three of four workloads beats no explanation, and manifest validity is
// the renderer's to own.
func TestReadWorkloadsSkipsWhatItCannotRead(t *testing.T) {
	got := readWorkloads([]delivery.Manifest{
		{Kind: "Service", Name: "web", YAML: []byte("apiVersion: v1\nkind: Service\n")},
		{Kind: "Deployment", Name: "broken", YAML: []byte("\t not: [yaml")},
		manifest("web", deploymentYAML("web", "img:1", nil, false)),
	})
	if len(got) != 1 || got[0].Name != "web" {
		t.Fatalf("read %+v, want only the one readable Deployment", got)
	}
}
