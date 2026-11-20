---
name: blis-evaluation
description: Run step 3 of this repo's autoscaling evaluation protocol (benchmark/protocol/) — evaluate a KEDA scenario, single-pool or prefill/decode, in blis-signal-lab (BLIS with KEDA's scale loop and an HPA per pool) on a laptop, with the same fixed run set, reports and gate as the GPU evaluation (knee staircase, SO-active staircase, mix-shift, overload under and over the cap, agentic, static-replica baseline, scale from zero, bursty, short spikes, scale-down). Translates the ScaledObjects' triggers into blis signals, runs each load, converts blis's output to llm-d-benchmark's layout, and writes one report per run (a per-stage table and a load-vs-supply figure) and a summary. Use when asked to simulate a scaling configuration, to evaluate a scenario with blis or without GPUs, to rehearse step 5, or to try a trigger change before a GPU run. Invoke with /blis-evaluation <scenario.yaml> [blis scenario] [--lab <blis-signal-lab checkout>].
---

# blis evaluation

**Arguments:** `$ARGUMENTS`: a single-pool or P/D scenario under
`benchmark/config/scenarios/`, optionally a BLIS scenario file with the
scenario's model (default: for a single pool, the vendored one matching the
scenario's GPU overlay; for P/D, one written for the evaluation;
3-blis-evaluation.md §3.3), and optionally `--lab <dir>` (default:
`$TMPDIR/src/blis-signal-lab`).

The rules live in the protocol; this skill adds the order, the gates and the
simulator commands. Read first:

- [`benchmark/protocol/run-set.md`](../../../benchmark/protocol/run-set.md):
  load, SLO and capacity (§1), the runs (§2), the reports and their writing
  rules (§3), the shared gate (§4) and the commands shared with step 5 (§5);
- [`benchmark/protocol/3-blis-evaluation.md`](../../../benchmark/protocol/3-blis-evaluation.md):
  what the simulator models (§3.2), the plant and the arms (§3.3), translating
  the triggers (§3.4), what the reports add (§3.6), the simulator gate (§3.7);
- the scenario's step 1 and 2 verdicts under `benchmark/protocol/verdicts/<guide>/`,
  if any (step 3 does not need them);
- the `run-benchmark` skill for `standup --dry-run`, and the `load-vs-supply`
  skill for the figure.

Scripts in this directory (standard library Python):

- `scripts/so_to_policy.py` writes the blis policy from the rendered
  ScaledObjects and the hand-written trigger terms, and `scaledobjects.json`
  (each ScaledObject's role, name and target) next to it;
- `scripts/blis_run.py` runs one load with an arm and leaves the run in
  llm-d-benchmark's layout, one controller per pool, with the trigger series
  from the decision logs.

The loads (`gen_load.py --target blis`) and the per-stage table
(`stage_table.py`) are shared with step 5, in `benchmark/protocol/scripts/`.

Nothing here touches a cluster. Every write is local: the lab checkout,
`benchmark/results/` (not committed) and the verdicts.

## Step 1: The lab

```bash
LAB=${LAB:-$TMPDIR/src/blis-signal-lab}
[ -d $LAB ] || git clone https://github.com/vishakha-ramani/blis-signal-lab $LAB
git -C $LAB fetch -q origin && git -C $LAB checkout 34b8b14   # the commit §3.2 describes
(cd $LAB && GOTOOLCHAIN=auto go build -o blis main.go)
```

In the Claude Code sandbox, set `GOPATH`, `GOMODCACHE` and `GOCACHE` under
`$TMPDIR`, and allow `proxy.golang.org`, `sum.golang.org` and
`storage.googleapis.com`. If the checkout is at another commit, say so: §3.2
may no longer hold, and the summary's header names the commit used.

## Step 2: Render, translate, write the policy

1. Render the scenario with `llmdbenchmark standup --dry-run` (the
   `run-benchmark` skill's setup) against its GPU overlay, in a scratch
   workspace. The ScaledObjects are in the `plan/<stack>/*.yaml` file with
   `kind: ScaledObject` (`grep -l 'kind: ScaledObject' plan/<stack>/*.yaml`).
   Read them: placeholders resolved, the thresholds intended. An E/P/D
   scenario, shared prefill-decode pods or a data-parallel P/D pool stop
   here (§3.1): say so.
2. Check the model (§3.3). It is the scenario's (`model:` in the rendered
   `plan/<stack>/config.yaml`); never substitute another.

   ```bash
   $LAB/blis catalog resolve --model <Hugging Face id> --json   # "identity" names blis's entry
   ```

   If `identity` is null, write the summary
   `benchmark/protocol/verdicts/<guide>/<scenario>-step3.md` with
   **missing model**: the model, the lab commit, the catalog searched (the
   `blis-substrate` version in the lab's `go.mod`), and every run *not run*.
   Commit it, tell the user, and stop.
3. Write the plant (§3.3), recording each choice with its source:
   - the BLIS scenario: for a single pool, the vendored file with that
     model (`grep -l "^model: <identity>$" $LAB/testdata/kernel/aisimulate/*.yaml`);
     for P/D, a scenario written from it with a `prefill` and a `decode`
     pool, each holding at least the role's `maxReplicaCount`, and a router
     config with the scenario's decider, both in `$V/` (start from the kit's
     `autoscaling-kit/scenarios/glm-5-h200-pd-autoscale.yaml` and
     `router/`);
   - the cold start;
   - the scrape interval (the rendered `plan/<stack>/18_podmonitor.yaml`)
     and the metrics delay.
4. Write `terms.json` (§3.4), one entry per trigger, in the evaluation's
   directory `benchmark/protocol/verdicts/<guide>/<scenario>-step3/`. Class
   each trigger exact or approximated, and for an approximated one, write
   where it diverges and which runs that affects. With several ScaledObjects,
   key it by ScaledObject name, then trigger name; a role-filtered gauge
   takes `"role": "prefill"` or `"decode"`. A product or ratio of series
   takes the helpers-and-expression form (§3.4).
5. Write the policy:

   ```bash
   B=.claude/skills/blis-evaluation/scripts
   V=benchmark/protocol/verdicts/<guide>/<scenario>-step3
   python3 -I $B/so_to_policy.py --scaledobject <plan>/<keda yaml> --terms $V/terms.json \
     --cold-start <s> --scrape <s> --metrics-delay <s> --out $V/policy.json
   ```

   A P/D ScaledObject's role comes from its target's `-prefill` or
   `-decode` suffix, or `--role <so>=prefill|decode`. The script prints
   what it noted (a missing `behavior` runs KEDA's legacy path, a composite
   trigger's helpers, mixed `useCachedMetrics`); copy each note into the
   summary.
6. Create the summary `benchmark/protocol/verdicts/<guide>/<scenario>-step3.md`:
   the shared header (blis-signal-lab commit, BLIS scenario and any GPU type
   or parallelism it does not share with the overlay, cold start, scrape
   interval and metrics delay with their sources, the run set's inputs), the
   translation table (§3.4), the gaps of §3.2 that apply, and the run table
   with every run *pending* (run-set.md §3). Commit it.

## Step 3: Run each run

`S=benchmark/protocol/scripts`, `RAW=benchmark/results/step3-<scenario>-<YYYYMMDD>`.
Write the knee loads with `--target blis` (run-set.md §5); for an estimate,
use a short probe (a knee at a guessed rate, read with `stage_table.py`, then
the real one centered on what it shows). Each run:

```bash
python3 -I $B/blis_run.py --lab $LAB --scenario <blis scenario> [--scenarios $V] \
  --load $LOAD --run <run> --policy $V/policy.json --out $RAW/<run> \
  --pin 1                       # knees; run 6: --pin <peak Ready of run 3>;
                                # runs 2-5, 7-10: --active
```

For P/D, pin per role (`--pin prefill=1,decode=<large enough>` for the
prefill knee, the other way round for decode, run-set.md §1), hold a role no
ScaledObject scales with `--static <role>=<n>`, and pass `--scenarios $V`
for the written scenario. An active pool with `minReplicaCount: 0` starts
after a quiet lead-in (3-blis-evaluation.md §3.3); the script prints the
Ready pods at the load's start.

It prints the fates (completed, timed out, dropped, unfinished). The
per-request records, stage windows, replica and HPA series are in
`$RAW/<run>/results/`, the trigger series in `$RAW/<run>/triggers/` (per
role for P/D, `composite.json` with a formula).

1. **The first knee gates the translation** (3-blis-evaluation.md §3.5):
   read `$RAW/<run>/sim/signals-hpa[-<role>].csv`; every trigger's
   `<name>_value` leaves 0, and each exact trigger reads what its query would
   for the load. Rerun it once with the command in `sim/command.txt` and
   compare the decision logs' sha256 with `blis-run.json` (§3.7).
2. **After the knees**, read μ_SLO with `stage_table.py --knee X`
   (run-set.md §5; no `--trace`: the clock comes from `blis-run.json`). A
   capacity-derived threshold is applied by rewriting the policy with
   `so_to_policy.py --threshold [<so>/]<trigger>=<value>`, and recorded. For
   P/D, read each role's knee with `--deployment <the role's target>`. Write the
   other runs' loads with `--target blis` and the lengths from the policy's
   behavior and cold start (run-set.md §2).
3. **Gate every run**: run-set.md §4 and 3-blis-evaluation.md §3.7. A failed
   gate is a tool or input error: fix it, rerun, and list the failed attempt
   in the summary.

## Step 4: Report each run as it finishes

Follow run-set.md §5, "Reporting each run", with `$RAW/<run>/results` as the
results directory and `--deployment <target>` (the ScaledObject's
`scaleTargetRef`, in `$V/scaledobjects.json`) for the table and the figure,
and, in item 1, these files into `$V/<run>/`:

```bash
cp $RAW/<run>/sim/{policy.json,workload.yaml,signals-hpa*.csv,signals-summary.json,command.txt} \
   $RAW/<run>/results/blis-run.json $V/<run>/
cp -r $RAW/<run>/triggers $V/<run>/
```

For P/D, write one per-stage table and one figure per pool, with that role's
`triggers/<role>/` series. With a composite trigger, draw
`--trigger triggers/composite.json:composite`.

The report header adds what 3-blis-evaluation.md §3.6 lists. The caption
says that the trigger panel is the HPA's reading at each sync, not a
Prometheus query, and that every number is a simulator number. Where the run
depends on an approximated trigger's divergence (§3.4), or on a gap of §3.2
(the timeout, the drain, rejections at 0 pods), the report says which, next
to the number it affects.

## Resuming

As run-set.md §5, "Resuming". The plant check: the lab is at the commit the
summary names, and `$V/policy.json` is the one the last report's
`policy.json` came from (the arm aside). Raw results are cheap to rebuild:
rerun a run whose `$RAW` directory is gone with its `command.txt`, and check
the decision logs' sha256 against its `blis-run.json`.
