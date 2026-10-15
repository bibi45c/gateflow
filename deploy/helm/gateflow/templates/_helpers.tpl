{{- define "gf.labels" -}}
{{ dict "app.kubernetes.io/name" "gateflow" "app.kubernetes.io/instance" .Release.Name | toJson }}
{{- end -}}
{{- define "gf.image" -}}
{{- if .service.digest -}}{{ .service.image }}@{{ .service.digest }}{{- else -}}{{ .service.image }}:{{ .root.Values.imageTag }}{{- end -}}
{{- end -}}
{{- define "gf.env" -}}
{{- $env := list -}}{{- $r := .root -}}{{- $n := .name -}}
{{- range $key := list "ACCESS_INTERNAL_TOKEN" "METERING_INTERNAL_TOKEN" -}}
{{- if or (eq $n "gateway") (eq $n "metering") (and (eq $n "access") (eq $key "ACCESS_INTERNAL_TOKEN")) -}}
{{- $env = append $env (dict "name" $key "valueFrom" (dict "secretKeyRef" (dict "name" $r.Values.secretName "key" $key))) -}}
{{- end -}}{{- end -}}
{{- if or (eq $n "access") (eq $n "metering") -}}
{{- $env = append $env (dict "name" "DATABASE_URL" "valueFrom" (dict "secretKeyRef" (dict "name" $r.Values.secretName "key" (printf "%s_DATABASE_URL" (upper $n))))) -}}
{{- end -}}
{{- if eq $n "access" -}}
{{- range $key := list "ACCESS_KEY_PEPPER" "ACCESS_SEED_SECRET" -}}
{{- $env = append $env (dict "name" $key "valueFrom" (dict "secretKeyRef" (dict "name" $r.Values.secretName "key" $key))) -}}
{{- end -}}
{{- $env = append $env (dict "name" "MOCK_UPSTREAM_BASE_URL" "value" (printf "http://%s-mock-upstream:8090" $r.Release.Name)) -}}
{{- end -}}
{{- if or (eq $n "gateway") (eq $n "metering") -}}
{{- $env = append $env (dict "name" "ACCESS_URL" "value" (printf "http://%s-access:8081" $r.Release.Name)) -}}
{{- $env = append $env (dict "name" "METERING_URL" "value" (printf "http://%s-metering:8082" $r.Release.Name)) -}}
{{- end -}}
{{- range $key, $value := dict "SHUTDOWN_DRAIN_DELAY" (printf "%ds" (int $r.Values.shutdown.drain)) "SHUTDOWN_TIMEOUT" (printf "%ds" (int $r.Values.shutdown.timeout)) -}}
{{- $env = append $env (dict "name" $key "value" $value) -}}
{{- end -}}
{{ $env | toJson }}
{{- end -}}
