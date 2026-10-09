# Proposal: GPU-free overprovisioning

**Goal.** Cut the time between "KEDA decided to scale up" and "a replica serves
tokens", without holding an idle GPU to do it.

**Idea in one line.** Keep a small pool of **buffer pods** that have already
paid every startup cost that does not need a GPU, and hold **no GPU at all**
while they wait.

## 1. The problem

Scaling up a vLLM replica is slow, and tuning only goes so far. On
`Qwen/Qwen3-32B` (61 GiB, 17 shards) on one H100 with weights on GPFS, a tuned
stack reaches `/health` in **~37s** against **90.65s** for vLLM's defaults — and
stops there ([llm-d-cold-start]). What remains resists tuning: the weight load is
**storage-bound** (61 GiB at ~10 GiB/s; TP=2 does *not* halve it, both ranks
contend for one GPFS client), and CUDA graph capture costs **11.4s — 26% of time
to ready** — which no configuration removes without making steady-state inference
worse.

The useful question is not how long the boot takes but **how much of it needs a
GPU**. Measured by parking a real instance at TP=1 and TP=2:

![Cold-start timeline split at the point the GPU is allocated: pod startup and
vLLM's 16–20s warm boot need no GPU, while the 20–27s GPU phase does.](images/cold-start-gpu-boundary.svg)

**About 40% of the wait needs no GPU at all**, and the pod-level costs on the left
are *excluded* from the ~37s above — on a node that has not run this image before,
the left side is larger still. A pod that has already done its warm boot, holding
no GPU, can reach serving in the right-hand span alone.

Today an operator picks a bad trade:

| option | cost |
|---|---|
| scale to zero / low replica floor | every burst pays the full cold start |
| always-hot spare replicas | idle GPUs — the most expensive resource in the cluster |
| vLLM sleep mode / `cuda-checkpoint` | wakes in 1.45–5.09s, but keeps 61–75 GiB of pinned host RAM per replica, **and never returns the device**: `nvidia.com/gpu: "1"` owns it for the pod's lifetime. TP>1 deadlocks. |

Sleep mode is warm standby, not overprovisioning: the blocker is Kubernetes, not
CUDA.

## 2. The mechanism: park before CUDA

The split above is not a natural pause — something has to hold the boot there. The
[handbrake] is injected into the pod and halts vLLM at the **last instruction before
any CUDA context exists**: all the GPU-free work above it is done, and no GPU worker
has been created yet. A pod parked there holds **zero GPU memory** — verified with
`nvidia-smi`, and by a second pod taking the device while the first sits parked. A
resume call releases the barrier in milliseconds and the GPU phase runs.

Where the barrier sits matters twice over. A process that has not yet touched CUDA
can start its GPU workers the cheap way, worth another ~5–6s. And it is the last
moment at which **the GPU this replica will use can still be chosen** — which is
what makes late device assignment possible at all.

It is injected at start-up rather than built in: **no vLLM fork, no image
rebuild.**

## 3. Architecture

![Four components: KEDA sizes the Deployment to active pods plus a buffer; a new
router screener keeps traffic off the buffer pods; a new handbrake controller labels,
relocates, promotes and re-parks them against per-node GPU leases.](images/handbrake-architecture.svg)

Four components, deliberately ignorant of each other:

| component | decides | status |
|---|---|---|
| **KEDA** | how many replicas exist, from demand + buffer size | exists; needs a `scalingModifiers` blueprint |
| **llm-d-router** | that buffer pods get no traffic | **new plugin — work to be done** |
| **handbrake** | when vLLM stops, and on which device it resumes | prototyped in [llm-d-cold-start] |
| **handbrake controller** | which pods are buffers, where they sit, when they are promoted | **new — modelled on [gpu-lease]** |

**KEDA.** A single formula sizes the Deployment: the replicas the current load
needs, plus the buffer. Both terms are in replicas, so the buffer passes through
exactly — ask for two buffer pods and you get two. The buffer size is read from the
controller rather than written into the formula, so the annotation on the Deployment
stays the only place it is set. One constraint matters: scale-down has to be bounded
so a single step cannot delete past the buffer and take a pod that is serving.

**Router plugin.** A buffer pod is running and ready — that is the point — so the
router will offer it traffic unless told otherwise. The exclusion has to be
unconditional, applying to every routing path rather than one of them, and it keys
off the label the controller puts on the pod.

**Handbrake controller.** Its job is the buffer pool, not the replica count:

- label/annotate which pods are buffers, so Services, the EPP screener and
  Prometheus can select on them;
- keep buffers where they can be promoted — periodically **relocate** them onto
  nodes with enough free GPUs (delete + recreate; a pod cannot move);
- **promote**: take a GPU lease for one specific node, then resume the handbrake
  on the leased device;
- **re-park**: when a lease is revoked, return the pod to the buffer pool;
- bias scale-down away from the pods that are serving.

**How it stays in step with KEDA.** The controller never sees demand, and does not need
to. KEDA has already folded demand into a single number — the Deployment's replica count
— so the controller reads that, subtracts the buffer size it already owns, and treats
the difference as its target for *active* pods. It promotes or re-parks until that target
is met. Buffers are the remainder; nothing counts them directly, and the two components
exchange nothing but the replica count and the buffer size.

Because there is one manifest, **every pod is born parked**. A scale-up therefore splits
in two: the replica count rises, so the controller promotes a pod that is *already warm*
— the fast path, and the entire point of the design — while the Deployment separately
creates one more pod, which warms up in the background to refill the pool. Only the
first step is on the critical path. Going down is the mirror image, except that the
replica count alone would let the Deployment delete a pod that is serving, which is why
the controller marks active pods as expensive to remove.

The two can fall out of step briefly, and that is the real cost. If a burst consumes the
last buffer pod, the pool is empty until its replacement finishes booting, and a second
burst inside that window pays the full cold start. Buffer sizing is that question and no
other: how many bursts to absorb per replenishment window. If the cluster has no free
GPU to lease, promotion simply does not happen and the pod stays parked — a shortfall
shows up as a pod that is warm but not serving, rather than one stuck Pending.

**Leases, not GPU requests.** No pod here requests `nvidia.com/gpu`. Every
container sees every device on its node, and the lease is what says which one it may
use; the handbrake reads that at resume. The lease system is the GPU resource manager
in this design — it replaces the usual request-and-attach path rather than sitting on
top of it, and the per-node budget can live in the controller itself. What the budget
never does is gate an application pod: a buffer pod becomes ready immediately, with no
GPU and no queue wait.

### Why not DRA?

Late device assignment is the one place this design steps outside Kubernetes, so the
fair question is whether Dynamic Resource Allocation already solves it. It does not,
for a specific reason: both ordinary device requests and DRA resolve a device while the
pod is being *scheduled*, and a pod's list of resource claims is immutable once the pod
exists — so neither can give a running pod a device it did not start with, which is
exactly what promotion needs. A DRA claim is
its own object and can outlive any single pod, but nothing allocates it except the
scheduler placing a pod that consumes it. So the lease names a device instead, and the
pod is trusted to honour it.

DRA is approaching the same problem from the other side: *device binding conditions*
(beta) already let an external controller perform a slow attach while the scheduler
waits on it — the right shape, still anchored at scheduling. If that ever reaches
running pods, the lease becomes redundant; a reason to keep it narrow.

### Compatible with Kueue

Kueue is a **separate concern** — nothing above needs it. But clusters that run Kueue
already express their per-node GPU budget there, and this design composes with that
budget rather than competing with it; [gpu-lease] shows how.

The trick is *what* gets queued. A lease is a standalone Kueue `Workload` with **no pod
behind it**, pinned to one node, holding one unit of GPU quota. The controller creates
it only when it decides to promote, waits for admission, and only then resumes the pod
— if the node's budget is exhausted, admission never comes and the promotion does not
happen. So Kueue gates the **lease, not the pod**: it enforces the shared per-node GPU
budget without ever knowing a Deployment exists. Admitting the pods themselves would
put every replica in a queue just to start, which is exactly what a buffer pool cannot
afford.

## 4. Costs and open questions

1. **Kubernetes stops accounting for GPUs on these nodes.** Since nothing requests
   a GPU, the scheduler cannot see the devices are in use, so the lease system has to
   be the *only* thing allocating them — an ordinary GPU workload landing on the same
   node would double-book a device. Enforcement is cooperative for the same reason: a
   pod that ignored its lease could touch a device it was not granted. [gpu-lease]
   proves the control plane on a GPU-less cluster; both points need settling before a
   real-GPU run.
2. **Re-park is a restart, not a sleep.** vLLM cannot un-boot. Re-parking costs
   one warm boot (~17s), off the critical path.
3. **Buffer pods are not free** — they cost host RAM, CPU and PVC bandwidth. The
   per-buffer-pod host RAM needs measuring before a sizing recommendation.
4. **Relocation churn.** Every relocation is a delete + recreate. The controller
   needs a rate limit and a reason metric.
5. **Promotion overshoot.** A promoting pod is not yet serving, so queue-based
   triggers may add replicas for a backlog that promotion is about to absorb.
   Subtracting the pods that are mid-promotion from the demand term corrects it.

## 5. Plan

1. **Buffer screener** in llm-d-router, plus the pod label contract.
2. **KEDA blueprint** with the buffer term, as a staging variant beside
   `baseline.yaml`.
3. **Handbrake controller** — buffer labelling, relocation, promote/re-park over
   the GPU-lease state machine.
4. **Handbrake hardening** — device injection at resume, re-park path.
5. **Measure**, on real GPUs: time from KEDA's scale-up decision to first token,
   buffer pods vs. cold start, at equal GPU budget.

Evaluation runs in the `benchmark/` test bed: the variant lands in
`benchmark/config/scenarios/staging/<guide>/` next to `baseline.yaml`, and the
report is the comparison.

[llm-d-cold-start]: https://github.com/llm-d/llm-d-cold-start
[handbrake]: https://github.com/llm-d/llm-d-cold-start/blob/main/docs/new-launcher-design.md
[gpu-lease]: https://github.com/llm-d-extensions/gpu-lease
