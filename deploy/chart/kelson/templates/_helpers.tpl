{{/*
Names. Standard Helm shapes; nothing clever, because the names appear in RBAC
subjects and in the uninstall instructions and both have to be predictable.
*/}}
{{- define "kelson.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "kelson.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "kelson.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Labels. The standard set, plus one label that exists for the uninstall story
(issue #59): kelson.dev/install is on EVERY object the chart creates, namespaced
and cluster-scoped alike, so

  kubectl delete <kinds> -A -l kelson.dev/install=<release>

is a complete statement of what the chart added. `helm uninstall` does the same
thing through the release record; the label is what makes the claim checkable
without trusting Helm's bookkeeping.
*/}}
{{- define "kelson.labels" -}}
helm.sh/chart: {{ include "kelson.chart" . }}
{{ include "kelson.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/part-of: kelson
app.kubernetes.io/managed-by: {{ .Release.Service }}
kelson.dev/install: {{ .Release.Name }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{- define "kelson.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kelson.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "kelson.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "kelson.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
The namespace the spec and history ConfigMaps live in (--namespace). It is also
the namespace whose Role grants those ConfigMaps, so the two cannot drift.
*/}}
{{- define "kelson.stateNamespace" -}}
{{- default .Release.Namespace .Values.server.namespace }}
{{- end }}

{{/*
The image reference. `required` rather than a default, for the reason
values.yaml gives: a mutable tag makes a Deployment's identity unknowable.
*/}}
{{- define "kelson.image" -}}
{{- $tag := required "image.tag is required: the chart does not default to `latest`, because a mutable tag makes a Deployment's identity unknowable, and it does not default to the chart's appVersion either, because that is a development placeholder rather than a published image. Pass the release you mean, e.g. --set image.tag=v0.1.0" .Values.image.tag -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end }}

{{/*
kelson-controller's names and image (ADR-0027 decision 2). It is a second
workload in the same release rather than a chart of its own: it is the same
project, the same version and the same namespace, and splitting it would make
"install kelson" two commands whose versions can disagree.

The image tag falls back to the server's, because the two binaries are cut from
one tag by one goreleaser run — pinning them separately would be a way to run a
controller and a server from different builds without noticing.
*/}}
{{- define "kelson.controller.fullname" -}}
{{- printf "%s-controller" (include "kelson.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "kelson.controller.serviceAccountName" -}}
{{- if .Values.controller.serviceAccount.create }}
{{- default (include "kelson.controller.fullname" .) .Values.controller.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.controller.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "kelson.controller.image" -}}
{{- $tag := default .Values.image.tag .Values.controller.image.tag -}}
{{- $tag = required "controller.image.tag (or image.tag) is required: the chart does not default to `latest`, because a mutable tag makes a Deployment's identity unknowable. Pass the release you mean, e.g. --set image.tag=v0.1.0" $tag -}}
{{- printf "%s:%s" .Values.controller.image.repository $tag -}}
{{- end }}

{{/*
The auth gate.

In-cluster the server binds 0.0.0.0, which the binary itself refuses without a
password unless --insecure-bind says so out loud (cmd/kelson-server, ADR-0013
§3). This reproduces that refusal at render time: the same posture decision,
made before anything is applied to the cluster rather than after the first pod
crashes.

Every template that could leak the decision includes this first, so there is no
render path that skips it.
*/}}
{{- define "kelson.authGate" -}}
{{- $secret := and .Values.auth.existingSecret .Values.auth.existingSecret.name -}}
{{- if and $secret .Values.auth.password -}}
{{- fail "auth.existingSecret.name and auth.password are both set. Pick one: the chart would otherwise have to guess which password the server should trust." -}}
{{- end -}}
{{- if and .Values.auth.insecure (or $secret .Values.auth.password) -}}
{{- fail "auth.insecure is set alongside a password. --insecure-bind means `serve with no authentication, deliberately`; with a password configured it is neither true nor needed. Unset auth.insecure." -}}
{{- end -}}
{{- if not (or $secret .Values.auth.password .Values.auth.insecure) -}}
{{- fail "\nkelson-server has no password configured.\n\nIn a cluster the server binds 0.0.0.0 (a Service cannot reach loopback), and serving that with no authentication means anything that can reach the Service can deploy to your cluster. The binary refuses to start in that posture and so does this chart.\n\nSet exactly one of:\n\n  auth.existingSecret.name=<secret>   preferred — the chart references it and never sees the value\n  auth.password=<literal>             the value ends up in the Helm release record too\n  auth.insecure=true                  serve with no authentication, deliberately (--insecure-bind)\n\nThere is still no TLS in kelson-server: put a TLS-terminating proxy in front of the Service, or reach it with kubectl port-forward. See docs/server.md and issue #84.\n" -}}
{{- end -}}
{{- end }}

{{/*
Where the shared password comes from: the Secret the operator made, or the one
the chart made from a literal. Both end as a secretKeyRef — the value never
reaches the pod spec as plaintext either way.
*/}}
{{- define "kelson.passwordSecretName" -}}
{{- if and .Values.auth.existingSecret .Values.auth.existingSecret.name -}}
{{- .Values.auth.existingSecret.name -}}
{{- else if .Values.auth.password -}}
{{- printf "%s-auth" (include "kelson.fullname" .) -}}
{{- end -}}
{{- end }}

{{- define "kelson.passwordSecretKey" -}}
{{- if and .Values.auth.existingSecret .Values.auth.existingSecret.name -}}
{{- default "password" .Values.auth.existingSecret.key -}}
{{- else -}}
password
{{- end -}}
{{- end }}
