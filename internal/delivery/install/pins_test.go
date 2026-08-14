package install

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/clusterprofile/support"
)

// The drift tests for the pins table.
//
// A pin is three facts that must agree — a version, the URL that serves that
// version, and the digest of the bytes at it — plus a claim that detection can
// see the component at all. Nothing here reaches the network: these assert the
// table's internal consistency, which is what a half-updated row breaks.

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// TestPinsAreInternallyConsistent: a row that documents one version and
// installs another cannot merge.
func TestPinsAreInternallyConsistent(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Components {
		t.Run(c.Name, func(t *testing.T) {
			if c.Name == "" || c.Title == "" || c.Namespace == "" || c.ProfileField == "" || c.Provides == "" {
				t.Fatalf("row is missing a required field: %+v", c)
			}
			if seen[c.Name] {
				t.Fatalf("duplicate component name %q", c.Name)
			}
			seen[c.Name] = true

			switch c.Status {
			case StatusSupported:
				if c.FollowUp != "" {
					t.Fatal("a supported row carries a FollowUp, which reads as a refusal it does not make")
				}
				if !strings.HasPrefix(c.ManifestURL, "https://") {
					t.Fatalf("ManifestURL %q is not https; a pinned manifest is applied with cluster-admin-shaped RBAC",
						c.ManifestURL)
				}
				if c.Version == "" {
					t.Fatal("a supported row must pin a version")
				}
				if !strings.Contains(c.ManifestURL, strings.TrimPrefix(c.Version, "v")) {
					t.Fatalf("ManifestURL %q does not contain version %q: the row documents one release and "+
						"installs another", c.ManifestURL, c.Version)
				}
				if !digestPattern.MatchString(c.SHA256) {
					t.Fatalf("SHA256 %q is not a lowercase hex sha256 digest", c.SHA256)
				}
			case StatusDeferred:
				if c.FollowUp == "" {
					t.Fatal("a deferred row must say why, and where the work is tracked")
				}
				if !strings.Contains(c.FollowUp, "#60") {
					t.Fatalf("FollowUp %q does not name the issue the follow-up hangs off", c.FollowUp)
				}
				if c.Version != "" || c.ManifestURL != "" || c.SHA256 != "" {
					t.Fatal("a deferred row carries a pin, which reads as an install it will not perform")
				}
			default:
				t.Fatalf("unknown status %q", c.Status)
			}
		})
	}
}

// TestProfileFieldsExist: a ProfileField that is not a real ClusterProfile
// field can never carry a detection Gap, so the component's presence would be
// permanently answerable — including when the probe could not look.
func TestProfileFieldsExist(t *testing.T) {
	fields := map[string]bool{}
	rt := reflect.TypeOf(clusterprofile.ClusterProfile{})
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("yaml")
		name, _, _ := strings.Cut(tag, ",")
		if name != "" {
			fields[name] = true
		}
	}
	for _, c := range Components {
		if !fields[c.ProfileField] {
			t.Errorf("%s: ProfileField %q is not a ClusterProfile field", c.Name, c.ProfileField)
		}
	}
}

// TestPresenceIsWiredForEveryRow: adding a row without teaching Presence about
// it would make kelson install a component that is already there.
func TestPresenceIsWiredForEveryRow(t *testing.T) {
	present := map[string]func(*clusterprofile.ClusterProfile){
		"flux":             func(p *clusterprofile.ClusterProfile) { p.FluxOperator = &clusterprofile.Component{} },
		"cert-manager":     func(p *clusterprofile.ClusterProfile) { p.CertManager = &clusterprofile.CertManager{} },
		"cnpg":             func(p *clusterprofile.ClusterProfile) { p.CloudNativePG = &clusterprofile.CloudNativePG{} },
		"envoy-gateway":    func(p *clusterprofile.ClusterProfile) { p.GatewayAPI = &clusterprofile.GatewayAPI{} },
		"external-secrets": func(p *clusterprofile.ClusterProfile) { p.ExternalSecrets = &clusterprofile.ExternalSecrets{} },
	}
	if len(present) != len(Components) {
		t.Fatalf("this test knows %d components and the table has %d: teach Presence about the new row",
			len(present), len(Components))
	}
	for _, c := range Components {
		mutate, ok := present[c.Name]
		if !ok {
			t.Fatalf("%s: no presence fixture, so nothing proves detection can refuse this install", c.Name)
		}
		var prof clusterprofile.ClusterProfile
		if outcome, _ := c.Presence(prof); outcome != clusterprofile.OutcomeNo {
			t.Errorf("%s: an empty profile gives %v, want no", c.Name, outcome)
		}
		mutate(&prof)
		if outcome, _ := c.Presence(prof); outcome != clusterprofile.OutcomeYes {
			t.Errorf("%s: a profile that reports it present gives %v, want yes", c.Name, outcome)
		}
		prof = clusterprofile.ClusterProfile{Incomplete: []clusterprofile.Gap{{Field: c.ProfileField, Reason: "forbidden"}}}
		if outcome, _ := c.Presence(prof); outcome != clusterprofile.OutcomeUnknown {
			t.Errorf("%s: a detection gap gives %v, want unknown", c.Name, outcome)
		}
	}
}

// TestFluxPresenceCoversBothFindings: Flux controllers installed by something
// other than flux-operator are still a refusal — dropping a FluxInstance beside
// them would hand a second manager the same controllers.
func TestFluxPresenceCoversBothFindings(t *testing.T) {
	flux, ok := Lookup("flux")
	if !ok {
		t.Fatal("no flux row")
	}
	prof := clusterprofile.ClusterProfile{Flux: &clusterprofile.Component{Version: "2.4.0"}}
	outcome, reason := flux.Presence(prof)
	if outcome != clusterprofile.OutcomeYes {
		t.Fatalf("Presence() = %v, want yes", outcome)
	}
	if !strings.Contains(reason, "other than flux-operator") {
		t.Fatalf("reason %q does not distinguish a hand-managed Flux", reason)
	}
}

// TestNamesMatchTheSupportMatrix: the two tables are read together, so a
// component named "cnpg" in one and "cloudnative-pg" in the other would make
// the version floor and the pinned version look like different components.
func TestNamesMatchTheSupportMatrix(t *testing.T) {
	// Every supported row must have a support-matrix floor, except envoy-gateway
	// which the matrix tracks as the API ("gateway-api") rather than as an
	// implementation.
	exempt := map[string]string{"envoy-gateway": "gateway-api", "external-secrets": ""}
	for _, c := range Components {
		if alias, ok := exempt[c.Name]; ok {
			if alias == "" {
				continue
			}
			if _, found := support.Lookup(alias); !found {
				t.Errorf("%s: support matrix has no row %q", c.Name, alias)
			}
			continue
		}
		if _, found := support.Lookup(c.Name); !found {
			t.Errorf("%s: the support matrix has no row of that name; the two tables are read together",
				c.Name)
		}
	}
}

// TestPinnedVersionsClearTheSupportFloor: installing a version kelson then
// refuses to render against would be a complete waste of everybody's time.
//
// The flux row is checked against its DISTRIBUTION version rather than against
// Component.Version. The row pins flux-operator (v0.58.x); what kelson renders
// against is the Flux the operator installs, which is FluxDistributionVersion —
// and the support matrix's `flux` and `helm-controller` floors are about that.
func TestPinnedVersionsClearTheSupportFloor(t *testing.T) {
	floorFor := func(c Component) (string, string) {
		if c.Name == "flux" {
			return strings.TrimSuffix(FluxDistributionVersion, ".x") + ".0", "flux"
		}
		return strings.TrimPrefix(c.Version, "v"), c.Name
	}
	for _, c := range Supported() {
		found, name := floorFor(c)
		row, ok := support.Lookup(name)
		if !ok {
			continue
		}
		if support.AtLeast(found, row.Minimum) == clusterprofile.OutcomeNo {
			t.Errorf("%s: the pinned version %s is below the support floor %s, so kelson would install a "+
				"component it then refuses to render against", c.Name, found, row.Minimum)
		}
	}
	// The Flux the FluxInstance asks for must also serve helm.toolkit.fluxcd.io/v2
	// for `kind: helm` (ADR-0016). helm-controller's floor is versioned
	// independently of Flux's, so it is a second check rather than the same one.
	hc, ok := support.Lookup("helm-controller")
	if !ok {
		t.Fatal("the support matrix has no helm-controller row")
	}
	if support.AtLeast("1.0.0", hc.Minimum) == clusterprofile.OutcomeNo {
		t.Fatalf("the helm-controller floor %s is above what Flux %s ships", hc.Minimum, FluxDistributionVersion)
	}
}

// TestFluxDistributionPinsAMinor: the operator owns patch upgrades, kelson owns
// the minor.
func TestFluxDistributionPinsAMinor(t *testing.T) {
	if !strings.HasSuffix(FluxDistributionVersion, ".x") {
		t.Fatalf("FluxDistributionVersion = %q, want a minor-pinned expression like 2.9.x",
			FluxDistributionVersion)
	}
	if !contains(FluxComponents, "helm-controller") {
		t.Fatalf("FluxComponents = %v, want helm-controller: `kind: helm` renders a HelmRelease (ADR-0016)",
			FluxComponents)
	}
	if contains(FluxComponents, "image-automation-controller") {
		t.Fatal("FluxComponents installs a controller kelson renders nothing for")
	}
}

// TestNamesAndLookupAgree.
func TestNamesAndLookup(t *testing.T) {
	for _, name := range Names() {
		if _, ok := Lookup(name); !ok {
			t.Errorf("Names() lists %q and Lookup() does not know it", name)
		}
	}
	if _, ok := Lookup("nothing-like-this"); ok {
		t.Fatal("Lookup invented a component")
	}
	if len(Supported()) == 0 {
		t.Fatal("no component can be installed at all")
	}
}
