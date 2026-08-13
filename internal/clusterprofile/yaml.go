package clusterprofile

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Marshal and Unmarshal exist because the profile's YAML tags are the schema of
// record for the wire (ADR-0013: the API carries the profile as the same
// document `kelson profile` prints, rather than duplicating the fields as proto
// messages it would then have to keep in sync). The ConnectRPC handlers in
// internal/api may not import a YAML library — their depguard rule allows only
// the transport and kelson itself — so the encoding lives with the type that
// defines it (issue #139).

// Marshal encodes a profile as its canonical YAML document.
func Marshal(p ClusterProfile) ([]byte, error) {
	data, err := yaml.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("clusterprofile: encoding profile: %w", err)
	}
	return data, nil
}

// Unmarshal decodes a profile document. An empty input is the zero profile —
// "nothing detected", which is a valid and deterministic render input, not an
// error.
func Unmarshal(data []byte) (ClusterProfile, error) {
	var p ClusterProfile
	if len(data) == 0 {
		return p, nil
	}
	if err := yaml.Unmarshal(data, &p); err != nil {
		return ClusterProfile{}, fmt.Errorf("clusterprofile: parsing profile: %w", err)
	}
	return p, nil
}
