#!/usr/bin/env python3
"""Generates engram.json, the Grafana dashboard for Engram Garden.

    python3 deploy/monitoring/grafana/generate.py

Edit this file rather than the JSON, then import engram.json into Grafana
(Dashboards > New > Import, or the API). The dashboard picks its Prometheus
and Loki data sources, job and instance through variables, so it fits however
your scrape config labels things. Stdlib only.
"""
import json
import pathlib

HERE = pathlib.Path(__file__).resolve().parent
PROM = {"type": "prometheus", "uid": "${prometheus}"}
LOKI = {"type": "loki", "uid": "${loki}"}
APPVIEW = 'job=~"$appview_job", instance=~"$instance"'
WEB = 'job=~"$web_job", instance=~"$instance"'
ALL = 'job=~"$appview_job|$web_job", instance=~"$instance"'

panels = []
_y = 0
_ref = 0


def _next_id():
    return len(panels) + 1


def row(title):
    global _y
    panels.append({"type": "row", "title": title, "id": _next_id(), "collapsed": False,
                   "gridPos": {"h": 1, "w": 24, "x": 0, "y": _y}, "panels": []})
    _y += 1


_x = 0
_row_h = 0


def _place(w, h):
    global _x, _y, _row_h
    if _x + w > 24:
        _x, _y, _row_h = 0, _y + _row_h, 0
    pos = {"h": h, "w": w, "x": _x, "y": _y}
    _x += w
    _row_h = max(_row_h, h)
    return pos


def newline():
    global _x, _y, _row_h
    if _x:
        _x, _y, _row_h = 0, _y + _row_h, 0


def targets(exprs):
    out = []
    for i, (expr, legend) in enumerate(exprs):
        out.append({"refId": chr(ord("A") + i), "datasource": PROM, "expr": expr, "legendFormat": legend})
    return out


def timeseries(title, exprs, unit="short", w=8, h=8, desc="", stack=False, min0=True):
    custom = {"lineWidth": 1, "fillOpacity": 10, "showPoints": "never", "spanNulls": True}
    if stack:
        custom["stacking"] = {"mode": "normal", "group": "A"}
        custom["fillOpacity"] = 40
    defaults = {"unit": unit, "custom": custom}
    if min0:
        defaults["min"] = 0
    panels.append({"type": "timeseries", "title": title, "description": desc, "id": _next_id(),
                   "datasource": PROM, "gridPos": _place(w, h), "targets": targets(exprs),
                   "fieldConfig": {"defaults": defaults, "overrides": []},
                   "options": {"legend": {"displayMode": "list", "placement": "bottom"},
                               "tooltip": {"mode": "multi", "sort": "desc"}}})


def stat(title, expr, unit="short", w=4, h=4, desc="", thresholds=None, decimals=None, mappings=None):
    defaults = {"unit": unit, "color": {"mode": "thresholds"},
                "thresholds": {"mode": "absolute", "steps": thresholds or [{"color": "green", "value": None}]}}
    if decimals is not None:
        defaults["decimals"] = decimals
    if mappings:
        defaults["mappings"] = mappings
    panels.append({"type": "stat", "title": title, "description": desc, "id": _next_id(), "datasource": PROM,
                   "gridPos": _place(w, h),
                   "targets": [{"refId": "A", "datasource": PROM, "expr": expr, "instant": True}],
                   "fieldConfig": {"defaults": defaults, "overrides": []},
                   "options": {"reduceOptions": {"calcs": ["lastNotNull"]}, "colorMode": "value",
                               "graphMode": "none", "textMode": "auto"}})


def q(quantile, metric, sel, by="", window="5m"):
    group = f"le{', ' + by if by else ''}"
    return f"histogram_quantile({quantile}, sum by ({group}) (rate({metric}_bucket{{{sel}}}[{window}])))"


def red(v):
    return [{"color": "green", "value": None}, {"color": "red", "value": v}]


# ---- Overview ----
row("Overview")
stat("Appview up", f"min(up{{{APPVIEW}}})", w=3, desc="Whether Prometheus can scrape the appview's metrics listener.",
     thresholds=[{"color": "red", "value": None}, {"color": "green", "value": 1}],
     mappings=[{"type": "value", "options": {"0": {"text": "down"}, "1": {"text": "up"}}}])
stat("Web up", f"min(up{{{WEB}}})", w=3,
     thresholds=[{"color": "red", "value": None}, {"color": "green", "value": 1}],
     mappings=[{"type": "value", "options": {"0": {"text": "down"}, "1": {"text": "up"}}}])
stat("Spaces indexed", f"sum(engram_spaces_indexed{{{APPVIEW}}})", w=3)
stat("Memories (loaded spaces)", f"sum(engram_memories{{{APPVIEW}}})", w=3)
stat("Index lag p95", q(0.95, "engram_index_lag_seconds", APPVIEW, window="30m"), unit="s", w=4, decimals=1,
     desc="Time from a repo commit to its memory being searchable (incremental syncs). Near-instant when write notifications arrive; up to the poll interval when they don't.",
     thresholds=[{"color": "green", "value": None}, {"color": "orange", "value": 30}, {"color": "red", "value": 120}])
stat("Rejected notifications (1h)",
     f'sum(increase(engram_notifications_total{{{APPVIEW}, result!~"accepted|deferred"}}[1h]))', w=4,
     desc="Authorities don't retry a rejected notification; the write waits for the next poll.", thresholds=red(1), decimals=0)
stat("5xx responses (1h)", f'sum(increase(engram_http_requests_total{{{ALL}, code=~"5.."}}[1h]))', w=4,
     thresholds=red(1), decimals=0)
newline()
timeseries("Requests by service", [
    (f"sum by (job) (rate(engram_http_requests_total{{{ALL}}}[$__rate_interval]))", "{{job}}")], unit="reqps")
timeseries("Error responses", [
    (f'sum by (job, code) (rate(engram_http_requests_total{{{ALL}, code=~"[45].."}}[$__rate_interval]))', "{{job}} {{code}}")],
    unit="reqps")
timeseries("Memories becoming searchable", [
    (q(0.5, "engram_index_lag_seconds", APPVIEW), "p50"),
    (q(0.95, "engram_index_lag_seconds", APPVIEW), "p95"),
    (q(0.99, "engram_index_lag_seconds", APPVIEW), "p99")], unit="s",
    desc="Index lag: from a repo commit to the change being in the index.")

# ---- Indexing ----
row("Indexing and notifications")
timeseries("Notifications by result", [
    (f"sum by (kind, result) (rate(engram_notifications_total{{{APPVIEW}}}[$__rate_interval]))", "{{kind}} {{result}}")],
    unit="ops", desc="accepted and deferred are good; anything else means an authority's notification was refused.")
timeseries("Notified syncs", [
    (f"sum by (result) (rate(engram_notified_syncs_total{{{APPVIEW}}}[$__rate_interval]))", "{{result}}")], unit="ops")
timeseries("Repo syncs", [
    (f"sum by (mode, result) (rate(engram_repo_syncs_total{{{APPVIEW}}}[$__rate_interval]))", "{{mode}} {{result}}")],
    unit="ops")
timeseries("Repo sync time p95", [
    (q(0.95, "engram_repo_sync_duration_seconds", APPVIEW, by="mode"), "{{mode}}")], unit="s")
timeseries("Records indexed", [
    (f"sum by (op) (rate(engram_records_indexed_total{{{APPVIEW}}}[$__rate_interval]))", "{{op}}")], unit="ops")
timeseries("Re-exports, over-limit, space syncs", [
    (f"sum(rate(engram_repo_reexports_total{{{APPVIEW}}}[$__rate_interval]))", "re-exports"),
    (f"sum(rate(engram_index_over_limit_total{{{APPVIEW}}}[$__rate_interval]))", "over limit"),
    (f'sum by (result) (rate(engram_space_syncs_total{{{APPVIEW}}}[$__rate_interval]))', "space syncs {{result}}")],
    unit="ops")
timeseries("Space sync time p95", [(q(0.95, "engram_space_sync_duration_seconds", APPVIEW), "p95")], unit="s")
timeseries("Notification registrations", [
    (f"sum by (result) (increase(engram_notify_registrations_total{{{APPVIEW}}}[1h]))", "{{result}}")],
    desc="Requests asking authorities to send write notifications; renewed about every 12 hours per space.")
timeseries("Spaces", [
    (f"sum(engram_spaces_indexed{{{APPVIEW}}})", "indexed"),
    (f"sum(engram_spaces_owned{{{APPVIEW}}})", "owned by these nodes"),
    (f"sum(engram_spaces_loaded{{{APPVIEW}}})", "loaded in memory")])

# ---- Search and API ----
row("Search and API")
timeseries("Appview requests by route", [
    (f"sum by (route) (rate(engram_http_requests_total{{{APPVIEW}}}[$__rate_interval]))", "{{route}}")], unit="reqps", w=12)
timeseries("Appview latency p95 by route", [
    (q(0.95, "engram_http_request_duration_seconds", APPVIEW, by="route"), "{{route}}")], unit="s", w=12)
timeseries("Searches by result", [
    (f"sum by (result) (rate(engram_searches_total{{{APPVIEW}}}[$__rate_interval]))", "{{result}}")], unit="ops")
timeseries("Search time", [
    (q(0.5, "engram_search_duration_seconds", APPVIEW), "p50"),
    (q(0.95, "engram_search_duration_seconds", APPVIEW), "p95"),
    (q(0.99, "engram_search_duration_seconds", APPVIEW), "p99")], unit="s")
timeseries("Requests in flight", [
    (f"sum by (job) (engram_http_requests_in_flight{{{ALL}}})", "{{job}}")])

# ---- Storage ----
row("Index storage")
timeseries("Index RAM", [
    (f"sum(engram_index_ram_bytes{{{APPVIEW}}})", "used"),
    (f"sum(engram_index_ram_budget_bytes{{{APPVIEW}}})", "budget")], unit="bytes")
timeseries("Disk cache", [
    (f"sum(engram_disk_cache_bytes{{{APPVIEW}}})", "used"),
    (f"sum(engram_disk_cache_budget_bytes{{{APPVIEW}}})", "budget")], unit="bytes")
timeseries("Segment reads served from disk cache", [
    (f'sum(rate(engram_segment_reads_total{{{APPVIEW}, source="disk_cache"}}[$__rate_interval])) / sum(rate(engram_segment_reads_total{{{APPVIEW}}}[$__rate_interval]))',
     "hit ratio")], unit="percentunit")
timeseries("Object storage calls", [
    (f"sum by (op, result) (rate(engram_blob_requests_total{{{APPVIEW}}}[$__rate_interval]))", "{{op}} {{result}}")],
    unit="ops")
timeseries("Object storage time p95", [
    (q(0.95, "engram_blob_request_duration_seconds", APPVIEW, by="op"), "{{op}}")], unit="s")
timeseries("Object storage bytes", [
    (f"sum by (direction) (rate(engram_blob_bytes_total{{{APPVIEW}}}[$__rate_interval]))", "{{direction}}")], unit="Bps")
timeseries("Flushes, merges, garbage collection", [
    (f"sum by (result) (increase(engram_flushes_total{{{APPVIEW}}}[$__rate_interval]))", "flush {{result}}"),
    (f"sum by (kind, result) (increase(engram_maintenance_total{{{APPVIEW}}}[$__rate_interval]))", "{{kind}} {{result}}")])
timeseries("Flush time p95", [(q(0.95, "engram_flush_duration_seconds", APPVIEW), "p95")], unit="s")
timeseries("Space loads and evictions", [
    (f"sum by (result) (increase(engram_space_loads_total{{{APPVIEW}}}[$__rate_interval]))", "load {{result}}"),
    (f"sum(increase(engram_space_evictions_total{{{APPVIEW}}}[$__rate_interval]))", "evicted")])
timeseries("Buffered changes and segments", [
    (f"sum(engram_buffered_memories{{{APPVIEW}}})", "buffered changes"),
    (f"sum(engram_segments{{{APPVIEW}}})", "segments")])

# ---- Web app ----
row("Web app")
timeseries("Web requests by route", [
    (f'sum by (route) (rate(engram_http_requests_total{{{WEB}, route!="GET /api/live"}}[$__rate_interval]))', "{{route}}")],
    unit="reqps", w=12, desc="The live view's long-lived stream (GET /api/live) is left out.")
timeseries("Web latency p95 by route", [
    (q(0.95, "engram_http_request_duration_seconds", f'{WEB}, route!="GET /api/live"', by="route"), "{{route}}")],
    unit="s", w=12)
timeseries("Sign-ins", [
    (f"sum by (result) (increase(engram_web_signins_total{{{WEB}}}[$__rate_interval]))", "{{result}}")])
timeseries("Open live views", [
    (f'sum(engram_http_requests_in_flight{{{WEB}}})', "requests in flight (mostly live views)")])

# ---- Process ----
row("Process")
timeseries("CPU", [(f"sum by (job) (rate(process_cpu_seconds_total{{{ALL}}}[$__rate_interval]))", "{{job}}")],
           unit="percentunit")
timeseries("Resident memory", [(f"sum by (job) (process_resident_memory_bytes{{{ALL}}})", "{{job}}")], unit="bytes")
timeseries("Goroutines", [(f"sum by (job) (go_goroutines{{{ALL}}})", "{{job}}")])
timeseries("Go heap in use", [(f'sum by (job) (go_memstats_heap_inuse_bytes{{{ALL}}})', "{{job}}")], unit="bytes")
# Prometheus 3 stores the summary's quantile="1" as "1.0".
timeseries("Longest recent GC pause", [
    (f'max by (job) (go_gc_duration_seconds{{{ALL}, quantile=~"1|1.0"}})', "{{job}}")], unit="s")
timeseries("Open file descriptors", [(f"sum by (job) (process_open_fds{{{ALL}}})", "{{job}}")])

# ---- Logs ----
row("Logs")
panels.append({"type": "timeseries", "title": "Log lines by level", "id": _next_id(), "datasource": LOKI,
               "gridPos": _place(24, 7),
               "targets": [{"refId": "A", "datasource": LOKI,
                            "expr": 'sum by (level) (count_over_time($log_selector | json | __error__="" [$__auto]))',
                            "legendFormat": "{{level}}"}],
               "fieldConfig": {"defaults": {"unit": "short", "custom": {"drawStyle": "bars", "fillOpacity": 60,
                                                                       "stacking": {"mode": "normal", "group": "A"}}},
                               "overrides": []},
               "options": {"legend": {"displayMode": "list", "placement": "bottom"}, "tooltip": {"mode": "multi"}}})
panels.append({"type": "logs", "title": "Warnings and errors", "id": _next_id(), "datasource": LOKI,
               "gridPos": _place(24, 10),
               "targets": [{"refId": "A", "datasource": LOKI,
                            "expr": '$log_selector | json | level=~"WARN|ERROR"'}],
               "options": {"showTime": True, "wrapLogMessage": True, "sortOrder": "Descending", "enableLogDetails": True}})
panels.append({"type": "logs", "title": "All logs (filter with the search box above)", "id": _next_id(),
               "datasource": LOKI, "gridPos": _place(24, 12),
               "targets": [{"refId": "A", "datasource": LOKI, "expr": '$log_selector |= "$search"'}],
               "options": {"showTime": True, "wrapLogMessage": True, "sortOrder": "Descending", "enableLogDetails": True}})


def query_var(name, label, query, include_all=True, regex=""):
    return {"name": name, "label": label, "type": "query", "datasource": PROM, "query": {"query": query, "refId": name},
            "definition": query, "refresh": 2, "includeAll": include_all, "multi": include_all, "allValue": ".*",
            "current": {"selected": True, "text": "All", "value": "$__all"} if include_all else {}, "regex": regex,
            "sort": 1}


dashboard = {
    "uid": "engram-garden",
    "title": "Engram Garden",
    "description": "Appview and web app: freshness, indexing, search, storage, process, logs. Generated by deploy/monitoring/grafana/generate.py.",
    "tags": ["engram"],
    "timezone": "browser",
    "schemaVersion": 39,
    "version": 1,
    "refresh": "30s",
    "time": {"from": "now-6h", "to": "now"},
    "graphTooltip": 1,
    "templating": {"list": [
        {"name": "prometheus", "label": "Prometheus", "type": "datasource", "query": "prometheus", "refresh": 1},
        {"name": "loki", "label": "Loki", "type": "datasource", "query": "loki", "refresh": 1},
        query_var("appview_job", "Appview job", "label_values(engram_spaces_indexed, job)", include_all=False),
        query_var("web_job", "Web job", "label_values(engram_web_signins_total, job)", include_all=False),
        query_var("instance", "Instance", "label_values(engram_build_info, instance)"),
        {"name": "log_selector", "label": "Log stream selector", "type": "textbox",
         "query": '{compose_project="engram-garden"}',
         "current": {"text": '{compose_project="engram-garden"}', "value": '{compose_project="engram-garden"}'},
         "description": "LogQL stream selector for Engram's logs; match it to how your log shipper labels them."},
        {"name": "search", "label": "Log search", "type": "textbox", "query": "", "current": {"text": "", "value": ""}},
    ]},
    "annotations": {"list": []},
    "panels": panels,
}

(HERE / "engram.json").write_text(json.dumps(dashboard, indent=2) + "\n")
print(f"wrote {HERE / 'engram.json'}: {len(panels)} panels")
