{{- /*
Common labels for every resource managed by this chart.
*/}}
{{- define "host-fixtures.labels" -}}
app.kubernetes.io/name: host-fixtures
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
fixture/scenario: {{ .Values.scenario | quote }}
{{- end }}

{{- /*
Per-host name used for ConfigMap and Deployment.
Call as: include "host-fixtures.hostName" (dict "Release" $.Release "host" .name)
*/}}
{{- define "host-fixtures.hostName" -}}
{{- printf "%s-%s" .Release.Name .host | trunc 63 | trimSuffix "-" -}}
{{- end -}}
