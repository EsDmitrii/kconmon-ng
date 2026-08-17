{{/*
Values that MOVED, refused loudly.

`helm upgrade` merges values by path: a key that no longer exists is not an error, it is a key
nobody reads. So an operator who kept `console.database.existingSecret` in their values file would
have upgraded successfully onto an in-memory console — the database silently detached, the history
gone, and nothing anywhere saying why. A rename is only safe when the old name fails.

The database and the Redis-compatible bus are not parts of the Console: they are the stack it runs
on, installed and pointed at independently. `console.database.*` and `console.valkey.*` said
otherwise, and made "bring your own PostgreSQL" read like a Console setting.

Called from every entrypoint template (the include is free when the values are absent).
*/}}
{{- define "kconmon-ng.migrations" -}}
{{- $console := .Values.console | default dict -}}
{{- if hasKey $console "database" -}}
{{- fail "console.database.* moved to the top-level database.*, and the chart no longer installs PostgreSQL: bring your own (CNPG, Percona, RDS, plain postgres) and point database.existingSecret at a Secret holding its postgres:// DSN. database.mode and database.cnpg.* are gone — a DSN is the whole configuration." -}}
{{- end -}}
{{- if hasKey $console "valkey" -}}
{{- fail "console.valkey.* moved to the top-level redis.*, and the chart no longer installs a bus: bring your own Redis-compatible server (Valkey, Redis, sentinel, ElastiCache) and point redis.existingSecret at a Secret holding its redis:// DSN. redis.mode, redis.address and the separate password Secret are gone — one DSN carries the host, the credentials, the database number and TLS (rediss://)." -}}
{{- end -}}
{{- if or (hasKey .Values "valkey") (hasKey .Values "cnpg-operator") -}}
{{- fail "the chart has NO subcharts any more: it consumes PostgreSQL, a Redis-compatible bus and Prometheus, and installs none of them. Install CloudNativePG / Valkey / anything equivalent however you already install infrastructure, then point database.existingSecret and redis.existingSecret at their DSN Secrets. The chart README documents the stack it is tested against." -}}
{{- end -}}
{{- if hasKey (.Values.database | default dict) "mode" -}}
{{- fail "database.mode is gone: a DSN is the whole configuration. Point database.existingSecret at a Secret holding a postgres:// DSN (or use database.secret.create), and leave it empty for the in-memory console." -}}
{{- end -}}
{{- if hasKey (.Values.database | default dict) "cnpg" -}}
{{- fail "database.cnpg.* is gone: the chart installs no PostgreSQL. This is the half-migration the mode guard above misses — moving console.database to the top level and leaving the cnpg block behind rendered a release with no database at all, silently, because nothing read the block and nothing complained. Install your Cluster however you install infrastructure and point database.existingSecret at its DSN Secret (CNPG publishes one as <cluster>-app, key uri); the chart README documents the Cluster it is tested against." -}}
{{- end -}}
{{- if or (hasKey (.Values.redis | default dict) "mode") (hasKey (.Values.redis | default dict) "address") -}}
{{- fail "redis.mode and redis.address are gone: a DSN is the whole configuration. Point redis.existingSecret at a Secret holding a redis:// DSN (or use redis.secret.create), and leave it empty for the single-replica in-process bus." -}}
{{- end -}}
{{- /* Keys that were deprecated and are now REMOVED. Each was a rename that the chart resolved for
       one release; carrying them further meant two names for one setting in every template that
       read them, and a values file that documented both. */ -}}
{{- $consoleNP := dig "networkPolicy" dict $console -}}
{{- if hasKey $consoleNP "valkeyEgress" -}}
{{- fail "console.networkPolicy.valkeyEgress was renamed to console.networkPolicy.redisEgress, following redis.* — a values file that kept the old name would have rendered the DEFAULT rule (TCP 6379 only) instead, so a TLS endpoint, a sentinel quorum or a managed service on any other port would have been blocked with the values file still reading as if it had been configured." -}}
{{- end -}}
{{- if hasKey $consoleNP "valkeyIngressFrom" -}}
{{- fail "console.networkPolicy.valkeyIngressFrom is removed: it admitted extra peers to the Valkey the chart used to install as a subchart, and the chart installs no bus any more. Write that rule wherever your Redis-compatible server is installed." -}}
{{- end -}}
{{- $ctrl := dig "controller" dict $console -}}
{{- if hasKey $ctrl "grpcAddr" -}}
{{- fail "console.controller.grpcAddr was renamed to console.controller.grpcAddress and is now removed." -}}
{{- end -}}
{{- if hasKey (dig "webhooks" dict $console) "encryptionKeySecret" -}}
{{- fail "console.webhooks.encryptionKeySecret.{name,key} were renamed to console.webhooks.existingSecret / existingSecretKey and are now removed." -}}
{{- end -}}
{{- if hasKey (dig "cnpg" "backup" dict (.Values.database | default dict)) "credentialsSecret" -}}
{{- fail "database.cnpg.backup.credentialsSecret was renamed to database.cnpg.backup.existingSecret and is now removed." -}}
{{- end -}}
{{- if or (hasKey (.Values.networkPolicy | default dict) "cnpgOperatorNamespace") (hasKey (.Values.networkPolicy | default dict) "cnpgOperatorPodLabels") -}}
{{- fail "networkPolicy.cnpgOperatorNamespace / cnpgOperatorPodLabels are removed: the chart no longer installs PostgreSQL, so it has no operator to open a path to. Write the policy where your database is installed." -}}
{{- end -}}
{{- if hasKey .Values "pdb" -}}
{{- fail "pdb.* moved to controller.pdb.* — it only ever rendered the CONTROLLER's PodDisruptionBudget, and the console's has always been console.pdb.*." -}}
{{- end -}}
{{- /* hasKey, not truthiness: a 1.x values file whose override list was EMPTIED rather than deleted
       (`prometheusRule.rules: []`) rendered clean and silently, while every other removed key here
       fails the render. */}}
{{- if hasKey (.Values.prometheusRule | default dict) "rules" -}}
{{- fail "prometheusRule.rules (the pre-1.12 full-override list) is removed: it replaced every built-in rule and silently discarded the per-rule knobs. Tune the built-ins with prometheusRule.<alertName>.{enabled,threshold,for,severity}, and append your own with prometheusRule.additionalRules." -}}
{{- end -}}
{{- /* NOT a migration: an INVARIANT the chart used to state and never check.

       Sessions, the fixed-window rate-limit counters and the realtime fan-out all live in the
       Redis-compatible server. Without one they live in each console process, so N replicas is N
       independent sets: console.rateLimit.promqlPerMinute: 60 actually admitted 120 arbitrary-PromQL
       proxy requests a minute against the operator's Prometheus, a session created on one replica
       was unknown to the other, and a run's live progress only reached the browsers the Service
       happened to route to its owner. The values file said "console.replicas must be 1" and nothing
       enforced it, while the default shipped 2. */}}
{{- if and .Values.console.enabled (gt (int .Values.console.replicas) 1) (not (include "kconmon-ng.console.hasRedis" .)) -}}
{{- fail "console.replicas is greater than 1 and no Redis-compatible server is configured: sessions, the rate-limit counters and the realtime fan-out are per-process without one, so the replicas would each keep their own — a session created on one is unknown to the other, and every console.rateLimit.* budget is multiplied by the replica count. Set redis.existingSecret (or redis.secret.create) to a redis:// DSN, or set console.replicas to 1." -}}
{{- end -}}
{{- /* NOT a migration either: the THREE PORTS must differ.

       config.httpPort, config.grpcPort and config.metricsPort become containerPorts and Service
       ports on the same Pod. Two of them equal renders a Service the apiserver rejects
       ("duplicate port") and a container spec the kubelet refuses — after `helm upgrade` has already
       started applying, so the release is left half-changed. The binaries validate this at startup
       (internal/config's Validate), which is far too late: by then the object is live and the pod is
       crash-looping. The chart knows all three values before it writes anything. */}}
{{- $ports := dict "httpPort" (int .Values.config.httpPort) "grpcPort" (int .Values.config.grpcPort) "metricsPort" (int .Values.config.metricsPort) -}}
{{- if eq $ports.httpPort $ports.grpcPort -}}
{{- fail (printf "config.httpPort and config.grpcPort are both %d: they are two ports on the same Pod and must differ" $ports.httpPort) -}}
{{- end -}}
{{- if eq $ports.httpPort $ports.metricsPort -}}
{{- fail (printf "config.httpPort and config.metricsPort are both %d: /metrics is on a listener of its OWN precisely so a scrape rule need not open the API port, and sharing the number puts them back together" $ports.httpPort) -}}
{{- end -}}
{{- if eq $ports.grpcPort $ports.metricsPort -}}
{{- fail (printf "config.grpcPort and config.metricsPort are both %d: they are two ports on the same Pod and must differ" $ports.grpcPort) -}}
{{- end -}}
{{- /* The CONSOLE's own pair. console.service.port and config.metricsPort land on the same console
       Pod and the same console Service, so equal numbers render a Service with two entries on one
       port — accepted by helm, rejected by the apiserver at apply time, i.e. after the upgrade has
       started. The guard above covered the agent/controller trio and missed this one. */}}
{{- if and .Values.console.enabled (eq (int .Values.console.service.port) $ports.metricsPort) -}}
{{- fail (printf "console.service.port and config.metricsPort are both %d: they are two ports on the same console Pod and Service, so the Service would carry the number twice and the apiserver would refuse it" $ports.metricsPort) -}}
{{- end -}}
{{- end }}
