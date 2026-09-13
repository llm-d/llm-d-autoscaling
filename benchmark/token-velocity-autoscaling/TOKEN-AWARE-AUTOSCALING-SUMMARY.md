# Token-aware autoscaling on llm-d — design summary

## Introduction

**P/D disaggregation** splits inference across two different kinds of pod. **Prefill** pods read the incoming
prompt and build its KV cache — compute-heavy, over in a moment. **Decode** pods then generate the answer one
token at a time — memory-heavy, and they hold the request for its whole lifetime. The KV cache is copied from
prefill to decode over a side channel (NIXL). Splitting the roles means each can be sized against its own
bottleneck instead of both being dragged along by one number.

**The EPP** (Endpoint Picker) is llm-d's router. For every request it chooses which prefill pod and which
decode pod will serve it, and it publishes metrics about what it dispatched.

**KEDA** is the autoscaler. Each role gets a `ScaledObject` holding one Prometheus query and one threshold.
KEDA runs the query, divides the result by the threshold, and rounds up — that is the replica count. So the
query's job is to answer *"how many pods' worth of work is outstanding right now?"*

**"Token-aware"** means those queries count **tokens**, not requests. A 8192-token prompt is 16× the work of
a 512-token one; a request counter rates them the same, and that is the gap this design closes.

**Two topologies, one design.** Sections §1–§7 describe the **P/D-disaggregated** deployment in detail. The
same token-velocity approach also drives a **co-located** deployment, where each pod runs both phases on a
single `InferencePool` (this repo's `optimized-baseline/` guide). **§8 covers it** — all the token math carries
over unchanged; only the query *shape* and the *number* of `ScaledObject`s differ.

## 1. Prerequisite: measure your prefill token rate

Token-aware autoscaling works by dividing **tokens of queued work** by **tokens per second of capacity**.
That second number is `peakPrefillThroughput`, and `guides/recipes/router/calibration/calibrate.sh` is what
measures it — sustained prefill tokens/s **through the full P/D path**, including the KV transfer and sidecar
hop.
For when to re-run it and how, see the
[calibrate.sh guide](https://github.com/llm-d/llm-d/blob/main/guides/recipes/router/calibration/README.md).

Measure it on your own hardware, through your own P/D path, rather than borrowing a published figure. The
value it produces — **`peakPrefillThroughput`** — is what the **prefill** trigger divides by, so it sets when
prefill scales and how accurately. Nothing on the decode side reads it.

## 2. `peakPrefillThroughput` and the prefill threshold

Two components read `peakPrefillThroughput`, and both act on prefill only:

1. **The EPP router**, as a `prefix-cache-affinity-filter` parameter — it prices recomputing a prompt against
   reusing a cached prefix when choosing which prefill pod gets the request.
2. **The KEDA prefill trigger**, has the denominator (V_P) that converts queued tokens into seconds of backlog.

**`peakPrefillThroughput` is not the KEDA threshold.** It sits inside the Prometheus query, where it converts
a token count into a number of seconds. KEDA then takes that result — a duration — and compares it against its
own `threshold` (a number of seconds of queue backlog you're willing to tolerate per replica):

```
replicas = ceil(  queued_tokens ÷ peakPrefillThroughput  ÷  threshold  )
                 └──── converts tokens → seconds ────┘   └ seconds of queue budget ┘
```

**Where the threshold comes from: your TTFT SLO.** It is not measured from anything — it is the share of your
time-to-first-token budget you're willing to spend waiting in the prefill queue. Queue wait is only one part of
TTFT; a request also pays its own prefill pass, the KV transfer to decode, and the first decode step. So a given
threshold only makes sense if your TTFT target sits comfortably above the workload's fixed floor. The value is
therefore **workload-dependent** — the shipped default of `1.5 s` suits short-context/interactive traffic (~2 s
TTFT); long-context serving needs a larger budget. The next subsection derives it.

**This is a tuning parameter.** Lower it to scale earlier and hold more prefill pods; raise it to tolerate more
queueing and run leaner. Being a little off costs slower first tokens, not failures — prefill latency degrades
gradually.

### Choosing the threshold per workload — 1.5 s is not universal

The threshold is **the queue-wait budget left after the irreducible floor of your workload**, so it changes with
input length.

```
threshold ≈ TTFT_SLO − (ISL_uncached / V_P)        [seconds]
            └Product target┘  └─── the idle floor ───┘
```

**`ISL/V_P` already *is* the end-to-end idle floor — do not add a transfer term.** `calibrate-peak-prefill.sh`
defines `V_P = CHUNK_SIZE / median(TTFT)`, and it measures **TTFT through the router on an idle stack**. Because
TTFT is time-to-*first-token*, that number already contains the NIXL KV transfer to decode and the first decode
step (you cannot get a first token before the KV lands). So `ISL/V_P` is the whole idle TTFT for the prompt, with
transfer and first token baked in — adding "+ transfer" would double-count what V_P already absorbed. Autoscaling
only removes *queueing*; it can never go below this floor. If `TTFT_SLO ≤ floor`, no threshold can meet it — you
must raise V_P (faster GPUs / bigger prefill chunk) or shrink the *uncached* ISL (prefix caching), not scale.

Applied to the three reference workload shapes (V_P ≈ 2696):

| workload | ISL | idle floor `ISL/V_P` (transfer + first token included) | example TTFT SLO | prefill threshold ≈ SLO − floor |
|---|---|---|---|---|
| prefill-heavy | 8192 | 3.0 s | 8 s | **~5.0 s** |
| symmetrical | 2048 | 0.76 s | 3 s | **~2.2 s** |
| decode-heavy | 256 | 0.1 s | TTFT trivially met | prefill threshold ~moot; **decode KV (0.8) governs** |

**Optional serving margin.** The calibrated floor is a *median under controlled load*. Real serving runs a bit
higher — this cluster's prefill-heavy run measured **4.2 s** mean TTFT at rate 0.15 vs the 3.0 s calibrated floor.

Two adjustments before you commit a number:
- **Tail, not mean.** The metric is an *average* per replica, so ~half of requests wait longer than the
  threshold. If your SLO is a p90/p99 target, shave the threshold to ~60–70 % of the formula result.
- **Cost vs latency.** A lower threshold holds more prefill pods (better TTFT, more GPUs); a higher one runs
  leaner. The floor is fixed; everything above it trades money for latency.

The script default **`1.5 s` fits interactive / short-context traffic** (small ISL, ~2 s SLO). For long-context
serving raise it with `--prefill-threshold` per the table above — 1.5 s at ISL 8192 scales out far harder than
an 8 s SLO actually requires.


## 3. The decode threshold is a different kind of number

Decode has no equivalent of `peakPrefillThroughput` — no measured rate, no unit conversion. Its query returns
KV cache occupancy, which is already a fraction of capacity, so the threshold `0.8` is compared against it
directly. The unit is **"pods' worth of full KV cache."**

**`0.8` is a tuning parameter — how much headroom you keep.** When a decode pod's KV cache fills, vLLM stops
accepting requests outright, so you scale before reaching it: `0.8` means "act at 80% full." Lower it if you
want more margin, raise it to run hotter.

## 4. The two queries

**Prefill — 100% llm-d metrics, no vLLM.** `AverageValue`, `threshold: 1.5` (the short-context default — set
per workload, see §2), min 1 / max 10.

Reads: *tokens the router has queued on live prefill pods, divided by prefill capacity in tokens/s.*

```promql
(
  sum(
      label_replace(
        llm_d_epp_inflight_tokens{namespace="pd-test",
                                  producer_name="inflight-load-producer",
                                  endpoint_name=~".*prefill.*"},
        "target_pod", "$1", "endpoint_name", "(.+)")
    and on (target_pod)
      label_replace(
        llm_d_epp_per_endpoint_queue_size{name="qwen-qwe-ea5367d7-wen3-32b-router",
                                          model_server_endpoint=~".*prefill.*"},
        "target_pod", "$1", "model_server_endpoint", "(.+)")
  ) / 15928        # peakPrefillThroughput — your calibrate.sh figure (§1), not a constant
) or vector(0)
```

**Decode — one vLLM metric.** `threshold: 0.8`, min 1 / max 10.

Reads: *how full the decode KV caches are, added up across pods.*

```promql
sum(vllm:kv_cache_usage_perc{namespace="pd-test", pod=~".*decode.*"}) or vector(0)
```

**Why decode uses a vLLM metric.** The autoscaler needs a KV reading **per pod**, so it can filter to decode
pods and sum them. vLLM reports exactly that. The EPP's only KV metric is a single pool-wide average covering
both roles, which cannot be split by role.

## 5. The liveness gate — counting only pods that still exist

The prefill numerator is not one metric but **two, intersected**. Only one carries the value; the
other is there to answer *"which prefill pods are alive right now?"*

- **`llm_d_epp_inflight_tokens`** — the value. Published by the EPP's `inflight-load-producer`
  plugin, one series per prefill pod, holding the tokens the router has dispatched to that pod but
  not yet finished prefilling. This is the backlog we care about.
- **`llm_d_epp_per_endpoint_queue_size`** — the gate. It contributes **nothing** to the number; it
  is used only to filter the series above down to live pods.

**Why the gate is needed.** `inflight_tokens` is a `GaugeVec` whose series are **never deleted when
a pod goes away**. When prefill scales 4 → 1, the three removed pods each leave behind a *stranded*
series frozen at its last non-zero value. Summing `inflight_tokens` alone would therefore be:

```
sum(inflight_tokens) = live pod's real backlog  +  3 dead pods' phantom leftovers
```

That phantom total never falls, so the metric stays above threshold forever — the fleet could scale
**up but never back down**. It becomes a one-way ratchet.

**How the gate fixes it.** `per_endpoint_queue_size` comes from a different collector that is
**rebuilt from the currently-live endpoints on every scrape** — dead pods simply aren't in it. The
PromQL `and on (target_pod)` is a **set intersection**, not arithmetic: it keeps only the
`inflight_tokens` series whose pod *also* appears in the fresh queue-size list. Phantom series have
no match on the right, so they drop out. The result is a backlog sum over live pods only, and
scale-down works.

```promql
      label_replace(llm_d_epp_inflight_tokens{...}, "target_pod", "$1", "endpoint_name",         "(.+)")
  and on (target_pod)                        # ← intersection = liveness filter, not a value
      label_replace(llm_d_epp_per_endpoint_queue_size{...}, "target_pod", "$1", "model_server_endpoint", "(.+)")
```

The two `label_replace` calls are pure plumbing: the metrics label the same pod under different
names (`endpoint_name` vs `model_server_endpoint`), so both are rewritten to a common `target_pod`
label so PromQL can line them up.

**Decode needs no such gate.** `vllm:kv_cache_usage_perc` is scraped directly from each pod's
`/metrics`; when a decode pod dies its scrape target disappears and the series goes stale on its
own. Nothing to strip, so decode is a plain `sum(...) or vector(0)`.

## 6. Worked example — how offered load becomes a replica count

Take this cluster's calibrated figure, **`peakPrefillThroughput` (V_P) = 2696 tok/s**, and the
prefill-heavy workload where every request is **ISL = 8192 tokens**. One prefill replica can retire
2696 prefill tokens per second, so the tokens arriving per second at a request rate `r` is
`r × 8192`, and the load expressed in **"replica-equivalents" (V_P units)** is:

```
V_P units = (r × 8192) ÷ 2696
```

The trigger then rounds that up against the `1.5` threshold and clamps to `--max`:

```
replicas = min( ceil(V_P units ÷ 1.5) , max )     # here shown with max = 4
```

Because one replica saturates at `V_P ÷ ISL = 2696 ÷ 8192 = 0.33 req/s`, each extra 0.33 req/s of
offered rate should pull in one more replica. Walking the ramp used in the prefill-heavy experiment:

| rate `r` (req/s) | tokens/s = `r×8192` | V_P units = `÷2696` | replicas (ceil, cap 4) | observed |
|---|---|---|---|---|
| 0.15 | 1229 | 0.46 | 1 | 1 |
| 0.30 | 2458 | 0.91 | 1 | 1 |
| 0.50 | 4096 | 1.52 | 2 | 1 → 2 (the knee) |
| 0.80 | 6554 | 2.43 | 3 | → 4 (see note) |
| 1.40 | 11469 | 4.25 | 5 → **capped 4** | 4 (at cap) |

The knee lands exactly where the math predicts: rate 0.50 is the first stage past one replica's
0.33 req/s ceiling, and that is precisely where the second replica appeared. At rate 1.40 the offered
load wants ~5 replicas but `4 × V_P = 10784 < 11469`, so prefill pegs at the cap and builds real
backlog — the signal is right, the ceiling is the limit.

**Why observed can exceed the offered-rate prediction (the 0.80 row).** The trigger does not watch
offered rate — it watches **measured `inflight_tokens`**, i.e. the actual backlog.


## 7. EPP plugins in P/D mode (live `pd-config.yaml`, 11 plugins)

| Plugin | Purpose |
|---|---|
| `disagg-headers-handler` | Attaches the chosen prefill target as a header, before the request is sent |
| `always-disagg-pd-decider` | Decides to disaggregate — here, unconditionally |
| `disagg-profile-handler` | Runs both scheduling profiles, merges results |
| `prefill-filter` / `decode-filter` | Split the one shared pool by the `llm-d.ai/role` label |
| `approx-prefix-cache-producer` | Tracks which pods already hold a prompt's prefix (`maxPrefixTokensToMatch: 131072`) |
| `inflight-load-producer` | Publishes per-pod `inflight_tokens`/`inflight_requests` — the prefill trigger's source |
| `prefix-cache-affinity-filter` | Prefers cache-warm prefill pods; **consumes `peakPrefillThroughput`** |
| `token-load-scorer` | Ranks prefill pods by **queued token load** |
| `active-request-scorer` | Ranks decode pods by concurrent requests |
| `max-score-picker` | Picks the winner — runs **twice per request** (10804 = 2 × 5402) |

**How a request flows:** the `prefill` profile (filter → affinity → token-load-scorer → picker) and the
`decode` profile (filter → active-request-scorer → picker) both resolve in **one** scheduling cycle, before
anything is dispatched. The request is then routed to the **decode** pod, whose `routing-proxy` sidecar calls
the prefill pod named in the header, pulls the KV cache back over NIXL, and generates.

## 8. The same design on a co-located (non-P/D) deployment

Everything above assumes prefill and decode live on **separate** pods. The identical token-velocity design also
drives a **co-located** deployment — each model-server pod runs both phases (`kv_role: kv_both`, a single
`InferencePool`, one `modelserver` Deployment). This is the `optimized-baseline/` guide in this repo. **The token
math of §2, §3 and §6 is unchanged**; only the *shape* of the queries and the *number* of `ScaledObject`s differ.

**One ScaledObject, two triggers, one Deployment.** P/D gives each role its own `ScaledObject` so prefill and
decode scale independently. Co-located has a single pod type, so it uses **one `ScaledObject` carrying both
triggers**, and KEDA scales the one Deployment to the **max** of the two triggers' desired replica counts:

```
replicas = max( ceil(prefill_backlog_s ÷ 1.5) , ceil(kv_util ÷ 0.8) )
```

Whichever phase is the current bottleneck wins. The trade-off vs P/D: **you cannot size prefill and decode
separately** — a prompt-heavy burst and a generation-heavy burst both scale the *whole* pod. Simpler to operate
(one calibration, one object), less able to track a lopsided load between the two phases.

**The two queries are the plain form** (from `optimized-baseline/launch-scaledobject.sh`):

```promql
# prefill — seconds of uncached prefill backlog        (threshold 1.5)
sum(llm_d_epp_inflight_tokens{producer_name="inflight-load-producer"}) / <V_P>

# decode — pool-wide average KV occupancy              (threshold 0.8)
avg(vllm:kv_cache_usage_perc{namespace="pd-test"})
```

Two shape differences from the P/D queries in §4:
- **No role filter.** There is one pool and every pod does both phases, so the prefill query drops the
  `endpoint_name=~".*prefill.*"` selector and the decode query drops `pod=~".*decode.*"`.
- **Decode uses `avg`, not `sum`.** With separate decode pods, P/D *sums* per-pod occupancy so more pods = more
  cache to fill. Co-located reads the pool as one number: the **average** fullness across identical pods,
  compared directly to `0.8`.

**No NIXL hop in the idle floor.** V_P is still measured by `calibrate-peak-prefill.sh`, but through the
co-located path — the first token comes from the *same* pod, with no KV transfer to a separate decode pod. So the
`ISL/V_P` idle floor of §2 contains no transfer term (there is no side-channel hop to absorb); the
threshold-from-SLO derivation is otherwise identical.

**Liveness caveat carries over.** `llm_d_epp_inflight_tokens` is still a `GaugeVec` that strands series on
scale-down (§5), and the shipped co-located prefill query is the plain `sum(...) / V_P` **without** the
intersection gate. If you see prefill fail to scale back down, add the same
`and on (target_pod) … llm_d_epp_per_endpoint_queue_size` liveness filter from §5 — the mechanism is identical,
only the role selector drops out.

**EPP plugins (co-located set — 4 plugins, from `optimized-baseline-plugins.yaml`).** No disaggregation
machinery: no headers handler, no profile handler, no prefill/decode filters. A single `default` scheduling
profile:

| Plugin | Purpose |
|---|---|
| `approx-prefix-cache-producer` | Tracks which pods already hold a prompt's prefix |
| `inflight-load-producer` | Publishes per-pod `inflight_tokens` — the prefill trigger's source |
| `prefix-cache-affinity-filter` | Prefers cache-warm pods; **consumes `peakPrefillThroughput`** |
| `token-load-scorer` | Ranks pods by queued token load |

A request resolves in that one profile (`prefix-cache-affinity-filter → token-load-scorer → pick`) and is served
end-to-end by the chosen pod — no header pass, no NIXL transfer.

## References

The token-velocity approach this design follows — sizing each role by dividing a token rate by that role's
token throughput, so prefill and decode share a common denominator in tokens/s — comes from:

> Ruiqi Lai, Hongrui Liu, Chengzhi Lu, Zonghao Liu, Siyu Cao, Siyang Shao, Yixin Zhang, Luo Mai, and
> Dmitrii Ustiugov. **"TokenScale: Timely and Accurate Autoscaling for Disaggregated LLM Serving with Token
> Velocity."** arXiv:2512.03416 [cs.DC], December 2025. <https://arxiv.org/abs/2512.03416>

llm-d documentation:

- [P/D disaggregation guide](https://github.com/llm-d/llm-d/tree/main/guides/pd-disaggregation) — the
  disaggregated deployment §1–§7 describe
- [`optimized-baseline/`](./optimized-baseline) — the co-located (`kv_both`) deployment of §8, in this repo
- [`calibrate.sh` guide](https://github.com/llm-d/llm-d/blob/main/guides/recipes/router/calibration/README.md) —
  measuring `peakPrefillThroughput` (§1)
- [NIXL connector notes](https://github.com/llm-d/llm-d/blob/main/docs/operations/disaggregation/vllm.md) — how
  the KV cache moves from prefill to decode
