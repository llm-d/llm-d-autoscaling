# Interactive dashboard

`benchmark/hack/benchmark_report.py serve` runs a live dashboard over every
session directory under a `--workspace` (default `benchmark/results`) --
including sessions that are still standing up or running, not just finished
ones. It scans the workspace fresh on every page load/refresh (no Grafana or
Prometheus required for the dashboard itself) and is organized spec-first.

For turning one finished session into a standalone `report.html` with
permanent Grafana snapshots, see [`benchmark-report.md`](benchmark-report.md)
instead -- this doc covers `serve` only.

```bash
benchmark/hack/benchmark_report.sh serve --workspace benchmark/results
```

then open `http://127.0.0.1:8787/`.

The topbar shows a lean status strip for the systems the dashboard depends
on: the current kube context (reachable or not), Prometheus, and Grafana --
each tagged **local** or **remote** depending on whether it's on this
machine (Prometheus always shows **remote**, since it's always reached via
Thanos Querier). The kube context is pinned once when `serve` starts (from
`kubectl config current-context`, or `--context`) rather than re-read on
every request, so a `kubectl config use-context` run elsewhere while the
server is up can't silently redirect its cluster status, Grafana service
discovery, or Grafana's port-forward -- a drift warning (&#9888;) shows on
the Cluster chip instead, and restarting the server picks up the new
context.

If Grafana is expected locally (the default `http://localhost:3000`) but
isn't reachable, its chip gets a gear icon that opens a port-forward picker:
it lists every Service in the cluster matching the usual Grafana label (plus
`kube-prometheus-stack-grafana`/`grafana` wherever they live, in case an
older chart doesn't set the label), since a cluster can have more than one.
Pick one (or type a namespace/service/port manually if discovery misses it)
and start; the server manages the underlying `kubectl port-forward` itself,
no more running it by hand (see [`benchmark-report.md`](benchmark-report.md)'s
"One-time setup" for installing Grafana in the first place). Pass `--host
0.0.0.0` to `serve` to host the dashboard for remote access instead of
`127.0.0.1`-only; native port-forwarding then becomes unavailable, since it
needs a local `kubectl`. Prometheus never gets a picker -- there's nothing
local to forward to, since it's always reached remotely via its Thanos
Querier Route.

Whichever Grafana Service you pick is remembered per kube context in
`benchmark/hack/.port-forward-state.json` (local to this checkout, not
committed) and reconnected automatically the next time `serve` starts for
that same context -- restarting the dashboard server doesn't drop it.
Stopping the port-forward from the picker forgets it too, so it won't come
back on the next restart. `serve` also cleans up its `kubectl port-forward`
child on a plain `kill` (SIGTERM), not just Ctrl+C, so restarting it
doesn't leave one behind holding the port.

## Views

- **Specs** (the landing page) — every spec under
  `benchmark/config/specification/**/*.yaml.j2` (guides and staging
  scaling-strategy variants), with a rollup of how many of its sessions are in
  progress, need attention, or completed, plus how many produced benchmark
  data. A session that stood up and then went idle (no log activity for a
  while) counts as settled **infrastructure**, not "in progress" — only
  actively-executing or freshly-waiting sessions are in progress. Sessions
  whose spec couldn't be determined yet (or whose spec file has since been
  removed) get an "(unspecified spec)" bucket instead of being hidden.
- a **spec detail** page (click a spec) — every session that has ever used
  this spec, historical or still in progress, with the same stage
  badges/log-error flags as before, followed by a **Start a session** form
  (pick a cluster-config + namespace and Standup, or a harness + workload and
  Run, or Teardown -- see "Launching sessions" below). A **stage filter**
  buckets sessions into *in progress*, *infrastructure* (settled
  standup-only), *attention*, *completed*, and *torn down*. Sessions that
  produced no benchmark data (standup-only infrastructure, in-progress, or
  torn down without a run) are de-emphasized with a neutral chip, never
  hidden.
- a **session detail** view — stage timeline, log tail (stdout/stderr, with
  links to the full logs), and per-experiment latency percentile charts,
  success/failure counts, and SLO-adjusted goodput, once an experiment has
  written `run_metadata.yaml`. Right under the stage badge, an
  **Observability** line links to Grafana and Prometheus for the session as a
  whole, scoped to its namespace and its lifetime so far (start of its first
  log line to "now" while still active, freezing at the last log line once
  torn down) -- unlike the per-experiment links below, this works from the
  moment a session starts, before any `run_metadata.yaml` exists. The
  Prometheus link opens its `/query` UI pre-filled with the KV-cache
  query; it needs the dashboard's own `--prometheus-url` reachable (see the
  system-status strip above). **Caveat:** vLLM's
  Prometheus metrics carry no per-session label, only `namespace` -- these
  links can only disambiguate sessions by time window, so if the same
  namespace is reused for a later session, the two can't be told apart in
  Grafana/Prometheus once their windows overlap or once retention drops the
  time boundary. Use one namespace per session -- see
  [`benchmark-report.md`](benchmark-report.md)'s "Identifying a session's
  metrics".
  Each experiment's **Grafana** section has its own **Live dashboard**
  link — time-boxed to that run's window and scoped to its
  namespace, so it shows only that run's data — available as soon as
  `run_metadata.yaml` exists (no snapshot needed; it just needs the local
  Grafana + the cluster's Prometheus, via Thanos Querier, still holding the
  data). If no snapshot has been captured
  yet, a **Create Grafana snapshot** button freezes that run's panels into a
  permanent snapshot (survives Prometheus retention) without dropping to the
  CLI. Its back-link returns to that session's spec page.
  When a session is waiting on input (standup done and ready to run a workload,
  a run finished and ready to run again, a failed phase ready to retry,
  ...), a **Next step** banner explains what's next and offers the button
  for it, pre-filled with that session's spec/cluster-config/namespace. Since
  every `llmdbenchmark` phase always gets its own fresh workspace directory
  (there's no way to make it reuse one), clicking one of these launches a
  new sibling session rather than continuing this one -- the banner sends you to
  that spec's page after a couple of seconds so you can watch the new session
  appear.
- a **Compare** view — once two or more sessions share the same model + backend
  (e.g. a `baseline` vs a `staging/<guide>/kv-early` scaling strategy), it
  compares them side by side. This is the one cross-spec view, since
  comparing scaling strategies means comparing sessions from *different* specs.

Only the `inference-perf` harness's lifecycle metrics are parsed into
charts today; other harnesses (`guidellm`, `vllm-benchmark`, ...) show their
raw result files with a note instead of charts.

A session's detail page (and a trash icon per row in a spec's session table) has a
**Delete session** action that permanently removes its directory from disk
(logs, plan, results) after a confirm prompt -- it doesn't touch cluster
resources, so run `teardown` separately if the session is still standing. The
server only accepts the delete from the dashboard page itself
(same-origin), binds to `127.0.0.1` only, and only ever deletes a directory
that is a direct child of `--workspace`.

## Launching sessions

Each spec's page has a **Start a session** section: pick a cluster-config
(scanned recursively from `benchmark/config/cluster-configs/**/*.yaml`, so
platform subdirectories like `k8s/`/`ocp/` are all discovered), a namespace, and a
harness + workload (scanned from the sibling `llm-d-benchmark` clone's
`workload/profiles/<harness>/`). From there:

- **Run all phases** chains standup → smoketest → run for you, stopping
  automatically if standup or smoketest fails (mirroring the manual
  dry-run → standup → smoketest → run walkthrough the `run-benchmark` skill
  documents). Each phase still lands in its own session directory -- the
  orchestration itself lives entirely server-side in a background thread
  (three sequential subprocess calls, gated on exit code), so it keeps
  going even if you close the browser tab.
- Or run **Standup**, **Run**, and **Teardown** individually.

Each button shows the exact `llmdbenchmark` command(s) (spec fixed to the
page you're on) and the current kube-context in a confirm prompt before
spawning anything -- the new session(s) show up on that spec's page within a
few seconds, same as if launched from the CLI. Same CSRF/localhost-only
guards as delete; inputs are validated against the scanned
spec/cluster-config/harness/workload lists (never taken as raw paths) and
the namespace against Kubernetes naming rules.

`benchmark_report.py index --workspace benchmark/results` writes the same
data as a one-off `index.json` file, if you want it without running a
server.
