---
name: blis-evaluation
description: Run step 3 of this repo's autoscaling evaluation protocol (benchmark/protocol/) — evaluate a KEDA scenario in blis-signal-lab (BLIS with a KEDA + HPA autoscaler) on a laptop, with the same fixed run set, reports and gate as the GPU evaluation (knee staircase, SO-active staircase, mix-shift, overload under and over the cap, agentic, static-replica baseline, bursty, short spikes, scale-down). Translates the ScaledObject's triggers into blis signals, runs each load, converts blis's output to llm-d-benchmark's layout, and writes one report per run (a per-stage table and a load-vs-supply figure) and a summary. Use when asked to simulate a scaling configuration, to evaluate a scenario with blis or without GPUs, to rehearse step 5, or to try a trigger change before a GPU run. Invoke with /blis-evaluation <scenario.yaml> [blis scenario] [--lab <blis-signal-lab checkout>].
---

# blis evaluation

**Arguments:** `$ARGUMENTS`: a single-pool scenario under
`benchmark/config/scenarios/`, optionally a BLIS scenario file name from
`<lab>/testdata/kernel/aisimulate/` with the scenario's model (default: the
one matching the scenario's GPU overlay, 3-blis-evaluation.md §3.3), and optionally
`--lab <dir>` (default: `$TMPDIR/src/blis-signal-lab`).

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
  ScaledObject and the hand-written trigger terms;
- `scripts/blis_run.py` runs one load with an arm and leaves the run in
  llm-d-benchmark's layout, with the trigger series from the HPA log.

The loads (`gen_load.py --target blis`) and the per-stage table
(`stage_table.py`) are shared with step 5, in `benchmark/protocol/scripts/`.

Nothing here touches a cluster. Every write is local: the lab checkout,
`benchmark/results/` (not committed) and the verdicts.

## Step 1: The lab

```bash
LAB=${LAB:-$TMPDIR/src/blis-signal-lab}
[ -d $LAB ] || git clone https://github.com/vishakha-ramani/blis-signal-lab $LAB
git -C $LAB checkout 727e998            # the commit 3-blis-evaluation.md §3.2 describes
(cd $LAB && GOTOOLCHAIN=auto go build -o blis main.go)
```

In the Claude Code sandbox, set `GOPATH`, `GOMODCACHE` and `GOCACHE` under
`$TMPDIR`, and allow `proxy.golang.org`, `sum.golang.org` and
`storage.googleapis.com`. If the checkout is at another commit, say so: §3.2
may no longer hold, and the summary's header names the commit used.

## Step 2: Render, translate, write the policy

1. Render the scenario with `llmdbenchmark standup --dry-run` (the
   `run-benchmark` skill's setup) against its GPU overlay, in a scratch
   workspace. The ScaledObject is the `plan/<stack>/*.yaml` file with
   `kind: ScaledObject` (`grep -l 'kind: ScaledObject' plan/<stack>/*.yaml`).
   Read it: placeholders resolved, the thresholds intended. A ScaledObject
   with `scalingModifiers`, or a scenario with more than one scaled pool,
   stops here (§3.2): say so.
2. Choose the BLIS scenario (§3.3). Its model must be the scenario's
   (`model:` in the rendered `plan/<stack>/config.yaml`); never substitute
   another model. blis names a model by its Hugging Face id without the
   organization, lowercased (`Qwen/Qwen3-8B` is `qwen3-8b`):

   ```bash
   grep -l "^model: qwen3-8b$" $LAB/testdata/kernel/aisimulate/*.yaml
   ```

   If no file has it, write the summary
   `benchmark/protocol/verdicts/<guide>/<scenario>-step3.md` with
   **missing model**: the model, the lab commit, the directory searched,
   and every run *not run*. Commit it, tell the user, and stop.
   Otherwise choose the cold start (§3.3) and record both with their source.
3. Write `terms.json` (§3.4), one entry per trigger, in the evaluation's
   directory `benchmark/protocol/verdicts/<guide>/<scenario>-step3/`. Class
   each trigger exact or approximated, and for an approximated one, write
   where it diverges and which runs that affects.
4. Write the policy:

   ```bash
   B=.claude/skills/blis-evaluation/scripts
   V=benchmark/protocol/verdicts/<guide>/<scenario>-step3
   python3 -I $B/so_to_policy.py --scaledobject <plan>/<keda yaml> --terms $V/terms.json \
     --cold-start <s> --out $V/policy.json
   ```

   It prints what it changed (`minReplicaCount` 0 runs as 1, a missing
   `behavior`, KEDA fields blis does not model); copy each note into the
   summary.
5. Create the summary `benchmark/protocol/verdicts/<guide>/<scenario>-step3.md`:
   the shared header (blis-signal-lab commit, BLIS scenario and any GPU type
   or parallelism it does not share with the overlay, cold start and source, the run set's inputs), the
   translation table (§3.4), the gaps of §3.2 that apply, and the run table
   with every run *pending*, run 7 *not run* (blis does not scale from zero).
   Commit it.

## Step 3: Run each run

`S=benchmark/protocol/scripts`, `RAW=benchmark/results/step3-<scenario>-<YYYYMMDD>`.
Write the knee loads with `--target blis` (run-set.md §5); for an estimate,
use a short probe (a knee at a guessed rate, read with `stage_table.py`, then
the real one centered on what it shows). Each run:

```bash
python3 -I $B/blis_run.py --lab $LAB --scenario <blis scenario> --load $LOAD --run <run> \
  --policy $V/policy.json --so-name <scaledobject name> --out $RAW/<run> \
  --pin 1                                # knees; run 6: --pin <peak Ready of run 3>;
                                         # runs 2-5, 8-10: --active
```

It prints the fates (completed, timed out, dropped, unfinished). The
per-request records, stage windows, replica and HPA series are in
`$RAW/<run>/results/`, the trigger series in `$RAW/<run>/triggers/`.

1. **The first knee gates the translation** (3-blis-evaluation.md §3.5):
   read `$RAW/<run>/sim/signals-hpa.csv`; every trigger's `<name>_value`
   leaves 0, and each exact trigger reads what its query would for the load.
   Rerun it once with the command in `sim/command.txt` and compare the HPA
   log's sha256 with `blis-run.json` (§3.7).
2. **After the knees**, read μ_SLO with `stage_table.py --knee X`
   (run-set.md §5; no `--trace`: the clock comes from `blis-run.json`). A
   capacity-derived threshold is applied by rewriting the policy with
   `so_to_policy.py --threshold <trigger>=<value>`, and recorded. Write the
   other runs' loads with `--target blis` and the lengths from the policy's
   behavior and cold start (run-set.md §2).
3. **Gate every run**: run-set.md §4 and 3-blis-evaluation.md §3.7. A failed
   gate is a tool or input error: fix it, rerun, and list the failed attempt
   in the summary.

## Step 4: Report each run as it finishes

Follow run-set.md §5, "Reporting each run", with `$RAW/<run>/results` as the
results directory, `--deployment blis-decode` for the figure, and, in item 1,
these files into `$V/<run>/`:

```bash
cp $RAW/<run>/sim/{policy.json,workload.yaml,signals-hpa.csv,signals-summary.json,command.txt} \
   $RAW/<run>/results/blis-run.json $V/<run>/
cp -r $RAW/<run>/triggers $V/<run>/
```

The report header adds what 3-blis-evaluation.md §3.6 lists. The caption
says that the trigger panel is the HPA's reading at each sync, not a
Prometheus query, and that every number is a simulator number. Where the run
depends on an approximated trigger's divergence (§3.4), or on a gap of §3.2
(the timeout, the drain, the pipeline delay), the report says which, next to
the number it affects.

## Resuming

As run-set.md §5, "Resuming". The plant check: the lab is at the commit the
summary names, and `$V/policy.json` is the one the last report's
`policy.json` came from (the arm aside). Raw results are cheap to rebuild:
rerun a run whose `$RAW` directory is gone with its `command.txt`, and check
the HPA log's sha256 against its `blis-run.json`.
