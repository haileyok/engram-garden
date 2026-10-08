# Monitoring

`engram-appview` and `engram-web` expose Prometheus metrics and write
structured JSON logs. `deploy/monitoring/` has what a host needs to collect
them: a Grafana dashboard, Prometheus alert rules, scrape config, and a
Grafana Alloy config that does both metrics and logs.

## Turning metrics on

Metrics are served on a listener of their own, separate from the public
one, so they're never reachable through the public URL. They're off until
you give them an address:

| Service | Setting | Example |
| --- | --- | --- |
| appview | `ENGRAM_METRICS_LISTEN` | `:9464` |
| web app | `ENGRAM_WEB_METRICS_LISTEN` | `:9465` |

Both serve `GET /metrics` in the Prometheus text format. Expose the port only
to your monitoring network: in Docker Compose, publish it on loopback
(`127.0.0.1:9464:9464`) or put Prometheus on the same Docker network.

`ENGRAM_METRICS_PER_SPACE=true` adds gauges labeled with each loaded space's
URI (memories, buffered changes, segments, RAM). Leave it off on an appview
with many spaces: every space becomes four more series.

## Collecting

Pick one:

- **Prometheus scrapes directly.** Add the jobs in
  [`deploy/monitoring/prometheus/scrape.yml`](../deploy/monitoring/prometheus/scrape.yml)
  and load [`engram-alerts.yml`](../deploy/monitoring/prometheus/engram-alerts.yml).
- **Grafana Alloy** scrapes and remote-writes to Prometheus (or Mimir, or
  Grafana Cloud), and ships logs to Loki:
  [`deploy/monitoring/alloy/engram.alloy`](../deploy/monitoring/alloy/engram.alloy).
  It reads container logs through the Docker socket and needs
  `PROMETHEUS_URL`, `LOKI_URL` and `ENGRAM_HOST` set.

Keep the job names `engram-appview` and `engram-web`: the alert rules use
them. The dashboard lets you pick any job.

## The dashboard

Import [`deploy/monitoring/grafana/engram.json`](../deploy/monitoring/grafana/engram.json)
(Dashboards → New → Import). Its variables choose the Prometheus and Loki
data sources, the appview and web jobs, the instance, and the LogQL stream
selector for Engram's logs (default `{compose_project="engram-garden"}`; set
it to match your log shipper's labels). To change the dashboard, edit
`generate.py` next to it and run it, rather than editing the JSON.

Rows: an overview, indexing and notifications, search and API, index
storage, the web app, the processes, and logs.

## Logs

Each line is a JSON object from Go's `log/slog`: `time`, `level` (`DEBUG`,
`INFO`, `WARN`, `ERROR`), `msg`, then fields such as `space`, `repo` and
`err`. In LogQL, `| json` parses them:

```logql
{compose_project="engram-garden"} | json | level="WARN" or level="ERROR"
{compose_service="appview"} | json | msg="synced repo" | line_format "{{.space}} {{.repo}} +{{.upserts}} -{{.deletes}}"
{compose_service="appview"} | json | msg="rejected a write notification"
```

## Metrics

All of Engram's own metrics start with `engram_`. Both services also export
the Go runtime's (`go_*`) and the process's (`process_*`) metrics, and
`engram_build_info{version, revision}`.

### HTTP (both services)

| Metric | Type | Labels |
| --- | --- | --- |
| `engram_http_requests_total` | counter | `route` (the matched route pattern, like `GET /xrpc/garden.engram.searchMemories`, or `unmatched`), `method`, `code` |
| `engram_http_request_duration_seconds` | histogram | `route` |
| `engram_http_requests_in_flight` | gauge | |

### Freshness and indexing (appview)

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `engram_index_lag_seconds` | histogram | | From a repo commit to its changes being in the index, for incremental syncs: how long a new memory takes to become searchable. Seconds when notifications arrive; up to `ENGRAM_POLL_INTERVAL` when they don't. |
| `engram_notifications_total` | counter | `kind` (`write`, `space_deleted`), `result` | Notifications from authorities. `accepted` and `deferred` (the space was at its sync cap; a running sync covers it) are normal. `bad_body`, `unauthorized`, `forbidden`, `unknown_space` and `error` are refusals, which authorities don't retry. |
| `engram_notified_syncs_total` | counter | `result` (`ok`, `error`, `no_slot`) | Repo syncs started by a notification. |
| `engram_notify_registrations_total` | counter | `result` | Requests asking authorities to send notifications (renewed about every 12 hours per space). |
| `engram_repo_syncs_total` | counter | `mode` (`incremental`, `full`), `result` | |
| `engram_repo_sync_duration_seconds` | histogram | `mode` | |
| `engram_repo_reexports_total` | counter | | Incremental syncs that couldn't be applied or verified, so the repo was exported in full. |
| `engram_records_indexed_total` | counter | `op` (`upsert`, `delete`) | |
| `engram_index_over_limit_total` | counter | | Repo changes refused, wholly or partly, by a per-space limit. |
| `engram_space_syncs_total` | counter | `result` | Whole-space syncs (the poll, and catching up after missed notifications). |
| `engram_space_sync_duration_seconds` | histogram | | |
| `engram_spaces_indexed` | gauge | | Spaces the appview indexes. |
| `engram_spaces_owned` | gauge | | Indexed spaces this node owns and may read. |

### Search and index storage (appview)

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `engram_searches_total` | counter | `result` (`ok`, `rate_limited`, `retry`, `canceled`, `error`) | |
| `engram_search_duration_seconds` | histogram | | Including loading the space. |
| `engram_spaces_loaded` | gauge | | Spaces in memory. |
| `engram_index_ram_bytes`, `engram_index_ram_budget_bytes` | gauge | | RAM used by loaded spaces, and `ENGRAM_RAM_BYTES`. |
| `engram_memories` | gauge | | Memories in the loaded spaces. |
| `engram_buffered_memories` | gauge | | Changes waiting in write buffers. |
| `engram_segments` | gauge | | Segment files of the loaded spaces. |
| `engram_flushes_total` | counter | `result` | Write buffer flushes. |
| `engram_flush_duration_seconds` | histogram | | |
| `engram_maintenance_total` | counter | `kind` (`merge`, `gc`), `result` | |
| `engram_maintenance_duration_seconds` | histogram | `kind` | |
| `engram_space_loads_total` | counter | `result` | |
| `engram_space_load_duration_seconds` | histogram | | |
| `engram_space_evictions_total` | counter | | Clean spaces dropped to stay within the RAM budget. |
| `engram_segment_reads_total`, `engram_segment_read_bytes_total` | counter | `source` (`disk_cache`, `object_storage`) | |
| `engram_disk_cache_bytes`, `engram_disk_cache_budget_bytes`, `engram_disk_cache_files` | gauge | | |
| `engram_blob_requests_total` | counter | `op`, `result` (`ok`, `not_found`, `exists`, `error`) | Object storage calls. |
| `engram_blob_request_duration_seconds` | histogram | `op` | |
| `engram_blob_bytes_total` | counter | `direction` (`put`, `get_range`) | |
| `engram_space_memories`, `engram_space_buffered_memories`, `engram_space_segments`, `engram_space_ram_bytes` | gauge | `space` | Only with `ENGRAM_METRICS_PER_SPACE`. |

### Web app

| Metric | Type | Labels |
| --- | --- | --- |
| `engram_web_signins_total` | counter | `result` (`ok`, `partial`, `declined`, `failed`, `refused`) |

## Alerts

[`engram-alerts.yml`](../deploy/monitoring/prometheus/engram-alerts.yml)
covers: a service down, notifications refused, new memories slow to become
searchable, failing repo syncs, object storage errors, failing flushes, 5xx
responses, slow searches, and the RAM budget filling up. The thresholds are
starting points.
