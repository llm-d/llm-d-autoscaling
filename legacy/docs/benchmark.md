# Benchmark Results

Summary of WVA benchmark runs with configuration details. 

## Environment

| Component | Version / Detail |
|-----------|-----------------|
| **Hardware** | NVIDIA H100 (OpenShift cluster) |
| **Load Generator** | GuideLLM (Poisson profile) |

## EPP Configuration

| Parameter | Default Value | Tuned Value |
|-----------|---------------|-------------|
| Scorer weights | queue=2, kv-cache=2, prefix-cache=3 | TBD |
| Feature gates | flowControl | TBD |

## WVA Configuration

| Parameter | Default | Tuned (prefill heavy) | Tuned (decode heavy) |
|-----------|---------|----------------------|-----------------------|
| **v1 Saturation (spare-based)** | | | |
| KV cache threshold | 0.80 | 0.90 | 0.75 |
| Queue length threshold | 5 | 10 | 3 |
| KV spare trigger | 0.10 | 0.05 | 0.15 |
| Queue spare trigger | 3 | 2 | 5 |
| Enable limiter | false | false | NA |
| Cost factor | 10.0 | 10.0 | 10.0 |
| **v2 Saturation (token-based)** | | | |
| Scale-up threshold | 0.85 | _TBD_ | _TBD_ |
| Scale-down boundary | 0.70 | _TBD_ | _TBD_ |
| Priority | 1.0 | _TBD_ | _TBD_ |
| Analyzer name | saturation | _TBD_ | _TBD_ |
| Analyzer score | 1.0 | _TBD_ | _TBD_ |
| Enable limiter | false | _TBD_ | _TBD_ |
| Cost factor | 10.0 | _TBD_ | _TBD_ |

## HPA Configuration

| Parameter | Value |
|-----------|-------|
| Min replicas | 1 |
| Max replicas | 10 |
| Scale-up stabilization | 0s |
| Scale-up policy | 10 Pods / 150s |
| Scale-down stabilization | 240s |
| Scale-down policy | 10 Pods / 150s |
| Metric source | External (`wva_desired_replicas`) |

## EPP+KEDA Saturation Configuration

| Parameter | Value |
|-----------|-------|
| **EPP** | |
| Scorer weights | Default (queue=2, kv-cache=2, prefix-cache=3) |
| Feature gates | Default (no flowControl) |
| **KEDA ScaledObject** | |
| KV cache target | 700m |
| Queue depth target | 2 |
| Min replicas | 1 |
| Max replicas | 10 |
| Polling interval | 15s |
| Scale-up policy | 1 Pod / 180s, stabilization 0s |
| Scale-down policy | 1 Pod / 300s, stabilization 300s |
| **Benchmark** | |
| Model | Qwen/Qwen3-32B |
| Duration | 600s |


## Prefill Heavy Scenario

### Prefill Heavy — Qwen/Qwen3-32B (600s)

**llm-d Release:** v0.6.0
**Model:** Qwen/Qwen3-32B
**Workload:** 4000 prompt tokens, 1000 output tokens, 20 RPS, 600s duration
**Saturation Engine:** Default(v1), Tuned(v1)

| Metric | WVA v0.6.0 Default(v1) Run 1 | WVA v0.6.0 Default(v1) Run 2 | WVA v0.6.0 Default(v1) Run 3 | Avg | WVA v0.6.0 Tuned(v1) (prefill) |
|--------|------------------------------|------------------------------|------------------------------|-----|--------------------------------|
| P99 TTFT (ms) | 98,810 | 97,811 | 98,638 | 98,420 | _TBD_ |
| P99 ITL (ms/token) | 55.06 | 54.4 | 54.98 | 54.8 | _TBD_ |
| Avg replicas | 1.68 | 1.77 | 1.73 | 1.73 | _TBD_ |
| Max replicas | 3 | 3 | 3 | 3 | _TBD_ |
| Avg KV cache utilization | 65.1% | 69.2% | 64.5% | 66.3% | _TBD_ |
| Avg queue depth (EPP) | 236.8 | 252.4 | 220.4 | 236.5 | _TBD_ |
| Error count | 4,186 | 4,193 | 4,173 | 4,184 | _TBD_ |
| Avg pod startup (s) | 115 | 106 | 109 | 110 | _TBD_ |
| Cost (avg replicas × GPU/hr) | _TBD_ | 1.77 | 1.73 | 1.73 | _TBD_ |

### Prefill Heavy — Qwen/Qwen3-32B (1800s)

**llm-d Release:** v0.6.0
**Model:** Qwen/Qwen3-32B
**Workload:** 4000 prompt tokens, 1000 output tokens, 20 RPS, 1800s duration
**Saturation Engine:** Default(v1)

| Metric | Run 1 | Run 2 | Run 3 | Avg |
|--------|-------|-------|-------|-----|
| P99 TTFT (ms) | 95,293 | 97,622 | 95,409 | 96,108 |
| P99 ITL (ms/token) | 54.29 | 54.15 | 53.94 | 54.13 |
| Avg replicas | 2.58 | 2.53 | 2.53 | 2.55 |
| Max replicas | 4 | 4 | 4 | 4 |
| Avg KV cache utilization | 63.5% | 56.1% | 60.3% | 59.9% |
| Avg queue depth (EPP) | 177.6 | 184.6 | 183.9 | 182.0 |
| Error count | 12,495 | 12,977 | 12,993 | 12,822 |
| Avg pod startup (s) | 105 | 106 | 110 | 107 |
| Cost (avg replicas × GPU/hr) | 2.58 | 2.53 | 2.53 | 2.55 |

### Prefill Heavy — Qwen/Qwen3-32B (HPA Sat V1, 600s)

**Model:** Qwen/Qwen3-32B
**Workload:** 4000 prompt tokens, 1000 output tokens, 20 RPS, 600s duration
**Setup:** HPA Saturation V1 (KV cache target=700m, queue depth target=2, min=1, max=10 replicas, scaleUp=180s, scaleDown=300s) ([#1220](https://github.com/llm-d/llm-d-workload-variant-autoscaler/pull/1220))

| Metric | Run 1 | Run 2 | Run 3 | Avg |
|--------|-------|-------|-------|-----|
| P99 TTFT (ms) | 557,065 | 553,972 | 555,602 | 555,546 |
| P99 ITL (ms/token) | 59.1 | 59.1 | 59.0 | 59.1 |
| Avg replicas | 2.36 | 2.33 | 2.21 | 2.30 |
| Max replicas | 4 | 4 | 4 | 4 |
| Avg KV cache utilization | 47.0% | 47.1% | 45.5% | 46.5% |
| Avg queue depth (EPP) | 119.9 | 119.9 | 122.6 | 120.8 |
| Error count | 0 | 0 | 0 | 0 |
| Avg pod startup (s) | 125 | 127 | 139 | 130 |
| Cost (avg replicas × GPU/hr) | 2.36 | 2.33 | 2.21 | 2.30 |

### Prefill Heavy — Qwen/Qwen3-32B (Static 2 Replicas, 600s)

**llm-d Release:** main (includes v0.7.0 WVA)
**Model:** Qwen/Qwen3-32B
**Workload:** 4000 prompt tokens, 1000 output tokens, 20 RPS, 600s duration
**Setup:** WVA disabled, HPA deleted, 2 constant replicas ([#1139](https://github.com/llm-d/llm-d-workload-variant-autoscaler/issues/1139))

| Metric | Run 1 | Run 2 | Run 3 | Avg |
|--------|-------|-------|-------|-----|
| P99 TTFT (ms) | 553,930 | 552,625 | 559,358 | 555,305 |
| P99 ITL (ms/token) | 58.2 | 57.5 | 58.8 | 58.2 |
| Avg replicas | 2.00 | 2.00 | 2.00 | 2.00 |
| Max replicas | 2 | 2 | 2 | 2 |
| Avg KV cache utilization | 50.6% | 52.6% | 50.8% | 51.3% |
| Error count | 0 | 0 | 0 | 0 |
| Avg pod startup (s) | 94 | 94 | 92 | 93 |

### Prefill Heavy — Qwen/Qwen3-0.6B (600s)

**llm-d Release:** v0.6.0
**Model:** Qwen/Qwen3-0.6B
**Workload:** 4000 prompt tokens, 1000 output tokens, 20 RPS, 600s duration
**Saturation Engine:** Default(v1)

| Metric | Run 1 | Run 2 | Run 3 | Avg |
|--------|-------|-------|-------|-----|
| P99 TTFT (ms) | 86,969 | 79,724 | 77,481 | 81,391 |
| P99 ITL (ms/token) | 50.87 | 53.10 | 51.84 | 51.94 |
| Avg replicas | 1.93 | 1.85 | 2.00 | 1.93 |
| Max replicas | 3 | 3 | 3 | 3 |
| Avg KV cache utilization | 67.3% | 61.9% | 66.2% | 65.1% |
| Avg queue depth (EPP) | 65.9 | 78.9 | 84.8 | 76.5 |
| Error count | 384 | 636 | 182 | 401 |
| Avg pod startup (s) | 65 | 64 | 65 | 65 |
| Cost (avg replicas × GPU/hr) | 1.93 | 1.85 | 2.00 | 1.93 |

### Prefill Heavy — Qwen/Qwen3-0.6B (1800s)

**llm-d Release:** v0.6.0
**Model:** Qwen/Qwen3-0.6B
**Workload:** 4000 prompt tokens, 1000 output tokens, 20 RPS, 1800s duration
**Saturation Engine:** Default(v1)

| Metric | Run 1 | Run 2 | Run 3 | Avg |
|--------|-------|-------|-------|-----|
| P99 TTFT (ms) | 67,747 | 65,054 | 65,731 | 66,177 |
| P99 ITL (ms/token) | 50.51 | 46.66 | 44.57 | 47.25 |
| Avg replicas | 2.70 | 3.27 | 3.54 | 3.17 |
| Max replicas | 5 | 6 | 5 | 5 |
| Avg KV cache utilization | 56.3% | 56.0% | 54.9% | 55.7% |
| Avg queue depth (EPP) | 56.1 | 37.2 | 30.4 | 41.2 |
| Error count | 1,196 | 754 | 629 | 860 |
| Avg pod startup (s) | 67 | 64 | 66 | 66 |
| Cost (avg replicas × GPU/hr) | 2.70 | 3.27 | 3.54 | 3.17 |

### Prefill Heavy — Qwen/Qwen3-32B (EPP+KEDA Saturation, 600s)

**Workload:** 4000 prompt tokens, 1000 output tokens, 20 RPS, 600s duration
**Setup:** See [EPP+KEDA Saturation Configuration](#eppkeda-saturation-configuration)

| Metric | Run 1 | Run 2 | Run 3 | Avg |
|--------|-------|-------|-------|-----|
| P99 TTFT (ms) | 526,612 | 498,804 | 495,336 | 506,917 |
| P99 ITL (ms/token) | 58.83 | 58.29 | 58.92 | 58.68 |
| Avg replicas | 7.41 | 7.37 | 7.49 | 7.42 |
| Max replicas | 10 | 10 | 10 | 10 |
| Avg KV cache utilization | 67.0% | 65.6% | 69.5% | 67.4% |
| Avg queue depth (EPP) | 108.7 | 118.6 | 108.2 | 111.8 |
| Error count | 0 | 0 | 0 | 0 |
| Avg pod startup (s) | 106 | 102 | 104 | 104 |
| Cost (avg replicas × GPU/hr) | 7.41 | 7.37 | 7.49 | 7.42 |

## Decode Heavy Scenario

### Decode Heavy — Qwen/Qwen3-32B (600s)

**llm-d Release:** v0.6.0
**Model:** Qwen/Qwen3-32B
**Workload:** 1000 prompt tokens, 4000 output tokens, 20 RPS, 600s duration
**Saturation Engine:** Default(v1), Tuned(v1)

| Metric | WVA v0.6.0 Default(v1) Run 1 | WVA v0.6.0 Default(v1) Run 2 | WVA v0.6.0 Default(v1) Run 3 | Avg | WVA v0.6.0 Tuned(v1) (decode) |
|--------|------------------------------|------------------------------|------------------------------|-----|-------------------------------|
| P99 TTFT (ms) | 85,612 | 85,397 | 63,144 | 78,051 | _TBD_ |
| P99 ITL (ms/token) | 47.09 | 47.05 | 47.26 | 47.13 | _TBD_ |
| Avg replicas | 1.73 | 1.82 | 1.96 | 1.84 | _TBD_ |
| Max replicas | 3 | 3 | 3 | 3 | _TBD_ |
| Avg KV cache utilization | 88.8% | 78.2% | 70.7% | 79.2% | _TBD_ |
| Avg queue depth (EPP) | 111.8 | 111.5 | 103.1 | 108.8 | _TBD_ |
| Error count | 3,506 | 3,551 | 3,632 | 3,563 | _TBD_ |
| Avg pod startup (s) | 119 | 103 | 106 | 109 | _TBD_ |
| Cost (avg replicas × GPU/hr) | _TBD_ | 1.82 | 1.96 | 1.89 | _TBD_ |

### Decode Heavy — Qwen/Qwen3-32B (1800s)

**llm-d Release:** v0.6.0
**Model:** Qwen/Qwen3-32B
**Workload:** 1000 prompt tokens, 4000 output tokens, 20 RPS, 1800s duration
**Saturation Engine:** Default(v1)

| Metric | Run 1 | Run 2 | Run 3 | Avg |
|--------|-------|-------|-------|-----|
| P99 TTFT (ms) | 70,977 | 70,989 | 70,637 | 70,868 |
| P99 ITL (ms/token) | 45.80 | 46.71 | 45.68 | 46.06 |
| Avg replicas | 2.29 | 2.15 | 2.77 | 2.40 |
| Max replicas | 4 | 3 | 4 | 4 |
| Avg KV cache utilization | 75.3% | 70.7% | 70.5% | 72.2% |
| Avg queue depth (EPP) | 91.2 | 100.6 | 73.4 | 88.4 |
| Error count | 11,041 | 10,178 | 11,067 | 10,762 |
| Avg pod startup (s) | 110 | 106 | 112 | 109 |
| Cost (avg replicas × GPU/hr) | 2.29 | 2.15 | 2.77 | 2.40 |

### Decode Heavy — Qwen/Qwen3-32B (HPA Sat V1, 600s)

**Model:** Qwen/Qwen3-32B
**Workload:** 1000 prompt tokens, 4000 output tokens, 20 RPS, 600s duration
**Setup:** HPA Saturation V1 (KV cache target=700m, queue depth target=2, min=1, max=10 replicas, scaleUp=180s, scaleDown=300s) ([#1220](https://github.com/llm-d/llm-d-workload-variant-autoscaler/pull/1220))

| Metric | Run 1 | Run 2 | Run 3 | Avg |
|--------|-------|-------|-------|-----|
| P99 TTFT (ms) | 356,636 | 357,012 | 356,991 | 356,880 |
| P99 ITL (ms/token) | 113.1 | 113.4 | 113.3 | 113.3 |
| Avg replicas | 2.17 | 2.21 | 2.26 | 2.21 |
| Max replicas | 4 | 4 | 4 | 4 |
| Avg KV cache utilization | 49.7% | 47.4% | 48.8% | 48.6% |
| Avg queue depth (EPP) | 86.0 | 79.8 | 83.6 | 83.1 |
| Error count | 0 | 0 | 0 | 0 |
| Avg pod startup (s) | 131 | 127 | 126 | 128 |
| Cost (avg replicas × GPU/hr) | 2.17 | 2.21 | 2.26 | 2.21 |

### Decode Heavy — Qwen/Qwen3-32B (Static 2 Replicas, 600s)

**llm-d Release:** main (includes v0.7.0 WVA)
**Model:** Qwen/Qwen3-32B
**Workload:** 1000 prompt tokens, 4000 output tokens, 20 RPS, 600s duration
**Setup:** WVA disabled, HPA deleted, 2 constant replicas ([#1139](https://github.com/llm-d/llm-d-workload-variant-autoscaler/issues/1139))

| Metric | Run 1 | Run 2 | Run 3 | Avg |
|--------|-------|-------|-------|-----|
| P99 TTFT (ms) | 357,610 | 356,292 | 355,795 | 356,566 |
| P99 ITL (ms/token) | 113.4 | 114.9 | 113.2 | 113.8 |
| Avg replicas | 2.00 | 2.00 | 2.00 | 2.00 |
| Max replicas | 2 | 2 | 2 | 2 |
| Avg KV cache utilization | 67.0% | 66.3% | 67.0% | 66.8% |
| Error count | 0 | 0 | 0 | 0 |
| Avg pod startup (s) | 91 | 93 | 108 | 97 |

### Decode Heavy — Qwen/Qwen3-0.6B (600s)

**llm-d Release:** v0.6.0
**Model:** Qwen/Qwen3-0.6B
**Workload:** 1000 prompt tokens, 4000 output tokens, 20 RPS, 600s duration
**Saturation Engine:** Default(v1)

| Metric | Run 1 | Run 2 | Run 3 | Avg |
|--------|-------|-------|-------|-----|
| P99 TTFT (ms) | 61,435 | 61,923 | 63,530 | 62,296 |
| P99 ITL (ms/token) | 41.25 | 40.86 | 41.22 | 41.11 |
| Avg replicas | 1.98 | 1.86 | 1.83 | 1.89 |
| Max replicas | 3 | 3 | 3 | 3 |
| Avg KV cache utilization | 61.9% | 61.6% | 61.7% | 61.7% |
| Avg queue depth (EPP) | 58.0 | 49.0 | 46.3 | 51.1 |
| Error count | 1,280 | 1,515 | 1,430 | 1,408 |
| Avg pod startup (s) | 63 | 66 | 66 | 65 |
| Cost (avg replicas × GPU/hr) | 1.98 | 1.86 | 1.83 | 1.89 |

### Decode Heavy — Qwen/Qwen3-0.6B (1800s)

**llm-d Release:** v0.6.0
**Model:** Qwen/Qwen3-0.6B
**Workload:** 1000 prompt tokens, 4000 output tokens, 20 RPS, 1800s duration
**Saturation Engine:** Default(v1)

| Metric | Run 1 | Run 2 | Run 3 | Avg |
|--------|-------|-------|-------|-----|
| P99 TTFT (ms) | 60,654 | 60,983 | 55,166 | 58,934 |
| P99 ITL (ms/token) | 39.61 | 38.00 | 56.65 | 44.75 |
| Avg replicas | 2.65 | 2.24 | 2.88 | 2.59 |
| Max replicas | 4 | 3 | 4 | 4 |
| Avg KV cache utilization | 55.9% | 58.0% | 57.6% | 57.2% |
| Avg queue depth (EPP) | 30.4 | 40.7 | 21.2 | 30.8 |
| Error count | 2,610 | 3,207 | 1,743 | 2,520 |
| Avg pod startup (s) | 64 | 68 | 67 | 66 |
| Cost (avg replicas × GPU/hr) | 2.65 | 2.24 | 2.88 | 2.59 |

### Decode Heavy — Qwen/Qwen3-32B (EPP+KEDA Saturation, 600s)

**Workload:** 1000 prompt tokens, 4000 output tokens, 20 RPS, 600s duration
**Setup:** See [EPP+KEDA Saturation Configuration](#eppkeda-saturation-configuration)

| Metric | Run 1 | Run 2 | Run 3 | Avg |
|--------|-------|-------|-------|-----|
| P99 TTFT (ms) | 327,058 | 448,772 | 448,750 | 408,193 |
| P99 ITL (ms/token) | 104.08 | 108.32 | 108.34 | 106.91 |
| Avg replicas | 10.00 | 7.32 | 7.34 | 8.22 |
| Max replicas | 10 | 10 | 10 | 10 |
| Avg KV cache utilization | 88.7% | 33.3% | 32.3% | 51.4% |
| Avg queue depth (EPP) | 40.4 | 123.2 | 116.7 | 93.4 |
| Error count | 0 | 0 | 0 | 0 |
| Avg pod startup (s) | 106 | 101 | 100 | 102 |
| Cost (avg replicas × GPU/hr) | 10.00 | 7.32 | 7.34 | 8.22 |

## Bursty Scenario

### Bursty — Qwen/Qwen3-32B (900s)

**llm-d Release:** v0.6.0
**Model:** Qwen/Qwen3-32B
**Workload:** ~1000 prompt tokens, ~1000 output tokens, multi-stage bursty RPS (15→2→10→15→5→2), 900s total duration
**Saturation Engine:** Default(v1)

| Metric | Run 1 | Run 2 | Run 3 | Avg |
|--------|-------|-------|-------|-----|
| P99 TTFT (ms) | 264,266 | 257,501 | 265,557 | 262,441 |
| P99 ITL (ms/token) | 196.1 | 210.3 | 182.4 | 196.3 |
| Avg replicas | 2.46 | 2.29 | 2.55 | 2.43 |
| Max replicas | 4 | 4 | 4 | 4 |
| Avg KV cache utilization | 31.5% | 50.9% | 52.9% | 45.1% |
| Avg queue depth (EPP) | 15.3 | 113.0 | 32.3 | 53.5 |
| Error count | 6,230 | 6,021 | 6,079 | 6,110 |
| Avg pod startup (s) | 109 | 101 | 100 | 103 |
| Cost (avg replicas × GPU/hr) | 2.46 | 2.29 | 2.55 | 2.43 |

### Bursty — Qwen/Qwen3-32B (1800s)

**llm-d Release:** v0.6.0
**Model:** Qwen/Qwen3-32B
**Workload:** ~1000 prompt tokens, ~1000 output tokens, multi-stage bursty RPS (15→2→10→15→5→2), 1800s total duration
**Saturation Engine:** Default(v1)
**Harness:** inference-perf

> **Note:** The inference-perf harness did not complete all stages within the
> harness timeout for this model/duration combination. Results are unavailable;
> a re-run with an increased harness timeout is needed.

### Bursty — Qwen/Qwen3-0.6B (600s)

**llm-d Release:** v0.6.0
**Model:** Qwen/Qwen3-0.6B
**Workload:** ~1000 prompt tokens, ~1000 output tokens, multi-stage bursty RPS (15→2→10→15→5→2), 900s total duration
**Saturation Engine:** Default(v1)
**Harness:** inference-perf

| Metric | Run 1 | Run 2 | Run 3 | Avg |
|--------|-------|-------|-------|-----|
| P99 TTFT (ms) | 14,900 | 11,671 | 13,556 | 13,376 |
| P99 ITL (ms/token) | 49.0 | 47.8 | 47.3 | 48.0 |
| Avg replicas | 2.03 | 1.95 | 1.99 | 1.99 |
| Max replicas | 3 | 3 | 3 | 3 |
| Avg KV cache utilization | 36.7% | 34.2% | 34.8% | 35.2% |
| Avg queue depth (EPP) | 16.5 | 17.0 | 14.6 | 16.0 |
| Error count | 54 | 49 | 49 | 51 |
| Avg pod startup (s) | 66 | 65 | 66 | 66 |
| Cost (avg replicas × GPU/hr) | 2.03 | 1.95 | 1.99 | 1.99 |

### Bursty — Qwen/Qwen3-0.6B (1800s)

**llm-d Release:** v0.6.0
**Model:** Qwen/Qwen3-0.6B
**Workload:** ~1000 prompt tokens, ~1000 output tokens, multi-stage bursty RPS (15→2→10→15→5→2), 1800s total duration
**Saturation Engine:** Default(v1)
**Harness:** inference-perf

| Metric | Run 1 | Run 2 | Run 3 | Avg |
|--------|-------|-------|-------|-----|
| P99 TTFT (ms) | 21,925 | 20,680 | 27,228 | 23,278 |
| P99 ITL (ms/token) | 49.8 | 49.1 | 51.4 | 50.1 |
| Avg replicas | 1.62 | 1.66 | 1.62 | 1.63 |
| Max replicas | 3 | 3 | 3 | 3 |
| Avg KV cache utilization | 29.8% | 27.4% | 31.4% | 29.5% |
| Avg queue depth (EPP) | 0.9 | 1.3 | 1.1 | 1.1 |
| Error count | 73 | 68 | 73 | 71 |
| Avg pod startup (s) | 65 | 63 | 64 | 64 |
| Cost (avg replicas × GPU/hr) | 1.62 | 1.66 | 1.62 | 1.63 |

## Symmetrical Scenario

### Symmetrical — Qwen/Qwen3-32B (600s)

**llm-d Release:** v0.6.0
**Model:** Qwen/Qwen3-32B
**Workload:** 1000 prompt tokens, 1000 output tokens, 20 RPS, 600s duration
**Saturation Engine:** Default(v1)

| Metric | WVA v0.6.0 Default(v1) Run 1 | WVA v0.6.0 Default(v1) Run 2 | WVA v0.6.0 Default(v1) Run 3 | Avg |
|--------|------------------------------|------------------------------|------------------------------|-----|
| P99 TTFT (ms) | 101,083 | 99,542 | 99,937 | 100,187 |
| P99 ITL (ms/token) | 67.61 | 67.0 | 67.25 | 67.29 |
| Avg replicas | 1.70 | 1.75 | 1.64 | 1.70 |
| Max replicas | 3 | 3 | 3 | 3 |
| Avg KV cache utilization | 66.7% | 70.2% | 73.7% | 70.2% |
| Avg queue depth (EPP) | 135.1 | 176.7 | 188.6 | 166.8 |
| Error count | 3,773 | 3,710 | 3,705 | 3,729 |
| Avg pod startup (s) | 97 | 107 | 105 | 103 |
| Cost (avg replicas × GPU/hr) | _TBD_ | 1.75 | 1.64 | 1.70 |

### Symmetrical — Qwen/Qwen3-32B (1800s)

**llm-d Release:** v0.6.0
**Model:** Qwen/Qwen3-32B
**Workload:** 1000 prompt tokens, 1000 output tokens, 20 RPS, 1800s duration
**Saturation Engine:** Default(v1)

| Metric | Run 1 | Run 2 | Run 3 | Avg |
|--------|-------|-------|-------|-----|
| P99 TTFT (ms) | 84,261 | 84,223 | 96,175 | 88,220 |
| P99 ITL (ms/token) | 66.56 | 66.26 | 66.38 | 66.40 |
| Avg replicas | 3.59 | 2.76 | 2.78 | 3.05 |
| Max replicas | 6 | 4 | 4 | 5 |
| Avg KV cache utilization | 58.5% | 59.6% | 59.0% | 59.0% |
| Avg queue depth (EPP) | 94.0 | 135.8 | 112.7 | 114.2 |
| Error count | 9,640 | 10,652 | 10,524 | 10,272 |
| Avg pod startup (s) | 100 | 108 | 103 | 103 |
| Cost (avg replicas × GPU/hr) | 3.59 | 2.76 | 2.78 | 3.05 |

### Symmetrical — Qwen/Qwen3-32B (HPA Sat V1, 600s)

**Model:** Qwen/Qwen3-32B
**Workload:** 1000 prompt tokens, 1000 output tokens, 20 RPS, 600s duration
**Setup:** HPA Saturation V1 (KV cache target=700m, queue depth target=2, min=1, max=10 replicas, scaleUp=180s, scaleDown=300s) ([#1220](https://github.com/llm-d/llm-d-workload-variant-autoscaler/pull/1220))

| Metric | Run 1 | Run 2 | Run 3 | Avg |
|--------|-------|-------|-------|-----|
| P99 TTFT (ms) | 517,882 | 518,167 | 518,893 | 518,314 |
| P99 ITL (ms/token) | 70.5 | 70.4 | 70.7 | 70.5 |
| Avg replicas | 2.33 | 2.26 | 2.36 | 2.32 |
| Max replicas | 4 | 4 | 4 | 4 |
| Avg KV cache utilization | 46.2% | 48.3% | 44.3% | 46.3% |
| Avg queue depth (EPP) | 99.0 | 98.5 | 94.8 | 97.4 |
| Error count | 0 | 0 | 0 | 0 |
| Avg pod startup (s) | 124 | 129 | 123 | 125 |
| Cost (avg replicas × GPU/hr) | 2.33 | 2.26 | 2.36 | 2.32 |

### Symmetrical — Qwen/Qwen3-32B (Static 2 Replicas, 600s)

**llm-d Release:** main (includes v0.7.0 WVA)
**Model:** Qwen/Qwen3-32B
**Workload:** 1000 prompt tokens, 1000 output tokens, 20 RPS, 600s duration
**Setup:** WVA disabled, HPA deleted, 2 constant replicas ([#1139](https://github.com/llm-d/llm-d-workload-variant-autoscaler/issues/1139))

| Metric | Run 1 | Run 2 | Run 3 | Avg |
|--------|-------|-------|-------|-----|
| P99 TTFT (ms) | 492,921 | 515,307 | 514,286 | 507,504 |
| P99 ITL (ms/token) | 70.5 | 70.5 | 70.4 | 70.5 |
| Avg replicas | 2.00 | 2.00 | 2.00 | 2.00 |
| Max replicas | 2 | 2 | 2 | 2 |
| Avg KV cache utilization | 50.4% | 48.6% | 48.7% | 49.3% |
| Error count | 0 | 0 | 0 | 0 |
| Avg pod startup (s) | 96 | 97 | 99 | 97 |

### Symmetrical — Qwen/Qwen3-0.6B (600s)

**llm-d Release:** v0.6.0
**Model:** Qwen/Qwen3-0.6B
**Workload:** 1000 prompt tokens, 1000 output tokens, 20 RPS, 600s duration
**Saturation Engine:** Default(v1)

| Metric | Run 1 | Run 2 | Run 3 | Avg |
|--------|-------|-------|-------|-----|
| P99 TTFT (ms) | 22,560 | 24,180 | 22,766 | 23,169 |
| P99 ITL (ms/token) | 44.07 | 43.26 | 42.47 | 43.27 |
| Avg replicas | 1.79 | 1.81 | 1.81 | 1.80 |
| Max replicas | 3 | 3 | 3 | 3 |
| Avg KV cache utilization | 53.1% | 51.9% | 51.1% | 52.0% |
| Avg queue depth (EPP) | 12.2 | 14.0 | 12.8 | 13.0 |
| Error count | 0 | 52 | 0 | 17 |
| Avg pod startup (s) | 62 | 64 | 67 | 64 |
| Cost (avg replicas × GPU/hr) | 1.79 | 1.81 | 1.81 | 1.80 |

### Symmetrical — Qwen/Qwen3-32B (EPP+KEDA Saturation, 600s)

**Workload:** 1000 prompt tokens, 1000 output tokens, 20 RPS, 600s duration
**Setup:** See [EPP+KEDA Saturation Configuration](#eppkeda-saturation-configuration)

| Metric | Run 1 | Run 2 | Run 3 | Avg |
|--------|-------|-------|-------|-----|
| P99 TTFT (ms) | 527,657 | 477,895 | 478,899 | 494,817 |
| P99 ITL (ms/token) | 58.64 | 68.68 | 68.92 | 65.41 |
| Avg replicas | 7.17 | 7.62 | 7.57 | 7.45 |
| Max replicas | 10 | 10 | 10 | 10 |
| Avg KV cache utilization | 64.0% | 68.6% | 66.9% | 66.5% |
| Avg queue depth (EPP) | 129.5 | 90.7 | 93.2 | 104.5 |
| Error count | 0 | 0 | 0 | 0 |
| Avg pod startup (s) | 122 | 98 | 105 | 108 |
| Cost (avg replicas × GPU/hr) | 7.17 | 7.62 | 7.57 | 7.45 |

## Two-Variant Efficiency-Aware Scenario

Runs the [two-variant WVA benchmark](developer-guide/two-variant-wva-benchmark.md):
two TP variants of the same model share one `InferencePool`/EPP, each with its own
`VariantAutoscaling` + HPA.  The WVA V2 saturation engine scales the **most
efficient** variant first (highest serving-capacity per unit cost) and routes
spillover to the cheaper TP=1 secondary.

**Hardware:** NVIDIA H100 (OpenShift cluster)
**Setup:** Primary TP=2 (cost=10, max=10), secondary TP=1 (cost=5, max=10),
WVA V2 saturation engine, HPA scaleUp window=0s

> **Cost formula:** `(avg_primary × 2 + avg_secondary × 1) × GPU/hr` — weights
> each variant by its TP (GPU) count so the cost column is a true GPU-hour proxy.

### Two-Variant — Meta-Llama-3.1-8B-Instruct (Prefill Heavy, 600s)

**llm-d Release:** v0.8.0
**Model:** unsloth/Meta-Llama-3.1-8B-Instruct
**Workload:** 4000 prompt tokens, 1000 output tokens, 15 RPS, 600s duration
**Saturation Engine:** V2 (saturation)

| Metric | Run 1 | Run 2 | Run 3 | Avg |
|--------|-------|-------|-------|-----|
| P99 TTFT (ms) | _TBD_ | _TBD_ | _TBD_ | _TBD_ |
| P99 ITL (ms/token) | _TBD_ | _TBD_ | _TBD_ | _TBD_ |
| Avg primary replicas (TP=2) | _TBD_ | _TBD_ | _TBD_ | _TBD_ |
| Max primary replicas (TP=2) | _TBD_ | _TBD_ | _TBD_ | _TBD_ |
| Avg secondary replicas (TP=1) | _TBD_ | _TBD_ | _TBD_ | _TBD_ |
| Max secondary replicas (TP=1) | _TBD_ | _TBD_ | _TBD_ | _TBD_ |
| Avg KV cache utilization | _TBD_ | _TBD_ | _TBD_ | _TBD_ |
| Avg queue depth (EPP) | _TBD_ | _TBD_ | _TBD_ | _TBD_ |
| Error count | _TBD_ | _TBD_ | _TBD_ | _TBD_ |
| Avg pod startup (s) | _TBD_ | _TBD_ | _TBD_ | _TBD_ |
| Cost (weighted avg replicas × GPU/hr) | _TBD_ | _TBD_ | _TBD_ | _TBD_ |

### Two-Variant — Meta-Llama-3.1-8B-Instruct (Symmetrical, 600s)

**llm-d Release:** v0.8.0
**Model:** unsloth/Meta-Llama-3.1-8B-Instruct
**Workload:** 1000 prompt tokens, 1000 output tokens, 15 RPS, 600s duration
**Saturation Engine:** V2 (saturation)

| Metric | Run 1 | Run 2 | Run 3 | Avg |
|--------|-------|-------|-------|-----|
| P99 TTFT (ms) | _TBD_ | _TBD_ | _TBD_ | _TBD_ |
| P99 ITL (ms/token) | _TBD_ | _TBD_ | _TBD_ | _TBD_ |
| Avg primary replicas (TP=2) | _TBD_ | _TBD_ | _TBD_ | _TBD_ |
| Max primary replicas (TP=2) | _TBD_ | _TBD_ | _TBD_ | _TBD_ |
| Avg secondary replicas (TP=1) | _TBD_ | _TBD_ | _TBD_ | _TBD_ |
| Max secondary replicas (TP=1) | _TBD_ | _TBD_ | _TBD_ | _TBD_ |
| Avg KV cache utilization | _TBD_ | _TBD_ | _TBD_ | _TBD_ |
| Avg queue depth (EPP) | _TBD_ | _TBD_ | _TBD_ | _TBD_ |
| Error count | _TBD_ | _TBD_ | _TBD_ | _TBD_ |
| Avg pod startup (s) | _TBD_ | _TBD_ | _TBD_ | _TBD_ |
| Cost (weighted avg replicas × GPU/hr) | _TBD_ | _TBD_ | _TBD_ | _TBD_ |

### Symmetrical — Qwen/Qwen3-0.6B (1800s)

**llm-d Release:** v0.6.0
**Model:** Qwen/Qwen3-0.6B
**Workload:** 1000 prompt tokens, 1000 output tokens, 20 RPS, 1800s duration
**Saturation Engine:** Default(v1)

| Metric | Run 1 | Run 2 | Run 3 | Avg |
|--------|-------|-------|-------|-----|
| P99 TTFT (ms) | 21,272 | 19,368 | 21,836 | 20,825 |
| P99 ITL (ms/token) | 39.41 | 40.52 | 41.13 | 40.36 |
| Avg replicas | 1.80 | 1.78 | 1.82 | 1.80 |
| Max replicas | 3 | 3 | 3 | 3 |
| Avg KV cache utilization | 46.5% | 48.0% | 45.9% | 46.8% |
| Avg queue depth (EPP) | 8.1 | 11.6 | 12.7 | 10.8 |
| Error count | 359 | 348 | 321 | 342 |
| Avg pod startup (s) | 66 | 67 | 66 | 66 |
| Cost (avg replicas × GPU/hr) | 1.80 | 1.78 | 1.82 | 1.80 |
