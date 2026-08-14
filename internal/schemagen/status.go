package main

// The status half of each CRD schema.
//
// It is written out here rather than reflected, and that asymmetry is
// deliberate. `spec` is the authoring model and has exactly one definition,
// internal/model, so reflecting it is what stops the CRD and schema/*.json from
// disagreeing (ADR-0027 decision 4). `status` is not part of the authoring
// model at all — it is what a controller observed, it lives in
// api/kelson/v1alpha1, and that package imports apimachinery, which the
// generator's depguard fence does not allow it to import back.
//
// The drift that asymmetry risks is real and is policed from the other side:
// api/kelson/v1alpha1/crd_test.go reads these files and requires every property
// here to be a json tag of the corresponding Go struct and back. A field added
// to EnvironmentStatus without a line here fails that test, in the package that
// owns the type.

// conditionsSchema is metav1.Condition, which every Kubernetes API spells the
// same way. The bounds are the ones the upstream type carries as kubebuilder
// markers, so a condition kelson writes is a condition any tool that knows
// conditions can read.
func conditionsSchema(description string) omap {
	var condition omap
	condition.set("type", "object")
	condition.set("required", []string{"lastTransitionTime", "message", "reason", "status", "type"})
	condition.set("properties", omap{
		{"lastTransitionTime", omap{
			{"description", "when the condition last changed from one status to another"},
			{"type", "string"},
			{"format", "date-time"},
		}},
		{"message", omap{
			{"description", "a human-readable message about the transition, which may be empty"},
			{"type", "string"},
			{"maxLength", int64(32768)},
		}},
		{"observedGeneration", omap{
			{"description", "the .metadata.generation the condition was set from"},
			{"type", "integer"},
			{"format", "int64"},
			{"minimum", int64(0)},
		}},
		{"reason", omap{
			{"description", "a programmatic identifier for the condition's last transition"},
			{"type", "string"},
			{"minLength", int64(1)},
			{"maxLength", int64(1024)},
			{"pattern", `^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$`},
		}},
		{"status", omap{
			{"description", "True, False or Unknown"},
			{"type", "string"},
			{"enum", []string{"True", "False", "Unknown"}},
		}},
		{"type", omap{
			{"description", "the condition's type, in CamelCase; Ready is kelson's summary condition"},
			{"type", "string"},
			{"maxLength", int64(316)},
			{"pattern", `^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])$`},
		}},
	})

	var m omap
	m.set("description", description)
	m.set("type", "array")
	m.set("items", condition)
	// Conditions are a set keyed by type, which is what lets a server-side
	// apply from two managers merge rather than clobber.
	m.set("x-kubernetes-list-type", "map")
	m.set("x-kubernetes-list-map-keys", []string{"type"})
	return m
}

// validationErrorsSchema mirrors api/kelson/v1alpha1.ValidationError, which
// mirrors model.Error. One taxonomy, three places it is spelled, and the codes
// mean the same thing in all three (ADR-0027 decision 5).
func validationErrorsSchema() omap {
	var item omap
	item.set("type", "object")
	item.set("required", []string{"code", "message"})
	item.set("properties", omap{
		{"code", omap{
			{"description", "the stable, machine-actionable code, e.g. schema/unknown-field"},
			{"type", "string"},
		}},
		{"column", omap{
			{"description", "1-based column in the authored YAML, when the source was available"},
			{"type", "integer"},
		}},
		{"docsUrl", omap{
			{"description", "documentation page for this code"},
			{"type", "string"},
		}},
		{"field", omap{
			{"description", "JSONPath-like location within the document, e.g. $.spec.components[2].port"},
			{"type", "string"},
		}},
		{"line", omap{
			{"description", "1-based line in the authored YAML, when the source was available"},
			{"type", "integer"},
		}},
		{"message", omap{
			{"description", "what is wrong"},
			{"type", "string"},
		}},
		{"remediation", omap{
			{"description", "what to do about it, stated as an action"},
			{"type", "string"},
		}},
		{"resource", omap{
			{"description", "the document the error is about, e.g. Environment/production"},
			{"type", "string"},
		}},
	})

	var m omap
	m.set("description", "what validate.go said about this document. An invalid spec is a status "+
		"and not a rejection: nothing renders, nothing is published, and the previous revision keeps serving.")
	m.set("type", "array")
	m.set("items", item)
	return m
}

func observedGenerationSchema() omap {
	var m omap
	m.set("description", "the .metadata.generation this status describes. A status whose "+
		"observedGeneration trails the object's generation has not caught up yet.")
	m.set("type", "integer")
	m.set("format", "int64")
	return m
}

func projectStatusSchema() omap {
	var m omap
	m.set("description", "ProjectStatus is what the controller observed about this Project. "+
		"A Project has no delivery of its own, so its status is validation and nothing else.")
	m.set("type", "object")
	m.set("properties", omap{
		{"conditions", conditionsSchema("the observed conditions, with Ready as the summary")},
		{"observedGeneration", observedGenerationSchema()},
		{"validationErrors", validationErrorsSchema()},
	})
	return m
}

func environmentStatusSchema() omap {
	var historyItem omap
	historyItem.set("type", "object")
	historyItem.set("required", []string{"revision"})
	historyItem.set("properties", omap{
		{"digest", omap{
			{"description", "the artifact's OCI digest — what was put there, as opposed to where"},
			{"type", "string"},
		}},
		{"images", omap{
			{"description", "the container images this revision resolved to, in component order"},
			{"type", "array"},
			{"items", omap{{"type", "string"}}},
		}},
		{"outcome", omap{
			{"description", "the delivery phase this revision reached: Proposed, Committed, " +
				"Reconciling, Applied, Healthy, Degraded or Rejected"},
			{"type", "string"},
		}},
		{"revision", omap{
			{"description", "the artifact tag: <generation>-<spec-hash-short>"},
			{"type", "string"},
		}},
		{"specHash", omap{
			{"description", "hash of the resolved spec this revision was rendered from"},
			{"type", "string"},
		}},
		{"timestamp", omap{
			{"description", "when the entry was recorded"},
			{"type", "string"},
			{"format", "date-time"},
		}},
	})

	var history omap
	history.set("description", "the bounded mirror of published revisions, newest first. The record "+
		"is the registry's tag list; this is a window onto it for humans and for the API (ADR-0028 decision 4).")
	history.set("type", "array")
	// The bound is the schema's, not only the controller's: a status that could
	// grow without limit would put the whole deployment history into every watch
	// event every controller in the cluster receives.
	history.set("maxItems", int64(20))
	history.set("items", historyItem)

	var m omap
	m.set("description", "EnvironmentStatus is what the controller observed about this Environment: "+
		"the validation record a Project also has, plus the delivery record.")
	m.set("type", "object")
	m.set("properties", omap{
		{"conditions", conditionsSchema("the observed conditions, with Ready as the summary")},
		{"history", history},
		{"observedGeneration", observedGenerationSchema()},
		{"phase", omap{
			{"description", "the delivery state-machine phase: Proposed, Committed, Reconciling, " +
				"Applied, Healthy, Degraded or Rejected"},
			{"type", "string"},
		}},
		{"revision", omap{
			{"description", "the settled revision — the artifact tag the OCIRepository is pinned to"},
			{"type", "string"},
		}},
		{"rollbackGeneration", omap{
			{"description", "the .metadata.generation the rollback was applied at. A generation past " +
				"this one means the spec was edited since, which resumes normal publishing " +
				"(ADR-0028 decision 5)."},
			{"type", "integer"},
			{"format", "int64"},
		}},
		{"rollbackRevision", omap{
			{"description", "the kelson.dev/rollback-to value the controller has acted on"},
			{"type", "string"},
		}},
		{"validationErrors", validationErrorsSchema()},
	})
	return m
}
