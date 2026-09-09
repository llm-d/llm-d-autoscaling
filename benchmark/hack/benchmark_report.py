#!/usr/bin/env python3
"""
Provision a local Grafana instance for benchmark observability, snapshot the
relevant dashboard for a session before Prometheus expires its data, render a
standalone HTML summary report per session, and serve an interactive dashboard
across every session (finished or in-progress) under a workspace.

Grafana is expected to be installed locally (see docs/benchmark-report.md's
"One-time setup"). Prometheus is always reached remotely, through OpenShift's
Thanos Querier Route -- auto-discovered against the current kube context, no
port-forwarding needed for it. Pass --prometheus-url yourself to skip
discovery (e.g. for a plain, non-OpenShift Prometheus you still want to
port-forward by hand).

A "session" is one llmdbenchmark workspace directory -- a single lifecycle
instance that progresses through the plan -> standup -> smoketest -> run ->
teardown phases. Only the `run` phase produces benchmark data; a session that
only stood infrastructure up is a valid session with no run results.

Usage:
    # One-time (or whenever unsure Grafana is set up correctly):
    python3 benchmark/hack/benchmark_report.py configure

    # After a session's benchmark run finishes (while Prometheus still has the data):
    python3 benchmark/hack/benchmark_report.py all <session_dir>

    # Individual steps:
    python3 benchmark/hack/benchmark_report.py snapshot <experiment_dir>
    python3 benchmark/hack/benchmark_report.py report <session_dir>

    # Interactive dashboard across every session under a workspace, including
    # sessions that are still standing up / running:
    python3 benchmark/hack/benchmark_report.py serve --workspace benchmark/results

<session_dir> is the top-level llmdbenchmark workspace directory, e.g.
`benchmark/results/<user>-<timestamp>/` (the --workspace passed to
standup/run/teardown). <experiment_dir> is one experiment under its
`results/` subdirectory, e.g. `<session_dir>/results/inference-perf-*_1`.
"""

import argparse
import base64
import datetime
import glob
import http.server
import json
import mimetypes
import os
import re
import shutil
import signal
import ssl
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

import yaml

DASHBOARD_JSON_PATH = os.path.join(
    os.path.dirname(os.path.abspath(__file__)), "..", "config", "grafana", "dashboard.json"
)
DASHBOARD_UID = "llm-d-autoscaling-benchmark-run"
# Remembers the last Service the operator picked for Grafana's port-forward, per kube
# context, so `serve` can reconnect it automatically on the next startup instead of
# requiring the picker to be used again every time -- see _load_port_forward_state below.
# Local to this machine's checkout, not versioned or shared across workspaces.
PORT_FORWARD_STATE_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), ".port-forward-state.json")
DEFAULT_GRAFANA_URL = "http://localhost:3000"
DEFAULT_GRAFANA_USER = "admin"
DEFAULT_GRAFANA_PASSWORD = "admin"
SNAPSHOT_WINDOW_BUFFER_SECONDS = 60

# Hosts the dashboard treats as "this machine", for the local/remote badge on
# Grafana's system-status chip and for deciding whether native port-forwarding
# (which shells out to a local `kubectl`) is even applicable. Prometheus is always remote
# (reached via OpenShift's Thanos Querier), so this no longer applies to it.
_LOCAL_HOSTS = {"localhost", "127.0.0.1", "::1", "0.0.0.0"}

# Service names ranked first in Grafana's port-forward picker regardless of which namespace
# they turn up in -- benchmark/docs/benchmark-report.md documents these as the ones that just
# work: a plain HTTP endpoint straight onto Grafana, no auth in front.
_PREFERRED_GRAFANA_NAMES = ("kube-prometheus-stack-grafana", "grafana")


def _http(method, url, user=None, password=None, json_body=None, params=None):
    """Issue an HTTP request, returning (status_code, decoded_json_or_None)."""
    if params:
        url = f"{url}?{urllib.parse.urlencode(params)}"
    data = json.dumps(json_body).encode() if json_body is not None else None
    headers = {"Content-Type": "application/json", "Accept": "application/json"}
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    if user is not None:
        creds = base64.b64encode(f"{user}:{password}".encode()).decode()
        req.add_header("Authorization", f"Basic {creds}")
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            body = resp.read()
            return resp.status, (json.loads(body) if body else None)
    except urllib.error.HTTPError as e:
        body = e.read()
        try:
            return e.code, json.loads(body)
        except (ValueError, UnicodeDecodeError):
            return e.code, {"error": body.decode(errors="replace")}
    except urllib.error.URLError as e:
        raise ConnectionError(f"cannot reach {url}: {e.reason}") from e


def _url_location(url):
    """'local' if url's host is this machine, else 'remote' ('unknown' if url is empty)."""
    if not url:
        return "unknown"
    return "local" if urllib.parse.urlparse(url).hostname in _LOCAL_HOSTS else "remote"


def _url_reachable(url, timeout=5, bearer_token=None, tls_skip_verify=False):
    req = urllib.request.Request(url)
    if bearer_token:
        req.add_header("Authorization", f"Bearer {bearer_token}")
    context = ssl._create_unverified_context() if tls_skip_verify else None
    try:
        with urllib.request.urlopen(req, timeout=timeout, context=context) as resp:
            return 200 <= resp.status < 300
    except (urllib.error.URLError, OSError):
        return False


def _load_yaml(path):
    if not os.path.isfile(path):
        return {}
    with open(path) as f:
        return yaml.safe_load(f) or {}


def _load_json(path):
    if not os.path.isfile(path):
        return None
    with open(path) as f:
        return json.load(f)


def _load_port_forward_state():
    """{"prometheus": {"<kube-context>": {"namespace", "service", "port"}}, "grafana": {...}}
    -- corrupt or missing state is just "nothing remembered yet", not an error."""
    try:
        with open(PORT_FORWARD_STATE_PATH) as f:
            return json.load(f)
    except (OSError, ValueError):
        return {}


def _remember_port_forward(target, context, namespace, service, port):
    state = _load_port_forward_state()
    state.setdefault(target, {})[context or ""] = {"namespace": namespace, "service": service, "port": port}
    with open(PORT_FORWARD_STATE_PATH, "w") as f:
        json.dump(state, f, indent=2)


def _forget_port_forward(target, context):
    state = _load_port_forward_state()
    if state.get(target, {}).pop(context or "", None) is not None:
        with open(PORT_FORWARD_STATE_PATH, "w") as f:
            json.dump(state, f, indent=2)


# --------------------------------------------------------------------------
# configure
# --------------------------------------------------------------------------

# The only variables _capture_snapshot substitutes before replaying a panel's
# expression against Prometheus. Anything else (another dashboard variable, a
# Grafana macro such as $__rate_interval) reaches Prometheus unexpanded, and
# the panel freezes empty in the snapshot -- see "Extending the dashboard" in
# docs/benchmark-report.md. `$1`-style label_replace capture groups are fine:
# Grafana leaves them alone and so do we.
SNAPSHOT_SAFE_VARIABLES = ("namespace",)


def _dashboard_expression_errors(dashboard):
    """Return a list of human-readable reasons the dashboard's panel queries wouldn't
    survive snapshot capture, empty if they all would."""
    errors = []
    for panel in dashboard.get("panels", []):
        for target in panel.get("targets", []) or []:
            expr = target.get("expr", "")
            for variable in re.findall(r"\$(\w+)", expr):
                if variable.isdigit() or variable in SNAPSHOT_SAFE_VARIABLES:
                    continue
                errors.append(
                    f"panel {panel.get('title')!r} target {target.get('refId')}: "
                    f"${variable} is not substituted during snapshot capture "
                    f"(only {', '.join('$' + v for v in SNAPSHOT_SAFE_VARIABLES)})"
                )
    return errors


def cmd_configure(args):
    err = resolve_remote_prometheus(args, context=getattr(args, "context", None))
    if err:
        print(f"ERROR: {err}", file=sys.stderr)
        return 1

    grafana_url = args.grafana_url.rstrip("/")
    user, password = args.grafana_user, args.grafana_password

    try:
        status, health = _http("GET", f"{grafana_url}/api/health")
    except ConnectionError as e:
        print(f"ERROR: {e}", file=sys.stderr)
        return 1
    if status != 200:
        print(f"ERROR: Grafana health check failed ({status}): {health}", file=sys.stderr)
        return 1
    print(f"Grafana reachable at {grafana_url} (version {health.get('version', '?')})")

    # Auth Grafana -> Prometheus, not this script -> Grafana (that's user/password above).
    # Needed to reach a token-protected endpoint directly (e.g. OpenShift's Thanos Querier,
    # which merges the platform and user-workload Prometheus instances -- see "Identifying a
    # session's metrics" in docs/benchmark-report.md for why both are required) without a
    # `kubectl port-forward` in front of it.
    json_data = {}
    secure_json_data = {}
    if args.prometheus_tls_skip_verify:
        json_data["tlsSkipVerify"] = True
    if args.prometheus_bearer_token:
        json_data["httpHeaderName1"] = "Authorization"
        secure_json_data["httpHeaderValue1"] = f"Bearer {args.prometheus_bearer_token}"

    ds_payload = {
        "name": "Prometheus",
        "type": "prometheus",
        "access": "proxy",
        "url": args.prometheus_url,
        "isDefault": True,
        "jsonData": json_data,
        "secureJsonData": secure_json_data,
    }
    status, existing = _http(
        "GET", f"{grafana_url}/api/datasources/name/Prometheus", user=user, password=password
    )
    if status == 200:
        # Grafana never echoes secureJsonData back, so a supplied bearer token can't be
        # compared for equality -- always re-PUT when one is given to keep it current.
        unchanged = (
            existing.get("url") == args.prometheus_url
            and existing.get("jsonData", {}).get("tlsSkipVerify", False) == bool(args.prometheus_tls_skip_verify)
            and not args.prometheus_bearer_token
        )
        if unchanged:
            print("Prometheus datasource already configured correctly")
        else:
            status, result = _http(
                "PUT",
                # The numeric-id form (/api/datasources/{id}) is gone in modern Grafana
                # (404s as of 13.x) -- only the uid-based endpoint still works.
                f"{grafana_url}/api/datasources/uid/{existing['uid']}",
                user=user,
                password=password,
                json_body=ds_payload,
            )
            if status not in (200, 202):
                print(f"ERROR: failed to update Prometheus datasource: {result}", file=sys.stderr)
                return 1
            print(f"Updated Prometheus datasource -> {args.prometheus_url}")
    else:
        status, result = _http(
            "POST", f"{grafana_url}/api/datasources", user=user, password=password, json_body=ds_payload
        )
        if status not in (200, 201):
            print(f"ERROR: failed to create Prometheus datasource: {result}", file=sys.stderr)
            return 1
        print(f"Created Prometheus datasource pointing at {args.prometheus_url}")

    with open(DASHBOARD_JSON_PATH) as f:
        dashboard = json.load(f)
    expr_errors = _dashboard_expression_errors(dashboard)
    if expr_errors:
        print(f"ERROR: {DASHBOARD_JSON_PATH} has queries that snapshots can't replay:", file=sys.stderr)
        for e in expr_errors:
            print(f"  - {e}", file=sys.stderr)
        return 1
    dashboard["id"] = None
    status, result = _http(
        "POST",
        f"{grafana_url}/api/dashboards/db",
        user=user,
        password=password,
        json_body={"dashboard": dashboard, "overwrite": True, "folderId": 0},
    )
    if status != 200:
        print(f"ERROR: failed to provision dashboard: {result}", file=sys.stderr)
        return 1
    print(f"Dashboard provisioned: {grafana_url}{result.get('url', '')}")
    return 0


# --------------------------------------------------------------------------
# snapshot
# --------------------------------------------------------------------------

def _run_window_epochs(meta):
    """Return (namespace, from_epoch_s, to_epoch_s) for a run_metadata dict, buffered by
    SNAPSHOT_WINDOW_BUFFER_SECONDS on each side, or None if the run window isn't recorded yet."""
    if not meta or not meta.get("harness_start") or not meta.get("harness_stop"):
        return None
    start = yaml.safe_load(f'x: {meta["harness_start"]}')["x"]
    stop = yaml.safe_load(f'x: {meta["harness_stop"]}')["x"]
    if not isinstance(start, datetime.datetime) or not isinstance(stop, datetime.datetime):
        return None
    start_s = start.timestamp() - SNAPSHOT_WINDOW_BUFFER_SECONDS
    stop_s = stop.timestamp() + SNAPSHOT_WINDOW_BUFFER_SECONDS
    return meta.get("namespace", ".*"), start_s, stop_s


def _run_window(experiment_dir):
    """Return (namespace, from_epoch_s, to_epoch_s, experiment_id) for an experiment's run window."""
    meta = _load_yaml(os.path.join(experiment_dir, "run_metadata.yaml"))
    window = _run_window_epochs(meta)
    if window is None:
        raise ValueError(f"no harness_start/harness_stop in {experiment_dir}/run_metadata.yaml")
    namespace, start_s, stop_s = window
    return namespace, start_s, stop_s, meta.get("experiment_id", os.path.basename(experiment_dir))


def _live_dashboard_url(grafana_url, dashboard_uid, namespace, start_s, stop_s):
    """Build a Grafana dashboard URL time-boxed to a run window and scoped to its namespace,
    so the dashboard shows only the data for that run. Namespace + time window is the whole
    of a session's identity here -- the pods carry no session-id label to filter on; see
    "Identifying a session's metrics" in docs/benchmark-report.md."""
    return (
        f"{grafana_url.rstrip('/')}/d/{dashboard_uid}"
        f"?orgId=1&from={int(start_s * 1000)}&to={int(stop_s * 1000)}"
        f"&var-namespace={urllib.parse.quote(namespace)}"
    )


def _session_id_from_experiment_dir(experiment_dir):
    """`<workspace>/<session_id>/results/<experiment_id>` -> `<session_id>`."""
    return Path(experiment_dir).resolve().parent.parent.name


# Grafana's App Platform (k8s-style) API namespaces dashboards per org; the
# default org (id 1, the only org in a fresh local instance) is "default".
GRAFANA_K8S_NAMESPACE = "default"


def _prometheus_datasource_uid(grafana_url, user, password):
    status, ds = _http(
        "GET", f"{grafana_url}/api/datasources/name/Prometheus", user=user, password=password
    )
    if status != 200:
        raise RuntimeError(f"Prometheus datasource not found ({status}): {ds}")
    return ds["uid"]


def _query_range(grafana_url, ds_uid, user, password, expr, namespace, start_s, stop_s, step):
    expr = expr.replace("$namespace", namespace)
    status, result = _http(
        "GET",
        f"{grafana_url}/api/datasources/proxy/uid/{ds_uid}/api/v1/query_range",
        user=user,
        password=password,
        params={"query": expr, "start": start_s, "end": stop_s, "step": step},
    )
    if status != 200 or result.get("status") != "success":
        print(f"WARNING: query_range failed for {expr!r}: {result}", file=sys.stderr)
        return []
    return result["data"]["result"]


def _interpolate_legend(legend_format, labels):
    if not legend_format:
        return labels.get("__name__", "value")
    return re.sub(r"\{\{\s*(\w+)\s*\}\}", lambda m: labels.get(m.group(1), ""), legend_format)


def _iso(epoch_s):
    return (
        datetime.datetime.fromtimestamp(epoch_s, datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3]
        + "Z"
    )


def _prometheus_series_to_frame(refid, expr, step_s, labels, times_s, values):
    """Build a Grafana DataFrameJSON frame (the shape /api/ds/query and v2
    dashboard snapshots use) for one Prometheus series."""
    value_field_name = labels.get("__name__", "Value")
    legend = _interpolate_legend(labels.get("__legend__", ""), labels) or value_field_name
    return {
        "schema": {
            "refId": refid,
            "fields": [
                {
                    "name": "Time",
                    "type": "time",
                    "typeInfo": {"frame": "time.Time"},
                    "config": {"interval": step_s * 1000},
                },
                {
                    "name": value_field_name,
                    "type": "number",
                    "typeInfo": {"frame": "float64"},
                    "labels": {k: v for k, v in labels.items() if k != "__legend__"},
                    "config": {"displayNameFromDS": legend},
                },
            ],
            "meta": {
                "custom": {"calculatedMinStep": step_s * 1000, "resultType": "matrix"},
                "executedQueryString": f"Expr: {expr}\nStep: {step_s}s",
                "preferredVisualisationType": "graph",
                "type": "timeseries-multi",
                "typeVersion": [0, 1],
            },
        },
        "data": {"values": [times_s, values]},
    }


def _capture_snapshot(experiment_dir, grafana_url, user, password, dashboard_uid):
    """Freeze the benchmark dashboard's panels for one experiment's run window into a
    Grafana snapshot, write grafana_snapshot.yaml next to the results, and return the
    written metadata dict. Raises ConnectionError/RuntimeError/ValueError on failure."""
    grafana_url = grafana_url.rstrip("/")
    namespace, start_s, stop_s, experiment_id = _run_window(experiment_dir)
    session_id = _session_id_from_experiment_dir(experiment_dir)
    ds_uid = _prometheus_datasource_uid(grafana_url, user, password)
    status, dashboard_resp = _http(
        "GET",
        f"{grafana_url}/apis/dashboard.grafana.app/v2/namespaces/{GRAFANA_K8S_NAMESPACE}"
        f"/dashboards/{dashboard_uid}",
        user=user,
        password=password,
    )
    if status != 200:
        raise RuntimeError(f"dashboard {dashboard_uid} not found ({status}): {dashboard_resp}")

    # Grafana's dashboard snapshots embed frozen query results using the v2
    # ("Scenes") dashboard schema (elements/layout), not the legacy
    # panels/gridPos schema returned by /api/dashboards/uid/<uid> -- the
    # legacy shape is silently accepted by POST /api/snapshots but the
    # modern snapshot viewer fails to load it ("Snapshot not found").
    spec = dashboard_resp["spec"]
    step_s = max(15, int((stop_s - start_s) / 500))
    for element in spec.get("elements", {}).values():
        for query in element.get("spec", {}).get("data", {}).get("spec", {}).get("queries", []):
            query_spec = query["spec"]
            refid = query_spec["refId"]
            orig = query_spec["query"]["spec"]
            expr = orig["expr"].replace("$namespace", namespace)
            legend_format = orig.get("legendFormat", "")

            series = _query_range(grafana_url, ds_uid, user, password, expr, namespace, start_s, stop_s, step_s)
            frames = []
            for s in series:
                labels = dict(s.get("metric", {}), __legend__=legend_format)
                times = [int(float(t) * 1000) for t, _ in s["values"]]
                values = [float(v) for _, v in s["values"]]
                frames.append(_prometheus_series_to_frame(refid, expr, step_s, labels, times, values))

            query_spec["query"] = {
                "datasource": {"name": "grafana"},
                "group": "grafana",
                "kind": "DataQuery",
                "spec": {"queryType": "snapshot", "snapshot": frames},
            }

    spec["timeSettings"]["from"] = _iso(start_s)
    spec["timeSettings"]["to"] = _iso(stop_s)

    dashboard = dict(spec)
    dashboard["uid"] = dashboard_uid
    dashboard["id"] = None
    dashboard["schemaVersion"] = None
    dashboard["version"] = None
    dashboard["snapshot"] = {"originalUrl": f"/d/{dashboard_uid}"}

    status, snap_result = _http(
        "POST",
        f"{grafana_url}/api/snapshots",
        user=user,
        password=password,
        json_body={"dashboard": dashboard, "name": experiment_id, "expires": 0},
    )
    if status != 200:
        raise RuntimeError(f"failed to create snapshot: {snap_result}")

    out = {
        "experiment_id": experiment_id,
        "namespace": namespace,
        "session_id": session_id,
        "captured_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "snapshot_url": snap_result.get("url"),
        "snapshot_delete_url": snap_result.get("deleteUrl"),
        "live_dashboard_url": _live_dashboard_url(grafana_url, dashboard_uid, namespace, start_s, stop_s),
    }
    out_path = os.path.join(experiment_dir, "grafana_snapshot.yaml")
    with open(out_path, "w") as f:
        yaml.safe_dump(out, f, sort_keys=False)
    return out


def cmd_snapshot(args):
    try:
        out = _capture_snapshot(
            args.experiment_dir, args.grafana_url, args.grafana_user, args.grafana_password, args.dashboard_uid
        )
    except (ConnectionError, RuntimeError, ValueError) as e:
        print(f"ERROR: {e}", file=sys.stderr)
        return 1
    print(f"Snapshot captured: {out['snapshot_url']}")
    print(f"Wrote {os.path.join(args.experiment_dir, 'grafana_snapshot.yaml')}")
    return 0


# --------------------------------------------------------------------------
# report
# --------------------------------------------------------------------------

def _flatten(value, prefix=""):
    """Yield (dotted.path, value) for scalar leaves of a nested dict/list."""
    if isinstance(value, dict):
        for k, v in value.items():
            yield from _flatten(v, f"{prefix}.{k}" if prefix else str(k))
    elif isinstance(value, list):
        if value and all(isinstance(v, (int, float, str, type(None))) for v in value):
            yield prefix, ", ".join(str(v) for v in value)
        elif value:
            yield prefix, f"[{len(value)} items]"
    else:
        yield prefix, value


def _html_escape(s):
    return (
        str(s)
        .replace("&", "&amp;")
        .replace("<", "&lt;")
        .replace(">", "&gt;")
        .replace('"', "&quot;")
    )


def _kpi_table_html(rows):
    body = "\n".join(
        f"<tr><td>{_html_escape(k)}</td><td>{_html_escape(v)}</td></tr>" for k, v in rows
    )
    return f"<table class='kpi'><tbody>{body}</tbody></table>"


def _render_experiment(experiment_dir):
    meta = _load_yaml(os.path.join(experiment_dir, "run_metadata.yaml"))
    traffic = _load_yaml(os.path.join(experiment_dir, "traffic_complete.yaml"))
    config_raw = ""
    config_path = os.path.join(experiment_dir, "config.yaml")
    if os.path.isfile(config_path):
        with open(config_path) as f:
            config_raw = f.read()

    metrics = _load_json(os.path.join(experiment_dir, "summary_lifecycle_metrics.json")) or _load_json(
        os.path.join(experiment_dir, "stage_0_lifecycle_metrics.json")
    )

    experiment_id = meta.get("experiment_id", os.path.basename(experiment_dir))
    meta_rows = [
        ("experiment_id", experiment_id),
        ("harness", meta.get("harness_name")),
        ("workload", meta.get("harness_workload")),
        ("model", meta.get("model")),
        ("namespace", meta.get("namespace")),
        ("start", meta.get("harness_start")),
        ("stop", meta.get("harness_stop")),
        ("duration", meta.get("harness_delta")),
        ("exit_code", meta.get("harness_rc")),
        ("traffic_completed_at", traffic.get("completed_at")),
    ]
    meta_rows = [(k, v) for k, v in meta_rows if v is not None]

    metric_rows = list(_flatten(metrics)) if metrics else []

    snapshot = _load_yaml(os.path.join(experiment_dir, "grafana_snapshot.yaml"))
    window = _run_window_epochs(meta)
    live_url = snapshot.get("live_dashboard_url") if snapshot else (
        _live_dashboard_url(DEFAULT_GRAFANA_URL, DASHBOARD_UID, *window) if window else None
    )
    live_html = (
        "<p><strong>Live dashboard</strong> (time-boxed to this run &amp; its namespace, so it shows "
        "only this run's data; requires Grafana at localhost:3000 with Prometheus still holding the data): "
        f"<a href='{_html_escape(live_url)}'>open</a></p>"
        if live_url else ""
    )
    if snapshot:
        grafana_html = (
            "<p><strong>Grafana snapshot</strong> (frozen copy, survives Prometheus retention): "
            f"<a href='{_html_escape(snapshot['snapshot_url'])}'>{_html_escape(snapshot['snapshot_url'])}</a>"
            f" &mdash; captured {_html_escape(snapshot.get('captured_at', '?'))}</p>"
            f"{live_html}"
        )
    else:
        grafana_html = (
            f"{live_html}"
            "<p class='muted'>No Grafana snapshot captured for this run. "
            f"Run <code>python3 benchmark/hack/benchmark_report.py snapshot {_html_escape(experiment_dir)}</code> "
            "while Prometheus still has the data.</p>"
        )

    return f"""
<section class="experiment">
  <h2>{_html_escape(experiment_id)}</h2>
  <h3>Run</h3>
  {_kpi_table_html(meta_rows)}
  <h3>Metrics</h3>
  {_kpi_table_html(metric_rows) if metric_rows else "<p class='muted'>No lifecycle metrics found.</p>"}
  <h3>Grafana</h3>
  {grafana_html}
  <details>
    <summary>Workload config</summary>
    <pre>{_html_escape(config_raw)}</pre>
  </details>
</section>
"""


_HTML_STYLE = """
body { font-family: -apple-system, Helvetica, Arial, sans-serif; margin: 2rem; color: #1a1a1a; }
h1 { border-bottom: 2px solid #ddd; padding-bottom: .5rem; }
section.experiment { margin-bottom: 3rem; padding: 1rem 1.5rem; border: 1px solid #ddd; border-radius: 8px; }
table.kpi { border-collapse: collapse; margin: .5rem 0 1rem; }
table.kpi td { padding: .25rem .75rem; border-bottom: 1px solid #eee; }
table.kpi td:first-child { font-weight: 600; color: #444; }
.muted { color: #888; }
pre { background: #f6f6f6; padding: 1rem; overflow-x: auto; font-size: .85em; }
code { background: #f0f0f0; padding: .1em .3em; border-radius: 3px; }
"""


def cmd_report(args):
    experiment_dirs = sorted(
        d
        for d in glob.glob(os.path.join(args.session_dir, "results", "*"))
        if os.path.isfile(os.path.join(d, "run_metadata.yaml"))
    )
    if not experiment_dirs:
        print(f"ERROR: no experiments with run_metadata.yaml found under {args.session_dir}/results", file=sys.stderr)
        return 1

    sections = "\n".join(_render_experiment(d) for d in experiment_dirs)
    html = f"""<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Benchmark report: {_html_escape(os.path.basename(os.path.normpath(args.session_dir)))}</title>
<style>{_HTML_STYLE}</style>
</head>
<body>
<h1>Benchmark report: {_html_escape(os.path.basename(os.path.normpath(args.session_dir)))}</h1>
{sections}
</body>
</html>
"""
    out_path = os.path.join(args.session_dir, "report.html")
    with open(out_path, "w") as f:
        f.write(html)
    print(f"Wrote {out_path}")
    return 0


# --------------------------------------------------------------------------
# all
# --------------------------------------------------------------------------

def cmd_all(args):
    if not args.skip_configure:
        if cmd_configure(args) != 0:
            print("WARNING: configure step failed; continuing without it", file=sys.stderr)

    experiment_dirs = sorted(
        d
        for d in glob.glob(os.path.join(args.session_dir, "results", "*"))
        if os.path.isfile(os.path.join(d, "run_metadata.yaml"))
    )
    for d in experiment_dirs:
        snapshot_args = argparse.Namespace(
            experiment_dir=d,
            grafana_url=args.grafana_url,
            grafana_user=args.grafana_user,
            grafana_password=args.grafana_password,
            dashboard_uid=args.dashboard_uid,
        )
        if cmd_snapshot(snapshot_args) != 0:
            print(f"WARNING: snapshot failed for {d}; report will note it as missing", file=sys.stderr)

    return cmd_report(argparse.Namespace(session_dir=args.session_dir))


# --------------------------------------------------------------------------
# dashboard (index + serve)
#
# Scans every session directory under a --workspace (a llmdbenchmark
# `--workspace benchmark/results`) and builds a JSON manifest the
# interactive dashboard (report.html) renders. Unlike `report`, this reads
# no Grafana/Prometheus state -- it only inspects what's already on disk, so
# it works for sessions that are still standing up or running, not just
# finished ones. See docs/interactive-dashboard.md.
# --------------------------------------------------------------------------

# Percentile keys as emitted by the inference-perf harness's
# {stage_N,summary}_lifecycle_metrics.json -> the dashboard's naming (no
# dots, p50 instead of "median", so they're safe JS property names).
_PCT_KEY_MAP = {
    "p0.1": "p0_1", "p1": "p1", "p5": "p5", "p10": "p10", "p25": "p25",
    "median": "p50", "p75": "p75", "p90": "p90", "p95": "p95", "p99": "p99",
    "p99.9": "p99_9",
}
_STALE_SECONDS = 15 * 60  # no log progress for this long while "running" => "stalled"
_LOG_TAIL_LINES = 20


def _failure_categories(failures):
    """Turn the inference-perf harness's failures.by_label map -- {label: {count, messages:
    [{message}, ...]}} -- into a count-sorted list the dashboard renders as a breakdown
    table, each with one representative (first non-empty) sample message."""
    by_label = (failures or {}).get("by_label") or {}
    total = failures.get("count") or sum(v.get("count", 0) for v in by_label.values())
    categories = []
    for label, info in by_label.items():
        count = info.get("count", 0)
        sample = next((m.get("message") for m in info.get("messages") or [] if m.get("message")), "")
        categories.append({
            "label": label,
            "count": count,
            "pct": count / total if total else None,
            "sample_message": sample,
        })
    categories.sort(key=lambda c: c["count"], reverse=True)
    return categories or None


def _normalize_percentiles(d, scale=1.0):
    if not isinstance(d, dict):
        return None
    out = {}
    for k, v in d.items():
        nk = _PCT_KEY_MAP.get(k)
        if nk is not None and isinstance(v, (int, float)):
            out[nk] = v * scale
    return out or None


def _parse_iso8601_duration_seconds(s):
    """Parse a subset of ISO-8601 durations, e.g. 'PT300.12S' or 'PT1H2M3S'."""
    if not s:
        return None
    m = re.match(r"^PT(?:([\d.]+)H)?(?:([\d.]+)M)?(?:([\d.]+)S)?$", str(s))
    if not m or not any(m.groups()):
        return None
    hours, minutes, seconds = (float(g) if g else 0.0 for g in m.groups())
    return hours * 3600 + minutes * 60 + seconds


def _read_text(path):
    try:
        with open(path, encoding="utf-8", errors="replace") as f:
            return f.read()
    except OSError:
        return ""


_LOG_TS_RE = re.compile(r"^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}),\d+")


def _log_timestamps(text):
    """Return (first_ts, last_ts) as naive datetimes parsed from log lines, or (None, None)."""
    first, last = None, None
    for line in text.splitlines():
        m = _LOG_TS_RE.match(line)
        if not m:
            continue
        ts = datetime.datetime.strptime(m.group(1), "%Y-%m-%d %H:%M:%S")
        if first is None:
            first = ts
        last = ts
    return first, last


def _tail_lines(text, n):
    lines = [line for line in text.splitlines() if line.strip()]
    return lines[-n:]


def _scenario_from_spec_path(spec_path):
    """'.../config/specification/guides/pd-disaggregation.yaml.j2' -> ('guides/pd-disaggregation', 'guides')."""
    if not spec_path:
        return None, None
    marker = "specification/"
    idx = spec_path.find(marker)
    tail = spec_path[idx + len(marker):] if idx >= 0 else spec_path
    for suffix in (".yaml.j2", ".yaml"):
        if tail.endswith(suffix):
            tail = tail[: -len(suffix)]
            break
    kind = tail.split("/", 1)[0] if "/" in tail else "unknown"
    return tail, kind


def _scenario_from_scenario_file_path(scenario_path):
    """'.../config/scenarios/guides/pd-disaggregation.yaml' -> ('guides/pd-disaggregation', 'guides').

    config/scenarios/ and config/specification/ mirror each other 1:1 (same
    relative path, `.yaml` vs `.yaml.j2`), so this yields the same id scheme
    as `_scenario_from_spec_path`.
    """
    if not scenario_path:
        return None, None
    marker = "config/scenarios/"
    normalized = scenario_path.replace(os.sep, "/")
    idx = normalized.find(marker)
    if idx < 0:
        return None, None
    tail = normalized[idx + len(marker):]
    if tail.endswith(".yaml"):
        tail = tail[: -len(".yaml")]
    kind = tail.split("/", 1)[0] if "/" in tail else "unknown"
    return tail, kind


def _scenario_from_plan_dir(session_dir):
    """Read plan/<name>.yaml's `scenario_file.path` -- set by the CLI on every
    standup regardless of whether --spec was given as a bare name, a relative
    path, or (as the dashboard's launch form does) an absolute path. More
    robust than grepping the log for the "Specification resolved" message,
    which the CLI only prints when --spec needed resolving in the first place.
    """
    plan_dir = os.path.join(session_dir, "plan")
    if not os.path.isdir(plan_dir):
        return None, None
    for path in sorted(glob.glob(os.path.join(plan_dir, "*.yaml"))):
        scenario_path = (_load_yaml(path).get("scenario_file") or {}).get("path")
        scenario_spec, scenario_kind = _scenario_from_scenario_file_path(scenario_path)
        if scenario_spec:
            return scenario_spec, scenario_kind
    return None, None


def _backend_from_cluster_config_path(path):
    """'.../config/cluster-configs/k8s/inference-sim.yaml' -> 'k8s/inference-sim' -- mirrors
    the relpath-based id `_discover_cluster_configs` assigns, so a session's recorded backend
    matches an entry in `meta.cluster_configs` (needed for the dashboard's quick-relaunch)."""
    if not path:
        return None
    marker = "cluster-configs/"
    normalized = path.replace(os.sep, "/")
    idx = normalized.find(marker)
    tail = normalized[idx + len(marker):] if idx >= 0 else os.path.basename(normalized)
    return tail[: -len(".yaml")] if tail.endswith(".yaml") else tail


def _scan_stacks(session_dir):
    """Read plan/<stack>/config.yaml for each rendered stack -> model/namespace."""
    plan_dir = os.path.join(session_dir, "plan")
    stacks = []
    if not os.path.isdir(plan_dir):
        return stacks
    for entry in sorted(os.listdir(plan_dir)):
        stack_dir = os.path.join(plan_dir, entry)
        config_path = os.path.join(stack_dir, "config.yaml")
        # A stack dir is one holding a rendered config.yaml. Don't key off a
        # sibling plan/<entry>.yaml: that file is named after the *experiment*
        # (e.g. plan/token-aware.yaml), which needn't match the stack dir's
        # name (plan/pd-disaggregation/), and requiring it silently dropped
        # every stack -- leaving the session with no namespace, and its
        # Grafana link scoped to `.*`. A dry-run's nested plan/setup/ has no
        # config.yaml, so it's still excluded.
        if not os.path.isdir(stack_dir) or not os.path.isfile(config_path):
            continue
        cfg = _load_yaml(config_path)
        model = cfg.get("model") or {}
        namespace = cfg.get("namespace") or {}
        stacks.append({
            "name": entry,
            "namespace": namespace.get("name"),
            "model_name": model.get("name"),
            "model_huggingface_id": model.get("huggingfaceId"),
            "model_short_name": model.get("shortName"),
        })
    return stacks


def _scan_experiment(workspace, experiment_dir, grafana_url=DEFAULT_GRAFANA_URL, dashboard_uid=DASHBOARD_UID):
    rel = os.path.relpath(experiment_dir, workspace)
    meta = _load_yaml(os.path.join(experiment_dir, "run_metadata.yaml"))
    harness_name = meta.get("harness_name")
    harness_rc = meta.get("harness_rc")
    status = "pending" if not meta else ("success" if str(harness_rc) == "0" else "failed")

    metrics = None
    metrics_unsupported_reason = None
    if meta:
        if harness_name == "inference-perf":
            metrics_path = None
            for candidate_dir in (experiment_dir, os.path.join(experiment_dir, "analysis")):
                summary = os.path.join(candidate_dir, "summary_lifecycle_metrics.json")
                if os.path.isfile(summary):
                    metrics_path = summary
                    break
                stage_files = sorted(
                    glob.glob(os.path.join(candidate_dir, "stage_*_lifecycle_metrics.json")),
                    key=lambda p: int(re.search(r"stage_(\d+)_", p).group(1)),
                )
                if stage_files:
                    metrics_path = stage_files[-1]
                    break
            raw_metrics = _load_json(metrics_path) if metrics_path else None
            if raw_metrics:
                successes = raw_metrics.get("successes") or {}
                failures = raw_metrics.get("failures") or {}
                latency = successes.get("latency") or {}
                metrics = {
                    "success_count": successes.get("count"),
                    "failure_count": failures.get("count"),
                    "failure_categories": _failure_categories(failures),
                    "throughput_req_per_sec": (successes.get("throughput") or {}).get("requests_per_sec"),
                    "ttft_ms": _normalize_percentiles(latency.get("time_to_first_token"), scale=1000),
                    "itl_ms": _normalize_percentiles(latency.get("inter_token_latency"), scale=1000),
                }
            else:
                metrics_unsupported_reason = "No lifecycle metrics file found yet for this experiment."
        else:
            metrics_unsupported_reason = (
                f"Metrics parsing for harness '{harness_name}' isn't implemented yet -- see raw files below."
            )

    raw_files = []
    if os.path.isdir(experiment_dir):
        for f in sorted(os.listdir(experiment_dir)):
            fpath = os.path.join(experiment_dir, f)
            if os.path.isfile(fpath):
                raw_files.append({"name": f, "path": os.path.join(rel, f), "size": os.path.getsize(fpath)})

    grafana = _load_yaml(os.path.join(experiment_dir, "grafana_snapshot.yaml")) or None

    # Live dashboard link, time-boxed to this run and scoped to its namespace, computed
    # straight from the run window -- available as soon as run_metadata.yaml exists, with
    # no snapshot required (it just needs Grafana + Prometheus still holding the data).
    window = _run_window_epochs(meta)
    grafana_live_url = _live_dashboard_url(grafana_url, dashboard_uid, *window) if window else None

    harness_delta_seconds = _parse_iso8601_duration_seconds(meta.get("harness_delta")) if meta else None

    return {
        "id": meta.get("experiment_id") or os.path.basename(experiment_dir),
        "dir": rel,
        "harness": harness_name,
        "status": status,
        "metadata": meta or None,
        "harness_delta_seconds": harness_delta_seconds,
        "metrics": metrics,
        "metrics_unsupported_reason": metrics_unsupported_reason,
        "grafana": grafana,
        "grafana_live_url": grafana_live_url,
        "raw_files": raw_files,
    }


def _scan_session(workspace, session_id, grafana_url=DEFAULT_GRAFANA_URL, dashboard_uid=DASHBOARD_UID):
    session_dir = os.path.join(workspace, session_id)
    stdout_text = _read_text(os.path.join(session_dir, "logs", "llmdbenchmark-stdout.log"))
    stderr_text = _read_text(os.path.join(session_dir, "logs", "llmdbenchmark-stderr.log"))

    scenario_spec, scenario_kind = _scenario_from_plan_dir(session_dir)
    if not scenario_spec:
        spec_match = re.search(r"Specification resolved: (\S+) to", stdout_text)
        scenario_spec, scenario_kind = _scenario_from_spec_path(spec_match.group(1) if spec_match else None)
    backend_match = re.search(r"Loaded cluster config overrides from (\S+)", stdout_text)
    backend = _backend_from_cluster_config_path(backend_match.group(1) if backend_match else None)

    stacks = _scan_stacks(session_dir)
    primary = stacks[0] if stacks else {}

    results_dir = os.path.join(session_dir, "results")
    experiment_dirs = sorted(
        d for d in glob.glob(os.path.join(results_dir, "*")) if os.path.isdir(d)
    ) if os.path.isdir(results_dir) else []
    experiments = [_scan_experiment(workspace, d, grafana_url, dashboard_uid) for d in experiment_dirs]

    # A session that ran against an already-stood-up stack has no plan/<stack>/
    # of its own, so fall back to the namespace its runs recorded. Namespace is
    # the only thing scoping this session's Grafana/Prometheus links (see
    # "Identifying a session's metrics" in docs/benchmark-report.md), so an
    # unknown one costs the link all of its precision.
    namespace = primary.get("namespace") or next(
        (e["metadata"]["namespace"] for e in experiments if (e.get("metadata") or {}).get("namespace")), None
    )

    combined_text = stdout_text + "\n" + stderr_text
    is_dry_run = "[DRY RUN]" in stdout_text or "would have executed" in stdout_text
    standup_done = "All standup steps complete." in stdout_text
    smoketest_done = "All smoketest steps complete." in stdout_text
    teardown_done = "Teardown complete" in stdout_text
    standup_failed = "Standup failed:" in combined_text
    smoketest_failed = "Smoketest failed:" in combined_text
    run_failed = "Run failed:" in combined_text
    teardown_failed = "Teardown failed:" in combined_text
    run_started = bool(experiments) or os.path.isdir(os.path.join(session_dir, "run")) or "Deployed pod" in stdout_text
    any_success = any(e["status"] == "success" for e in experiments)
    any_failed = any(e["status"] == "failed" for e in experiments)
    any_results = any_success or any_failed

    first_ts, last_ts = _log_timestamps(stdout_text)
    now = datetime.datetime.now()
    is_stale = bool(last_ts) and (now - last_ts).total_seconds() > _STALE_SECONDS

    if teardown_done:
        stage, stage_label = "torn_down", "Torn down"
    elif teardown_failed:
        stage, stage_label = "teardown_failed", "Teardown failed"
    elif any_results:
        if any_success and not any_failed:
            stage, stage_label = "completed", "Completed"
        elif any_success and any_failed:
            stage, stage_label = "completed_partial", "Completed (partial failures)"
        else:
            stage, stage_label = "completed_failed", "Completed (failed)"
    elif run_failed:
        stage, stage_label = "run_failed", "Run failed"
    elif run_started:
        stage, stage_label = ("stalled", "Stalled") if is_stale else ("running", "Running")
    elif smoketest_failed:
        stage, stage_label = "smoketest_failed", "Smoketest failed"
    elif smoketest_done:
        stage, stage_label = "smoketest_passed", "Smoketest passed"
    elif standup_failed:
        stage, stage_label = "standup_failed", "Standup failed"
    elif standup_done:
        stage, stage_label = "stood_up", "Stood up"
    elif os.path.isdir(os.path.join(session_dir, "setup")):
        stage, stage_label = "standing_up", "Standing up"
    else:
        stage, stage_label = "planned", "Planned"

    if is_dry_run and stage != "torn_down":
        stage_label += " (dry-run)"

    # Session-level live dashboard link, scoped to this session's namespace and its whole
    # lifetime so far -- unlike an experiment's link (only available once run_metadata.yaml
    # records a finished harness_start/harness_stop), this works from the moment the first
    # log line lands, including while standup/run is still in progress (end of window is
    # "now" until torn down, then freezes at the last observed log activity).
    session_window_to = last_ts if (stage == "torn_down" and last_ts) else now
    session_grafana_live_url = (
        _live_dashboard_url(
            grafana_url, dashboard_uid, namespace or ".*",
            first_ts.timestamp(), session_window_to.timestamp(),
        )
        if first_ts else None
    )

    return {
        "id": session_id,
        "is_latest": False,
        "scenario_spec": scenario_spec,
        "scenario_kind": scenario_kind,
        "backend": backend,
        "namespace": namespace,
        "model": primary.get("model_name"),
        "stacks": stacks,
        "is_dry_run": is_dry_run,
        "stage": stage,
        "stage_label": stage_label,
        "milestones": {
            "plan": True,
            "standup": standup_done,
            "smoketest": smoketest_done,
            "run": run_started,
            "results": any_results,
            "teardown": teardown_done,
        },
        "has_errors": bool(re.search(r"- ERROR\s", stderr_text)),
        "has_warnings": bool(re.search(r"- WARNING\s", stderr_text)),
        "started_at": first_ts.isoformat() if first_ts else None,
        "last_activity_at": last_ts.isoformat() if last_ts else None,
        "is_stale": is_stale,
        "log_tail": _tail_lines(stdout_text, _LOG_TAIL_LINES),
        "stderr_tail": _tail_lines(stderr_text, _LOG_TAIL_LINES),
        "grafana_live_url": session_grafana_live_url,
        "experiments": experiments,
    }


def build_dashboard_index(workspace, grafana_url=DEFAULT_GRAFANA_URL, dashboard_uid=DASHBOARD_UID):
    """Scan every session under `workspace` and return the dashboard's JSON manifest."""
    latest_target = None
    latest_link = os.path.join(workspace, "latest")
    if os.path.islink(latest_link):
        latest_target = os.path.basename(os.readlink(latest_link).rstrip("/"))

    session_ids = sorted(
        e for e in os.listdir(workspace)
        if os.path.isdir(os.path.join(workspace, e)) and not os.path.islink(os.path.join(workspace, e))
    ) if os.path.isdir(workspace) else []

    sessions = []
    for session_id in session_ids:
        try:
            session = _scan_session(workspace, session_id, grafana_url, dashboard_uid)
        except (OSError, yaml.YAMLError) as e:
            session = {"id": session_id, "error": str(e)}
        session["is_latest"] = session_id == latest_target
        sessions.append(session)
    sessions.sort(key=lambda s: s["id"], reverse=True)

    return {
        "generated_at": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "workspace": workspace,
        "sessions": sessions,
    }


def cmd_index(args):
    manifest = build_dashboard_index(args.workspace)
    out_path = args.out or os.path.join(args.workspace, "index.json")
    with open(out_path, "w") as f:
        json.dump(manifest, f, indent=2)
    print(f"Wrote {out_path} ({len(manifest['sessions'])} session(s))")
    return 0


# --------------------------------------------------------------------------
# launching sessions/phases from the dashboard
#
# Discovers the specs/cluster-configs/harnesses+workloads the `run-benchmark`
# skill would otherwise have a human pick by hand, so the dashboard's
# "Start a session" form can offer the same choices and shell out to the same
# `llmdbenchmark` CLI (from the sibling llm-d-benchmark clone's venv) with
# the same flags. See .claude/skills/run-benchmark/SKILL.md.
# --------------------------------------------------------------------------

_K8S_NAMESPACE_RE = re.compile(r"^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$")


def _discover_specs(benchmark_dir):
    spec_root = os.path.join(benchmark_dir, "config", "specification")
    specs = []
    for path in sorted(glob.glob(os.path.join(spec_root, "**", "*.yaml.j2"), recursive=True)):
        rel = os.path.relpath(path, spec_root)
        spec_id = rel[: -len(".yaml.j2")] if rel.endswith(".yaml.j2") else rel
        spec_id = spec_id.replace(os.sep, "/")
        specs.append({"id": spec_id, "path": path, "kind": spec_id.split("/", 1)[0]})
    return specs


def _discover_cluster_configs(benchmark_dir):
    """Deep search: cluster-configs may be organized into subdirectories (e.g. `k8s/`, `ocp/`
    for platform-specific overlays), so this recurses rather than globbing the top level only.
    `id` is the path relative to cc_root (e.g. 'k8s/inference-sim'), mirroring `_discover_specs`."""
    cc_root = os.path.join(benchmark_dir, "config", "cluster-configs")
    out = []
    for path in sorted(glob.glob(os.path.join(cc_root, "**", "*.yaml"), recursive=True)):
        rel = os.path.relpath(path, cc_root)
        cc_id = rel[: -len(".yaml")] if rel.endswith(".yaml") else rel
        cc_id = cc_id.replace(os.sep, "/")
        out.append({"id": cc_id, "path": path})
    return out


def _discover_harnesses(llmd_benchmark_dir):
    profiles_root = os.path.join(llmd_benchmark_dir, "workload", "profiles")
    if not os.path.isdir(profiles_root):
        return []
    harnesses = []
    for entry in sorted(os.listdir(profiles_root)):
        harness_dir = os.path.join(profiles_root, entry)
        if not os.path.isdir(harness_dir):
            continue
        # Most profiles are `.yaml.in` Jinja templates the CLI renders at run
        # time via `-w <name>.yaml`; a few are already-rendered plain `.yaml`.
        # Either way the `-w` flag takes the name without the `.in` suffix.
        workloads = sorted({
            f[: -len(".in")] if f.endswith(".in") else f
            for f in os.listdir(harness_dir)
            if f.endswith((".yaml", ".yml", ".yaml.in", ".yml.in"))
            and os.path.isfile(os.path.join(harness_dir, f))
        })
        harnesses.append({"id": entry, "workloads": workloads})
    return harnesses


def _current_kube_context():
    for binary in ("kubectl", "oc"):
        try:
            result = subprocess.run(
                [binary, "config", "current-context"], capture_output=True, text=True, timeout=5
            )
        except (OSError, subprocess.TimeoutExpired):
            continue
        if result.returncode == 0:
            return result.stdout.strip()
    return None


def _kubectl_run(args_list, timeout=5, context=None):
    """Try `kubectl <args>` then `oc <args>`; return the first CompletedProcess with rc 0,
    or None if neither binary is on PATH or reachable within timeout. `context`, if given,
    is passed as `--context` so callers aren't affected by `kubectl config use-context`
    elsewhere -- see `build_cluster_status`."""
    ctx_args = ["--context", context] if context else []
    for binary in ("kubectl", "oc"):
        try:
            result = subprocess.run([binary] + args_list + ctx_args, capture_output=True, text=True, timeout=timeout)
        except (OSError, subprocess.TimeoutExpired):
            continue
        if result.returncode == 0:
            return result
    return None


def build_cluster_status(pinned_context):
    """Status of the kube context this server pinned at startup (see cmd_serve) -- its API
    server address and whether the cluster still answers -- for the system-status strip.
    Explicitly targets `pinned_context` (rather than whatever `kubectl config
    current-context` says *right now*) so a `kubectl config use-context` run elsewhere while
    this server is up doesn't silently redirect its cluster/service discovery or
    port-forwards; `context_drifted` flags when that's happened so the operator can restart
    the server to follow it if they want to."""
    if not pinned_context:
        return {"context": None, "server": None, "reachable": False, "location": "unknown", "context_drifted": False}
    server_result = _kubectl_run(
        ["config", "view", "--minify", "-o", "jsonpath={.clusters[0].cluster.server}"], context=pinned_context
    )
    server = server_result.stdout.strip() if server_result else None
    reachable = _kubectl_run(["cluster-info"], context=pinned_context) is not None
    live_context = _current_kube_context()
    return {
        "context": pinned_context,
        "server": server,
        "reachable": reachable,
        "location": _url_location(server),
        "context_drifted": bool(live_context) and live_context != pinned_context,
    }


def _services_by_name(name, context=None):
    """Every Service across all namespaces literally named `name` (there can be more than
    one -- e.g. OpenShift's platform and user-workload monitoring stacks both run a
    `prometheus-operated`), each annotated with its first port."""
    result = _kubectl_run(
        ["get", "svc", "--all-namespaces", "--field-selector", f"metadata.name={name}",
         "-o", "jsonpath={range .items[*]}{.metadata.namespace} {.metadata.name} {.spec.ports[0].port}\n{end}"],
        context=context,
    )
    entries = []
    if result and result.stdout.strip():
        for line in result.stdout.strip().splitlines():
            parts = line.split()
            if len(parts) == 3:
                namespace, svc_name, port = parts
                entries.append({"namespace": namespace, "name": svc_name, "port": int(port)})
    return entries


def _discover_services(label_selector, preferred_names, context=None):
    """Every candidate Service for a label selector across all namespaces, plus any Service
    literally named one of `preferred_names` wherever it lives (in case an older chart
    doesn't set the label), each annotated with its first port -- feeds the dashboard's
    port-forward service picker (GET /api/port-forward/discover) so the operator can choose
    when more than one matches, or when label-based discovery misses the actual one. Ranked
    by Service *name*, not namespace: `preferred_names` sort first (the plain-HTTP endpoints
    benchmark-report.md documents as working out of the box)."""
    found = {}
    result = _kubectl_run(
        ["get", "svc", "--all-namespaces", "-l", label_selector,
         "-o", "jsonpath={range .items[*]}{.metadata.namespace} {.metadata.name} {.spec.ports[0].port}\n{end}"],
        context=context,
    )
    if result and result.stdout.strip():
        for line in result.stdout.strip().splitlines():
            parts = line.split()
            if len(parts) == 3:
                namespace, name, port = parts
                found[(namespace, name)] = {"namespace": namespace, "name": name, "port": int(port)}
    for name in preferred_names:
        for entry in _services_by_name(name, context=context):
            found.setdefault((entry["namespace"], entry["name"]), entry)

    preferred_rank = {name: i for i, name in enumerate(preferred_names)}

    def sort_key(item):
        if item["name"] in preferred_rank:
            return (0, preferred_rank[item["name"]], item["namespace"])
        return (1, 0, item["namespace"], item["name"])

    return sorted(found.values(), key=sort_key)


def discover_grafana_services(context=None):
    return _discover_services("app.kubernetes.io/name=grafana", _PREFERRED_GRAFANA_NAMES, context)


def _route_host(name, namespace, context=None):
    """`spec.host` of an OpenShift Route, or None if it doesn't exist (not OpenShift, or a
    different namespace/name)."""
    result = _kubectl_run(
        ["get", "route", name, "-n", namespace, "-o", "jsonpath={.spec.host}"], context=context
    )
    host = result.stdout.strip() if result else ""
    return host or None


def discover_thanos_querier_url(context=None):
    """https://<host> for OpenShift's Thanos Querier Route in openshift-monitoring -- the
    endpoint that merges the platform and user-workload Prometheus instances (see
    "Identifying a session's metrics" in docs/benchmark-report.md) -- or None if this isn't an
    OpenShift cluster with in-cluster monitoring enabled."""
    host = _route_host("thanos-querier", "openshift-monitoring", context=context)
    return f"https://{host}" if host else None


def discover_cluster_monitoring_view_serviceaccount(namespace, context=None):
    """A ServiceAccount in `namespace` already bound (via ClusterRoleBinding) to the
    cluster-monitoring-view ClusterRole, so a Thanos Querier token can be minted from it
    without creating any new RBAC. Skips bindings pointing at ServiceAccounts that no longer
    exist -- stale ClusterRoleBindings (left behind by removed installs) are common on
    long-lived shared clusters. Returns None if no such (binding, live ServiceAccount) pair
    is found."""
    result = _kubectl_run(["get", "clusterrolebinding", "-o", "json"], context=context, timeout=15)
    if not result:
        return None
    try:
        bindings = json.loads(result.stdout).get("items", [])
    except ValueError:
        return None
    candidates = [
        subject["name"]
        for binding in bindings
        if (binding.get("roleRef") or {}).get("name") == "cluster-monitoring-view"
        for subject in binding.get("subjects") or []
        if subject.get("kind") == "ServiceAccount" and subject.get("namespace") == namespace
    ]
    for name in candidates:
        if _kubectl_run(["get", "serviceaccount", name, "-n", namespace], context=context):
            return name
    return None


def create_service_account_token(namespace, name, duration, context=None):
    result = _kubectl_run(
        ["create", "token", name, "-n", namespace, f"--duration={duration}"], context=context, timeout=15
    )
    return result.stdout.strip() if result else None


def resolve_remote_prometheus(args, context=None):
    """Resolve Prometheus access against the current OpenShift cluster's Thanos Querier --
    the only way this tool reaches Prometheus, since Prometheus is always assumed remote (see
    the module docstring; unlike Prometheus, Grafana stays local). Discovers the Thanos
    Querier Route, mints a bearer token from a ServiceAccount already granted
    cluster-monitoring-view, and turns on TLS skip-verify (Grafana doesn't have the cluster's
    internal CA bundle to validate the Route's cert). No-ops if --prometheus-url was already
    given explicitly (e.g. pointing straight at a known Route + token, or at a manually
    port-forwarded plain Prometheus, skipping discovery). Mutates args.prometheus_url /
    prometheus_bearer_token / prometheus_tls_skip_verify in place. Returns an error message
    on failure, or None on success."""
    if getattr(args, "prometheus_url", None):
        return None

    url = discover_thanos_querier_url(context=context)
    if not url:
        return (
            "No --prometheus-url was given and no thanos-querier Route was found in the "
            "openshift-monitoring namespace. Is this an OpenShift cluster with in-cluster "
            "monitoring enabled? Otherwise pass --prometheus-url yourself (e.g. a "
            "kubectl-port-forwarded plain Prometheus)."
        )

    namespace = args.prometheus_service_account_namespace
    sa_name = discover_cluster_monitoring_view_serviceaccount(namespace, context=context)
    if not sa_name:
        return (
            f"No ServiceAccount in namespace {namespace!r} is bound to the "
            "cluster-monitoring-view ClusterRole, so a Thanos Querier token can't be minted. "
            f"Bind one first, e.g.:\n  oc adm policy add-cluster-role-to-user cluster-monitoring-view "
            f"-z default -n {namespace}"
        )

    token = create_service_account_token(namespace, sa_name, args.prometheus_token_duration, context=context)
    if not token:
        return f"Failed to mint a Thanos Querier token for ServiceAccount {namespace}/{sa_name}."

    print(f"Auto-detected Thanos Querier at {url}")
    print(f"Minted a {args.prometheus_token_duration} token for ServiceAccount {namespace}/{sa_name}")
    args.prometheus_url = url
    args.prometheus_bearer_token = token
    args.prometheus_tls_skip_verify = True
    return None


class ServicePortForward:
    """Manages one native `kubectl port-forward` this server owns, to a Service the operator
    selected from the dashboard's port-forward picker (backed by `_discover_services`) or
    entered manually. Used for both Prometheus and Grafana when either is expected on this
    machine -- see `_url_location` -- e.g. a locally-installed Grafana reaching an in-cluster
    Prometheus (the "One-time setup" section of benchmark-report.md), or reaching an
    in-cluster Grafana directly instead of a local install."""

    def __init__(self, local_port):
        self.local_port = local_port
        self._lock = threading.Lock()
        self._proc = None
        self._namespace = None
        self._service = None
        self._remote_port = None
        self._error = None

    def status(self):
        with self._lock:
            alive = self._proc is not None and self._proc.poll() is None
            return {
                "supported": True,
                "active": alive,
                "namespace": self._namespace,
                "service": self._service,
                "local_port": self.local_port,
                "remote_port": self._remote_port,
                "pid": self._proc.pid if alive else None,
                "error": None if alive else self._error,
            }

    def start(self, namespace, service, remote_port, context=None):
        """Returns (ok, error_message)."""
        with self._lock:
            if self._proc is not None and self._proc.poll() is None:
                return True, None
            self._error = None
            if not namespace or not service or not remote_port:
                self._error = "namespace, service, and port are required."
                return False, self._error
            argv = ["kubectl"] + (["--context", context] if context else [])
            argv += ["port-forward", "-n", namespace, f"svc/{service}", f"{self.local_port}:{remote_port}"]
            try:
                proc = subprocess.Popen(argv, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True, start_new_session=True)
            except OSError as e:
                self._error = f"Failed to launch kubectl: {e}"
                return False, self._error
            time.sleep(1.0)  # let it fail fast (e.g. port already in use) before reporting success
            if proc.poll() is not None:
                self._error = f"kubectl port-forward exited immediately: {proc.stderr.read().strip()}"
                return False, self._error
            self._proc, self._namespace, self._service, self._remote_port = proc, namespace, service, remote_port
            return True, None

    def stop(self):
        with self._lock:
            if self._proc is not None and self._proc.poll() is None:
                self._proc.terminate()
                try:
                    self._proc.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    self._proc.kill()
            self._proc = None


def build_system_status(
    grafana_url, prometheus_url, dashboard_host, kube_context, grafana_port_forward,
    prometheus_bearer_token=None, prometheus_tls_skip_verify=False, grafana_user=None, grafana_password=None,
):
    """Status of every system the dashboard depends on -- current k8s cluster, Prometheus,
    and Grafana -- plus whether Grafana is local to this machine or remote, for the
    dashboard's system-status strip. Prometheus is always remote (no port-forward of its
    own -- see the module docstring). Never raises: an unreachable system is a status, not
    an error."""
    try:
        g_status, g_health = _http("GET", f"{grafana_url.rstrip('/')}/api/health")
        grafana_reachable = g_status == 200
        grafana_version = (g_health or {}).get("version") if grafana_reachable else None
    except ConnectionError:
        grafana_reachable, grafana_version = False, None

    # A bearer-token-protected Prometheus (e.g. OpenShift's thanos-querier) can't be linked to
    # directly from the browser -- there's no Authorization header on a plain <a href>, and
    # thanos-querier's Route is scoped to /api only anyway, so it has no browsable UI at all.
    # report.html uses this to link into Grafana Explore (which already holds the token via
    # the datasource) instead of building a dead link straight to Prometheus.
    requires_auth = bool(prometheus_bearer_token)
    datasource_uid = None
    if requires_auth and grafana_reachable:
        try:
            datasource_uid = _prometheus_datasource_uid(grafana_url, grafana_user, grafana_password)
        except RuntimeError:
            datasource_uid = None

    return {
        "generated_at": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "cluster": build_cluster_status(kube_context),
        "prometheus": {
            "url": prometheus_url,
            # Not /-/healthy: OpenShift's thanos-querier Route only forwards the /api path
            # (`spec.path: /api` -- cluster-monitoring-operator sets this up that way), so
            # anything outside /api 503s at the router regardless of Prometheus's own health.
            # /api/v1/query is served by both plain Prometheus and Thanos Querier alike.
            "reachable": _url_reachable(
                f"{prometheus_url.rstrip('/')}/api/v1/query?query=1",
                bearer_token=prometheus_bearer_token, tls_skip_verify=prometheus_tls_skip_verify,
            ),
            "location": _url_location(prometheus_url),
            "requires_auth": requires_auth,
            "datasource_uid": datasource_uid,
            "port_forward": {"supported": False},
        },
        "grafana": {
            "url": grafana_url,
            "reachable": grafana_reachable,
            "version": grafana_version,
            "location": _url_location(grafana_url),
            "port_forward": grafana_port_forward.status() if grafana_port_forward else {"supported": False},
        },
        "dashboard": {
            "host": dashboard_host,
            "location": "local" if dashboard_host in _LOCAL_HOSTS else "external",
        },
    }


def build_launch_meta(benchmark_dir, llmd_benchmark_dir):
    llmdbenchmark_bin = os.path.join(llmd_benchmark_dir, ".venv", "bin", "llmdbenchmark")
    return {
        "specs": _discover_specs(benchmark_dir),
        "cluster_configs": _discover_cluster_configs(benchmark_dir),
        "harnesses": _discover_harnesses(llmd_benchmark_dir),
        "kube_context": _current_kube_context(),
        "llmd_benchmark_dir": llmd_benchmark_dir,
        "llmdbenchmark_bin_found": os.path.isfile(llmdbenchmark_bin),
    }


class _DashboardHandler(http.server.BaseHTTPRequestHandler):
    workspace = None  # set by cmd_serve before serving
    report_html_path = None
    benchmark_dir = None
    repo_root = None
    llmd_benchmark_dir = None
    grafana_url = DEFAULT_GRAFANA_URL
    grafana_user = DEFAULT_GRAFANA_USER
    grafana_password = DEFAULT_GRAFANA_PASSWORD
    dashboard_uid = DASHBOARD_UID
    prometheus_url = None  # always resolved by resolve_remote_prometheus before the server starts
    prometheus_bearer_token = None
    prometheus_tls_skip_verify = False
    kube_context = None  # pinned once by cmd_serve at startup -- see build_cluster_status
    grafana_port_forward = None  # set by cmd_serve when grafana_url is local

    def log_message(self, fmt, *args):
        sys.stderr.write(f"{self.address_string()} - {fmt % args}\n")

    def _send_bytes(self, body, content_type, code=200):
        self.send_response(code)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def _send_json(self, code, obj):
        self._send_bytes(json.dumps(obj).encode(), "application/json", code=code)

    def _send_error(self, code, message):
        self.send_response(code)
        self.send_header("Content-Type", "text/plain")
        self.end_headers()
        self.wfile.write(message.encode())

    def do_GET(self):
        path = urllib.parse.urlparse(self.path).path
        if path == "/" or path == "/report.html":
            self._send_bytes(Path(self.report_html_path).read_bytes(), "text/html; charset=utf-8")
        elif path == "/index.json":
            manifest = build_dashboard_index(self.workspace, self.grafana_url, self.dashboard_uid)
            self._send_bytes(json.dumps(manifest).encode(), "application/json")
        elif path == "/meta.json":
            meta = build_launch_meta(self.benchmark_dir, self.llmd_benchmark_dir)
            self._send_bytes(json.dumps(meta).encode(), "application/json")
        elif path == "/system-status.json":
            status = build_system_status(
                self.grafana_url, self.prometheus_url, self.server.server_address[0],
                self.kube_context, self.grafana_port_forward,
                prometheus_bearer_token=self.prometheus_bearer_token,
                prometheus_tls_skip_verify=self.prometheus_tls_skip_verify,
                grafana_user=self.grafana_user, grafana_password=self.grafana_password,
            )
            self._send_bytes(json.dumps(status).encode(), "application/json")
        elif path == "/api/port-forward/discover":
            target = (urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query).get("target") or [None])[0]
            if target != "grafana":
                self._send_json(400, {"error": f"Unknown target: {target!r}"})
                return
            services = discover_grafana_services(context=self.kube_context)
            self._send_json(200, {"target": target, "services": services})
        elif path.startswith("/results/"):
            requested = Path(self.workspace).resolve() / Path(path[len("/results/"):])
            root = Path(self.workspace).resolve()
            if root not in requested.resolve().parents and requested.resolve() != root:
                self._send_error(403, "Forbidden")
                return
            if not requested.is_file():
                self._send_error(404, "Not found")
                return
            content_type = mimetypes.guess_type(str(requested))[0] or "application/octet-stream"
            self._send_bytes(requested.read_bytes(), content_type)
        else:
            self._send_error(404, "Not found")

    def _is_same_origin(self):
        """Best-effort CSRF guard: refuse cross-origin/no-origin state-changing requests.

        Prefers the Fetch Metadata `Sec-Fetch-Site` header (sent by current
        Chrome/Firefox/Edge for every request, same-origin included); falls
        back to comparing the `Origin` header's host to our own `Host`.
        Missing both is treated as untrusted (e.g. curl/script), not same-origin.
        """
        sec_fetch_site = self.headers.get("Sec-Fetch-Site")
        if sec_fetch_site is not None:
            return sec_fetch_site == "same-origin"
        origin = self.headers.get("Origin")
        host = self.headers.get("Host")
        if not origin or not host:
            return False
        return urllib.parse.urlparse(origin).netloc == host

    def do_DELETE(self):
        path = urllib.parse.urlparse(self.path).path
        prefix = "/results/"
        if not path.startswith(prefix):
            self._send_error(404, "Not found")
            return
        if not self._is_same_origin():
            self._send_error(403, "Forbidden (cross-origin delete rejected)")
            return

        session_id = urllib.parse.unquote(path[len(prefix):])
        if not session_id or "/" in session_id or session_id in (".", ".."):
            self._send_error(400, "Bad request: expected /results/<session-id>")
            return

        root = Path(self.workspace).resolve()
        target = root / session_id
        if os.path.islink(target) or target.resolve().parent != root or not target.is_dir():
            self._send_error(404, "Not found")
            return

        shutil.rmtree(target)
        self._send_bytes(json.dumps({"deleted": session_id}).encode(), "application/json")

    def _read_json_body(self):
        try:
            length = int(self.headers.get("Content-Length", 0))
            return json.loads(self.rfile.read(length) or b"{}")
        except (ValueError, json.JSONDecodeError):
            return None

    def _handle_snapshot(self):
        """POST /api/snapshot {"dir": "<experiment dir relative to workspace>"} -- capture a
        Grafana snapshot for one experiment (same as the `snapshot` CLI subcommand)."""
        if not self._is_same_origin():
            self._send_json(403, {"error": "Forbidden (cross-origin request rejected)"})
            return
        body = self._read_json_body()
        if body is None:
            self._send_json(400, {"error": "Invalid JSON body"})
            return

        rel = body.get("dir") or ""
        root = Path(self.workspace).resolve()
        target = (root / rel).resolve()
        if root not in target.parents or not (target / "run_metadata.yaml").is_file():
            self._send_json(400, {"error": f"Unknown experiment dir: {rel!r}"})
            return

        try:
            out = _capture_snapshot(
                str(target), self.grafana_url, self.grafana_user, self.grafana_password, self.dashboard_uid
            )
        except (ConnectionError, RuntimeError, ValueError) as e:
            self._send_json(502, {
                "error": f"{e}. Is Grafana reachable at {self.grafana_url} (run `configure` once) and the "
                         "cluster's Prometheus (via Thanos Querier) still holding this run's data?",
            })
            return
        self._send_json(200, {
            "snapshot_url": out.get("snapshot_url"),
            "live_dashboard_url": out.get("live_dashboard_url"),
        })

    def _port_forward_for(self, target):
        return {"grafana": self.grafana_port_forward}.get(target)

    def _handle_port_forward_start(self):
        """POST /api/port-forward/start {"target", "namespace", "service", "port"} -- start
        the native port-forward this server manages for `target` (see ServicePortForward),
        to the Service the operator picked from the discover list or entered manually."""
        if not self._is_same_origin():
            self._send_json(403, {"error": "Forbidden (cross-origin request rejected)"})
            return
        body = self._read_json_body() or {}
        pf = self._port_forward_for(body.get("target"))
        if pf is None:
            self._send_json(400, {"error": f"Port-forwarding isn't available for target {body.get('target')!r}."})
            return
        namespace, service, port = body.get("namespace"), body.get("service"), body.get("port")
        if not namespace or not service or not port:
            self._send_json(400, {"error": "namespace, service, and port are required"})
            return
        try:
            port = int(port)
        except (TypeError, ValueError):
            self._send_json(400, {"error": f"Invalid port: {port!r}"})
            return
        ok, error = pf.start(namespace, service, port, context=self.kube_context)
        if not ok:
            self._send_json(502, {"error": error})
            return
        _remember_port_forward(body.get("target"), self.kube_context, namespace, service, port)
        self._send_json(200, pf.status())

    def _handle_port_forward_stop(self):
        if not self._is_same_origin():
            self._send_json(403, {"error": "Forbidden (cross-origin request rejected)"})
            return
        body = self._read_json_body() or {}
        pf = self._port_forward_for(body.get("target"))
        if pf is None:
            self._send_json(400, {"error": f"Port-forwarding isn't available for target {body.get('target')!r}."})
            return
        pf.stop()
        _forget_port_forward(body.get("target"), self.kube_context)
        self._send_json(200, pf.status())

    def do_POST(self):
        path = urllib.parse.urlparse(self.path).path
        if path == "/api/snapshot":
            self._handle_snapshot()
            return
        if path == "/api/port-forward/start":
            self._handle_port_forward_start()
            return
        if path == "/api/port-forward/stop":
            self._handle_port_forward_stop()
            return
        if path != "/api/launch":
            self._send_error(404, "Not found")
            return
        if not self._is_same_origin():
            self._send_json(403, {"error": "Forbidden (cross-origin request rejected)"})
            return

        try:
            length = int(self.headers.get("Content-Length", 0))
            body = json.loads(self.rfile.read(length) or b"{}")
        except (ValueError, json.JSONDecodeError):
            self._send_json(400, {"error": "Invalid JSON body"})
            return

        action = body.get("action")
        if action not in ("standup", "run", "teardown", "all"):
            self._send_json(400, {"error": f"Unknown action: {action!r}"})
            return

        meta = build_launch_meta(self.benchmark_dir, self.llmd_benchmark_dir)
        specs_by_id = {s["id"]: s for s in meta["specs"]}
        cluster_configs_by_id = {c["id"]: c for c in meta["cluster_configs"]}
        harnesses_by_id = {h["id"]: h for h in meta["harnesses"]}

        spec = body.get("spec")
        cluster_config = body.get("clusterConfig")
        namespace = body.get("namespace") or ""
        dry_run = bool(body.get("dryRun"))

        if spec not in specs_by_id:
            self._send_json(400, {"error": f"Unknown spec: {spec!r}"})
            return
        if cluster_config not in cluster_configs_by_id:
            self._send_json(400, {"error": f"Unknown cluster-config: {cluster_config!r}"})
            return
        if not _K8S_NAMESPACE_RE.match(namespace):
            self._send_json(400, {"error": "Namespace is required and must be a valid Kubernetes namespace name"})
            return
        if not meta["llmdbenchmark_bin_found"]:
            self._send_json(400, {
                "error": f"llmdbenchmark CLI not found under {meta['llmd_benchmark_dir']} -- "
                         "see benchmark/README.md to set up the sibling llm-d-benchmark clone.",
            })
            return

        llmdbenchmark_bin = os.path.join(self.llmd_benchmark_dir, ".venv", "bin", "llmdbenchmark")
        common_flags = [
            "--spec", specs_by_id[spec]["path"],
            "--cluster-config", cluster_configs_by_id[cluster_config]["path"],
            "--workspace", self.workspace,
            "-p", namespace,
        ]

        def make_argv(verb, extra=None):
            argv = [llmdbenchmark_bin, verb] + common_flags + (extra or [])
            if dry_run and verb != "teardown":  # teardown always removes real resources
                argv.append("--dry-run")
            return argv

        def validate_harness_workload():
            harness = body.get("harness")
            workload = body.get("workload")
            if harness not in harnesses_by_id:
                self._send_json(400, {"error": f"Unknown harness: {harness!r}"})
                return None
            if workload not in harnesses_by_id[harness]["workloads"]:
                self._send_json(400, {"error": f"Unknown workload {workload!r} for harness {harness!r}"})
                return None
            return harness, workload

        def spawn(argv):
            return subprocess.Popen(
                argv, cwd=self.repo_root, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                start_new_session=True,
            )

        if action == "all":
            hw = validate_harness_workload()
            if hw is None:
                return
            harness, workload = hw
            argv_chain = [make_argv("standup"), make_argv("smoketest"), make_argv("run", ["-l", harness, "-w", workload])]

            def orchestrate():
                # Each phase gets its own workspace directory (the CLI always mints a
                # fresh timestamped one); this just runs them back to back, stopping
                # at the first non-zero exit -- same "stopping on failure" behavior
                # the run-benchmark skill documents for a manual dry-run -> standup ->
                # smoketest -> run walkthrough.
                for phase_argv in argv_chain:
                    try:
                        proc = spawn(phase_argv)
                    except OSError:
                        return
                    if proc.wait() != 0:
                        return

            threading.Thread(target=orchestrate, daemon=True).start()
            self._send_json(202, {"launched": "all", "namespace": namespace})
            return

        argv = make_argv(action)
        if action == "run":
            hw = validate_harness_workload()
            if hw is None:
                return
            harness, workload = hw
            argv += ["-l", harness, "-w", workload]

        try:
            proc = spawn(argv)
        except OSError as e:
            self._send_json(500, {"error": f"Failed to launch: {e}"})
            return
        threading.Thread(target=proc.wait, daemon=True).start()  # reap without blocking the server

        self._send_json(202, {"launched": action, "pid": proc.pid, "namespace": namespace})


def cmd_serve(args):
    workspace = os.path.abspath(args.workspace)
    if not os.path.isdir(workspace):
        print(f"ERROR: workspace not found: {workspace}", file=sys.stderr)
        return 1
    hack_dir = os.path.dirname(os.path.abspath(__file__))
    report_html_path = os.path.join(hack_dir, "report.html")
    if not os.path.isfile(report_html_path):
        print(f"ERROR: {report_html_path} not found", file=sys.stderr)
        return 1

    benchmark_dir = os.path.normpath(os.path.join(hack_dir, ".."))
    repo_root = os.path.normpath(os.path.join(benchmark_dir, ".."))
    llmd_benchmark_dir = (
        os.path.abspath(args.llm_d_benchmark_dir) if args.llm_d_benchmark_dir
        else os.path.normpath(os.path.join(repo_root, "..", "llm-d-benchmark"))
    )
    if not os.path.isfile(os.path.join(llmd_benchmark_dir, ".venv", "bin", "llmdbenchmark")):
        print(
            f"WARNING: llmdbenchmark CLI not found under {llmd_benchmark_dir} -- "
            "the \"Start a session\" form will show but launching will fail. See benchmark/README.md.",
            file=sys.stderr,
        )

    # Pinned once at startup rather than re-read from `kubectl config current-context` on
    # every request, so a `kubectl config use-context` run elsewhere while this server is up
    # doesn't silently redirect its cluster status, service discovery, or port-forwards --
    # see build_cluster_status. Restart the server (or pass --context) to point it elsewhere.
    kube_context = args.context or _current_kube_context()
    if kube_context:
        print(f"Pinned kube context: {kube_context} (won't follow `kubectl config use-context` elsewhere; restart to change it)")
    else:
        print("WARNING: no kube context found -- cluster status, Grafana service discovery, and Thanos Querier discovery will be unavailable.", file=sys.stderr)

    err = resolve_remote_prometheus(args, context=kube_context)
    if err:
        print(f"ERROR: {err}", file=sys.stderr)
        return 1

    # Native port-forwarding only makes sense for Grafana when it's expected on this same
    # machine but isn't actually installed locally (e.g. an in-cluster Grafana forwarded to
    # localhost for convenience) -- if --grafana-url points elsewhere, there's nothing for
    # this server to forward to reach it. Prometheus is always remote (via Thanos Querier),
    # so it has no port-forward of its own.
    def _local_port_forward(url, default_port):
        parsed = urllib.parse.urlparse(url)
        return ServicePortForward(parsed.port or default_port) if parsed.hostname in _LOCAL_HOSTS else None

    grafana_port_forward = _local_port_forward(args.grafana_url, 3000)

    # Reconnect whatever Service the operator last picked via the port-forward picker for
    # this kube context, so a server restart doesn't silently drop it and leave Grafana
    # pointed at nothing until the picker is used again -- see _remember_port_forward.
    saved_state = _load_port_forward_state()
    saved = saved_state.get("grafana", {}).get(kube_context or "")
    if grafana_port_forward and saved:
        ok, error = grafana_port_forward.start(saved["namespace"], saved["service"], saved["port"], context=kube_context)
        if ok:
            print(f"Restored grafana port-forward: {saved['namespace']}/{saved['service']}:{saved['port']} -> localhost:{grafana_port_forward.local_port}")
        else:
            print(f"WARNING: could not restore grafana port-forward to {saved['namespace']}/{saved['service']}: {error}", file=sys.stderr)

    handler = type("_BoundDashboardHandler", (_DashboardHandler,), {
        "workspace": workspace,
        "report_html_path": report_html_path,
        "benchmark_dir": benchmark_dir,
        "repo_root": repo_root,
        "llmd_benchmark_dir": llmd_benchmark_dir,
        "grafana_url": args.grafana_url,
        "grafana_user": args.grafana_user,
        "grafana_password": args.grafana_password,
        "dashboard_uid": args.dashboard_uid,
        "prometheus_url": args.prometheus_url,
        "prometheus_bearer_token": args.prometheus_bearer_token,
        "prometheus_tls_skip_verify": args.prometheus_tls_skip_verify,
        "kube_context": kube_context,
        "grafana_port_forward": grafana_port_forward,
    })
    server = http.server.ThreadingHTTPServer((args.host, args.port), handler)

    # kubectl port-forward children run with start_new_session=True (so they survive this
    # process's own signal-handling quirks), which also means a plain `kill <pid>` -- SIGTERM,
    # what `kill` sends by default and what most process managers use to stop a daemon --
    # doesn't reach them the way Ctrl+C's SIGINT does; only the KeyboardInterrupt this
    # process itself catches ever ran the cleanup below. Without this handler, killing the
    # server with a bare `kill` leaves them running as orphans holding the port, which then
    # blocks the *next* server's restore-on-startup (see saved_state above) from binding it.
    def _handle_sigterm(signum, frame):
        sys.exit(0)
    signal.signal(signal.SIGTERM, _handle_sigterm)

    print(f"Serving dashboard at http://{args.host}:{args.port}/  (workspace: {workspace})")
    print("Press Ctrl+C to stop.")
    try:
        server.serve_forever()
    except (KeyboardInterrupt, SystemExit):
        pass
    finally:
        if grafana_port_forward:
            grafana_port_forward.stop()
        server.server_close()
    return 0


# --------------------------------------------------------------------------
# CLI
# --------------------------------------------------------------------------

def _add_grafana_auth_args(parser):
    parser.add_argument("--grafana-url", default=DEFAULT_GRAFANA_URL)
    parser.add_argument("--grafana-user", default=DEFAULT_GRAFANA_USER)
    parser.add_argument("--grafana-password", default=DEFAULT_GRAFANA_PASSWORD)


def _add_prometheus_auth_args(parser):
    """Prometheus is always reached remotely, through OpenShift's Thanos Querier: if
    --prometheus-url isn't given, it's auto-discovered (Route in openshift-monitoring, token
    minted from a ServiceAccount already bound to cluster-monitoring-view, TLS skip-verify
    enabled) -- see resolve_remote_prometheus and docs/benchmark-report.md. Pass
    --prometheus-url yourself to skip discovery (e.g. a plain Prometheus you've
    kubectl-port-forwarded by hand)."""
    parser.add_argument(
        "--prometheus-url",
        help="Remote Prometheus URL Grafana's datasource points at. If omitted, "
             "auto-discovered as the cluster's OpenShift Thanos Querier Route.",
    )
    parser.add_argument(
        "--prometheus-bearer-token",
        help="Bearer token Grafana's Prometheus datasource sends with every request "
             "(e.g. a ServiceAccount token for a Thanos Querier route/Service). "
             "Re-applied on every `configure` run. Minted automatically when "
             "--prometheus-url is auto-discovered.",
    )
    parser.add_argument(
        "--prometheus-tls-skip-verify",
        action="store_true",
        help="Skip TLS certificate verification on the Prometheus datasource "
             "(e.g. a Route whose cert Grafana doesn't trust). Enabled automatically when "
             "--prometheus-url is auto-discovered.",
    )
    parser.add_argument(
        "--prometheus-service-account-namespace", default="openshift-monitoring",
        help="Namespace to look for a ServiceAccount already bound to cluster-monitoring-view "
             "in, for Thanos Querier auto-discovery (default: openshift-monitoring).",
    )
    parser.add_argument(
        "--prometheus-token-duration", default="8760h",
        help="Lifetime of an auto-minted Thanos Querier token (default: 8760h = 1 year). "
             "Re-run configure with a fresh one before it expires.",
    )


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)

    p_configure = sub.add_parser("configure", help="Provision the Prometheus datasource + benchmark dashboard")
    _add_grafana_auth_args(p_configure)
    _add_prometheus_auth_args(p_configure)
    p_configure.add_argument("--context", help="kube context to use for Thanos Querier discovery (default: current `kubectl config current-context`).")
    p_configure.set_defaults(func=cmd_configure)

    p_snapshot = sub.add_parser("snapshot", help="Capture a Grafana snapshot for one experiment")
    p_snapshot.add_argument("experiment_dir")
    _add_grafana_auth_args(p_snapshot)
    p_snapshot.add_argument("--dashboard-uid", default=DASHBOARD_UID)
    p_snapshot.set_defaults(func=cmd_snapshot)

    p_report = sub.add_parser("report", help="Render report.html for a session")
    p_report.add_argument("session_dir", help="llmdbenchmark session/workspace dir, e.g. benchmark/results/<user>-<timestamp>")
    p_report.set_defaults(func=cmd_report)

    p_all = sub.add_parser("all", help="configure + snapshot (per experiment) + report")
    p_all.add_argument("session_dir", help="llmdbenchmark session/workspace dir, e.g. benchmark/results/<user>-<timestamp>")
    _add_grafana_auth_args(p_all)
    _add_prometheus_auth_args(p_all)
    p_all.add_argument("--context", help="kube context to use for Thanos Querier discovery (default: current `kubectl config current-context`).")
    p_all.add_argument("--dashboard-uid", default=DASHBOARD_UID)
    p_all.add_argument("--skip-configure", action="store_true")
    p_all.set_defaults(func=cmd_all)

    default_workspace = os.path.normpath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "results"))

    p_index = sub.add_parser("index", help="Scan a workspace and write its dashboard JSON manifest")
    p_index.add_argument("--workspace", default=default_workspace, help="llmdbenchmark --workspace dir (default: benchmark/results)")
    p_index.add_argument("--out", help="Output path (default: <workspace>/index.json)")
    p_index.set_defaults(func=cmd_index)

    p_serve = sub.add_parser("serve", help="Serve the interactive dashboard (report.html) over a workspace")
    p_serve.add_argument("--workspace", default=default_workspace, help="llmdbenchmark --workspace dir (default: benchmark/results)")
    p_serve.add_argument(
        "--host", default="127.0.0.1",
        help="Address to bind the dashboard server to (default: 127.0.0.1, local-only). "
             "Use 0.0.0.0 to host the dashboard for remote access.",
    )
    p_serve.add_argument("--port", type=int, default=8787)
    p_serve.add_argument(
        "--context",
        help="kube context to use (default: current `kubectl config current-context`). "
             "Pinned for this server's lifetime so cluster status, Grafana service "
             "discovery/port-forwards, and Thanos Querier discovery aren't affected by "
             "`kubectl config use-context` run elsewhere while it's up -- restart the "
             "server to pick up a different context.",
    )
    p_serve.add_argument(
        "--llm-d-benchmark-dir",
        help="Path to the llm-d-benchmark clone (default: ../llm-d-benchmark next to this repo), "
             "used to discover harnesses/workloads and launch runs from the dashboard",
    )
    _add_grafana_auth_args(p_serve)
    _add_prometheus_auth_args(p_serve)
    p_serve.add_argument(
        "--dashboard-uid", default=DASHBOARD_UID,
        help="Grafana dashboard UID used for live links and the snapshot button",
    )
    p_serve.set_defaults(func=cmd_serve)

    args = parser.parse_args()
    sys.exit(args.func(args))


if __name__ == "__main__":
    main()
