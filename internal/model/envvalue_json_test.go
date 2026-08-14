package model

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// EnvValue is a YAML union with a hand-written UnmarshalYAML, and under
// ADR-0027 the same value now also travels as JSON: a custom resource is
// decoded, stored and served by the API server as JSON, so `env:` reaches the
// controller through the JSON path and never through the YAML one.
//
// Two decoders for one union is two chances to disagree about what a document
// means. These tests are what stops that: every case is written once, in YAML,
// and asserted to mean the same thing after the JSON round trip and after being
// read straight from the equivalent JSON text.
//
// A divergence here is not cosmetic. The three arms are "a plain string", "a
// secret reference" and "a service binding", and the whole guarantee of issue
// #82 is that a value that decoded is one of exactly those — so a JSON decoder
// that accepted a fourth shape, or read a secret reference as a literal, would
// put a credential where the renderer expects configuration.

// envValueCase is one document written both ways. The YAML is the authored
// form; the JSON is what the API server would hold for it.
type envValueCase struct {
	name string
	yaml string
	json string
	want EnvValue
}

var envValueCases = []envValueCase{
	{
		name: "literal",
		yaml: `info`,
		json: `"info"`,
		want: EnvValue{Literal: "info"},
	},
	{
		name: "empty literal",
		yaml: `""`,
		json: `""`,
		want: EnvValue{Literal: ""},
	},
	{
		name: "literal that looks like a reference but is a string",
		yaml: `"secret: not-a-mapping"`,
		json: `"secret: not-a-mapping"`,
		want: EnvValue{Literal: "secret: not-a-mapping"},
	},
	{
		name: "secret reference",
		yaml: `{secret: checkout-db, key: url}`,
		json: `{"secret":"checkout-db","key":"url"}`,
		want: EnvValue{Secret: &SecretRef{Name: "checkout-db", Key: "url"}},
	},
	{
		name: "secret reference in block form",
		yaml: "secret: checkout-db\nkey: url",
		json: `{"secret":"checkout-db","key":"url"}`,
		want: EnvValue{Secret: &SecretRef{Name: "checkout-db", Key: "url"}},
	},
	{
		name: "service binding",
		yaml: `{from: {service: db, key: uri}}`,
		json: `{"from":{"service":"db","key":"uri"}}`,
		want: EnvValue{From: &ServiceBinding{Service: "db", Key: "uri"}},
	},
	{
		name: "service binding in block form",
		yaml: "from:\n  service: db\n  key: uri",
		json: `{"from":{"service":"db","key":"uri"}}`,
		want: EnvValue{From: &ServiceBinding{Service: "db", Key: "uri"}},
	},
}

// TestEnvValueYAMLEqualsJSON is the parity assertion: for each authored
// document, decoding the YAML and decoding the equivalent JSON produce the same
// union member, and marshalling either back produces something that decodes to
// the same value again.
func TestEnvValueYAMLEqualsJSON(t *testing.T) {
	for _, tc := range envValueCases {
		t.Run(tc.name, func(t *testing.T) {
			var fromYAML EnvValue
			if err := yaml.Unmarshal([]byte(tc.yaml), &fromYAML); err != nil {
				t.Fatalf("yaml.Unmarshal(%q): %v", tc.yaml, err)
			}
			if !reflect.DeepEqual(fromYAML, tc.want) {
				t.Fatalf("YAML decoded to %s, want %s", fromYAML.String(), tc.want.String())
			}

			var fromJSON EnvValue
			if err := json.Unmarshal([]byte(tc.json), &fromJSON); err != nil {
				t.Fatalf("json.Unmarshal(%q): %v", tc.json, err)
			}
			if !reflect.DeepEqual(fromJSON, tc.want) {
				t.Fatalf("JSON decoded to %s, want %s", fromJSON.String(), tc.want.String())
			}

			// The document the YAML author wrote, marshalled as JSON, must be
			// the JSON document — this is the byte the API server would store.
			encoded, err := json.Marshal(fromYAML)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			if string(encoded) != tc.json {
				t.Errorf("YAML-decoded value marshals to %s, want %s", encoded, tc.json)
			}

			// And back again: JSON out, JSON in, same union member. This is the
			// path a spec takes every time a controller reads its own resource.
			var round EnvValue
			if err := json.Unmarshal(encoded, &round); err != nil {
				t.Fatalf("json.Unmarshal(%s): %v", encoded, err)
			}
			if !reflect.DeepEqual(round, tc.want) {
				t.Errorf("JSON round trip produced %s, want %s", round.String(), tc.want.String())
			}
		})
	}
}

// TestEnvValueRefusalsAgree pins the other half of the union's semantics: the
// shapes that are *not* an environment value must be refused by both decoders,
// and the refusal must name the same problem. A JSON decoder that accepted what
// the YAML decoder rejects would make `kubectl apply` a way around validation.
func TestEnvValueRefusalsAgree(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		json string
		// want is a fragment both messages must contain.
		want string
	}{
		{
			name: "both arms at once",
			yaml: `{from: {service: db, key: uri}, secret: s}`,
			json: `{"from":{"service":"db","key":"uri"},"secret":"s"}`,
			want: "written as both",
		},
		{
			name: "from with a stray key",
			yaml: `{from: {service: db, key: uri}, key: url}`,
			json: `{"from":{"service":"db","key":"uri"},"key":"url"}`,
			want: "written as both",
		},
		{
			name: "a mapping naming neither arm",
			yaml: `{value: hunter2}`,
			json: `{"value":"hunter2"}`,
			want: "names neither",
		},
		{
			name: "a sequence",
			yaml: `[a, b]`,
			json: `["a","b"]`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var fromYAML EnvValue
			yerr := yaml.Unmarshal([]byte(tc.yaml), &fromYAML)
			if yerr == nil {
				t.Fatalf("YAML %q was accepted; it must be refused", tc.yaml)
			}
			var fromJSON EnvValue
			jerr := json.Unmarshal([]byte(tc.json), &fromJSON)
			if jerr == nil {
				t.Fatalf("JSON %q was accepted; it must be refused", tc.json)
			}
			if tc.want == "" {
				return
			}
			if !strings.Contains(yerr.Error(), tc.want) {
				t.Errorf("YAML refusal %q does not mention %q", yerr, tc.want)
			}
			if !strings.Contains(jerr.Error(), tc.want) {
				t.Errorf("JSON refusal %q does not mention %q", jerr, tc.want)
			}
		})
	}
}

// TestEnvMapSurvivesJSONRoundTrip exercises the union where it actually lives —
// inside a spec — because that is the shape the CRD carries and the shape a
// controller reads back out of the API server.
func TestEnvMapSurvivesJSONRoundTrip(t *testing.T) {
	const doc = `
project: checkout
components:
  - name: web
    env:
      LOG_LEVEL: info
      DATABASE_URL: {from: {service: db, key: uri}}
      API_TOKEN: {secret: api, key: token}
`
	var spec EnvironmentSpec
	if err := yaml.Unmarshal([]byte(doc), &spec); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}

	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var round EnvironmentSpec
	if err := json.Unmarshal(encoded, &round); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(spec, round) {
		t.Fatalf("EnvironmentSpec changed across a JSON round trip:\n got: %#v\nwant: %#v", round, spec)
	}
	env := round.Components[0].Env
	if env["LOG_LEVEL"].Literal != "info" {
		t.Errorf("LOG_LEVEL is %s, want the literal info", env["LOG_LEVEL"].String())
	}
	if env["DATABASE_URL"].From == nil || env["DATABASE_URL"].From.Service != "db" {
		t.Errorf("DATABASE_URL is %s, want a binding to db", env["DATABASE_URL"].String())
	}
	if env["API_TOKEN"].Secret == nil || env["API_TOKEN"].Secret.Name != "api" {
		t.Errorf("API_TOKEN is %s, want a reference to secret api", env["API_TOKEN"].String())
	}
}
