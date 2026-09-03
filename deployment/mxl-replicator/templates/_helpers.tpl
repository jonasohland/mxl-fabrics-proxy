{{/*
Names.
*/}}
{{- define "mxl-replicator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "mxl-replicator.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "mxl-replicator.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Labels. `commonLabels` is merged in last so an operator can override anything the chart sets.
*/}}
{{- define "mxl-replicator.labels" -}}
helm.sh/chart: {{ include "mxl-replicator.chart" . }}
app.kubernetes.io/name: {{ include "mxl-replicator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- with .Chart.AppVersion }}
app.kubernetes.io/version: {{ . | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: mxl-replicator
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/*
Selector labels for one component. Immutable on a Deployment and a DaemonSet, so `commonLabels`
is deliberately *not* in here: adding a label after the fact would otherwise make an upgrade fail.

Usage: include "mxl-replicator.selectorLabels" (dict "root" $ "component" "agent")
*/}}
{{- define "mxl-replicator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "mxl-replicator.name" .root }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- define "mxl-replicator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "mxl-replicator.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
The server image.
*/}}
{{- define "mxl-replicator.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- if .Values.image.registry -}}
{{- printf "%s/%s:%s" .Values.image.registry .Values.image.repository $tag -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}
{{- end -}}

{{/*
The agent image. Same as the server's unless `agent.image.tag` names another, which is what an EFA
deployment does: the EFA build carries a libfabric with the provider compiled in and the stock
image does not.

Usage: include "mxl-replicator.agentImage" (dict "root" $ "agent" $agent)
*/}}
{{- define "mxl-replicator.agentImage" -}}
{{- $root := .root -}}
{{- $tag := default (default $root.Chart.AppVersion $root.Values.image.tag) (dig "image" "tag" "" .agent) -}}
{{- if $root.Values.image.registry -}}
{{- printf "%s/%s:%s" $root.Values.image.registry $root.Values.image.repository $tag -}}
{{- else -}}
{{- printf "%s:%s" $root.Values.image.repository $tag -}}
{{- end -}}
{{- end -}}

{{- define "mxl-replicator.imagePullSecrets" -}}
{{- with .Values.image.pullSecrets }}
imagePullSecrets:
{{- range . }}
  - name: {{ . }}
{{- end }}
{{- end }}
{{- end -}}

{{/*
Auth.

The Secret's name, whether the chart owns it or not.
*/}}
{{- define "mxl-replicator.authSecretName" -}}
{{- if .Values.auth.existingSecret -}}
{{- .Values.auth.existingSecret -}}
{{- else -}}
{{- printf "%s-token" (include "mxl-replicator.fullname" .) -}}
{{- end -}}
{{- end -}}

{{- define "mxl-replicator.authSecretKey" -}}
{{- if .Values.auth.existingSecret -}}
{{- .Values.auth.existingSecretKey -}}
{{- else -}}
token
{{- end -}}
{{- end -}}

{{/*
The token itself, for the Secret the chart creates.

An explicit value wins. Otherwise the existing Secret is read back, so an upgrade does not rotate
the token out from under a running fleet, and only a first install generates one. `helm template`
has no cluster to read and so emits a fresh token on every render, which is why the values file
says not to pipe it into `kubectl apply` for an existing release.
*/}}
{{- define "mxl-replicator.authToken" -}}
{{- if .Values.auth.token -}}
{{- .Values.auth.token -}}
{{- else -}}
{{- $name := printf "%s-token" (include "mxl-replicator.fullname" .) -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace $name -}}
{{- if and $existing (dig "data" "token" "" $existing) -}}
{{- index $existing.data "token" | b64dec -}}
{{- else -}}
{{- randAlphaNum 48 -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
The MXL_REPLICATOR_AUTH_TOKEN env entry, or nothing when auth is off. Both roles read the same
variable.
*/}}
{{- define "mxl-replicator.authEnv" -}}
{{- if .Values.auth.enabled }}
- name: MXL_REPLICATOR_AUTH_TOKEN
  valueFrom:
    secretKeyRef:
      name: {{ include "mxl-replicator.authSecretName" . }}
      key: {{ include "mxl-replicator.authSecretKey" . }}
{{- end }}
{{- end -}}

{{/*
The control-plane URL an agent points at. Explicitly configured servers win, and otherwise the
in-cluster Service, with the scheme following whether the server terminates TLS itself.
*/}}
{{- define "mxl-replicator.serverURLs" -}}
{{- if .Values.agent.servers -}}
{{- toYaml .Values.agent.servers -}}
{{- else -}}
{{- $scheme := ternary "https" "http" .Values.server.tls.enabled -}}
{{- printf "- %s://%s.%s.svc:%v" $scheme (include "mxl-replicator.fullname" .) .Release.Namespace .Values.server.service.port -}}
{{- end -}}
{{- end -}}

{{/*
The name of the PVC the server uses, when it uses one. An existing claim is named as given, and
otherwise the chart creates one under its own name.
*/}}
{{- define "mxl-replicator.storeClaimName" -}}
{{- if .Values.server.persistence.pvc.existingClaim -}}
{{- .Values.server.persistence.pvc.existingClaim -}}
{{- else -}}
{{- printf "%s-store" (include "mxl-replicator.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
The sqlite store file, always inside the mounted volume so it cannot be written to the container's
ephemeral filesystem and lost on restart.
*/}}
{{- define "mxl-replicator.storePath" -}}
{{- printf "%s/store.db" (trimSuffix "/" .Values.server.persistence.mountPath) -}}
{{- end -}}

{{/*
The agent's configuration file.

Areas and fabric attachments are lists of records, which is what does not fit on a command line, so
they go in a file. `node` is deliberately absent. It comes from MXL_REPLICATOR_NODE, which is the
pod's own spec.nodeName.

Usage: include "mxl-replicator.agentConfig" (dict "root" $ "agent" $agent)
*/}}
{{- define "mxl-replicator.agentConfig" -}}
server:
{{ include "mxl-replicator.serverURLs" .root | indent 2 }}
{{- with .agent.areas }}
areas:
{{ toYaml . | indent 2 }}
{{- end }}
{{- with .agent.fabrics }}
fabrics:
{{ toYaml . | indent 2 }}
{{- end }}
{{- end -}}

{{/*
Every area path must be inside a host mount, or the agent advertises a directory the container
cannot see. That presents as a node which discovers no domains and accepts no destinations, a long
way from the line that caused it.

Usage: include "mxl-replicator.checkAreaMounts" (dict "root" $ "agent" $agent "pool" $name)
*/}}
{{- define "mxl-replicator.checkAreaMounts" -}}
{{- $agent := .agent -}}
{{- range $area := $agent.areas -}}
{{- $covered := false -}}
{{- range $mount := $agent.hostMounts -}}
{{- $prefix := trimSuffix "/" $mount.mountPath -}}
{{- if or (eq $area.path $mount.mountPath) (hasPrefix (printf "%s/" $prefix) $area.path) -}}
{{- $covered = true -}}
{{- end -}}
{{- end -}}
{{- if not $covered -}}
{{- fail (printf "agent pool %q: area %q has path %q, which is not inside any agent.hostMounts entry, so the container cannot see it. Add a hostMounts entry covering it, or correct the path." $.pool $area.name $area.path) -}}
{{- end -}}
{{- end -}}
{{- end -}}

