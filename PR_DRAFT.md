Title: fix(saturation): treat missing KV/queue metrics as saturated, not empty

## Summary

Fixes #360.

When a replica's KV cache usage or queue length metric fails to scrape for a
cycle, the collector (`internal/collector/replica_metrics.go`) defaults the
value to 0 so the rest of the pipeline has something to work with. The V1
saturation analyzer (`internal/saturationv1/analyzer.go`) then reads that 0
exactly like a genuinely idle replica: a pod that is actually saturated but
had a transient scrape failure gets counted as non-saturated with full spare
capacity. That can make an unsafe scale-down look safe, or suppress a
scale-up that should have fired — silently, since nothing distinguishes "0"
from "unknown."

## Fix

- `domain.ReplicaMetrics` gains two fields, `KvCacheUsageMissing` and
  `QueueLengthMissing`, set by the collector whenever the corresponding
  Prometheus query returned no value for a pod this cycle. The 0 placeholder
  is still stored (for compatibility with anything already reading the
  numeric fields), but it's now paired with a flag saying whether it's real.
- `saturationv1.Analyzer.analyzeVariant` treats either flag as an automatic
  saturation hit for that replica — conservative-by-default, matching the
  direction of the risk in the bug report (missing data biases toward
  "assume full," which can only cause an extra scale-up or a blocked
  scale-down, never the unsafe direction). The replica is excluded from
  `AvgKvCacheUsage`/`MaxKvCacheUsage` so the 0 placeholder doesn't drag those
  down either.
- No new "excluded replica" bookkeeping was needed: forcing a
  missing-metrics replica into `SaturatedReplicas` naturally drops
  `NonSaturatedCount`, which trips the existing
  `MinNonSaturatedReplicasForScaleDown` gate when too few real replicas
  remain to redistribute load — the same path that already protects a
  single-replica scale-down today.

Two design options were considered (exclude the replica entirely vs. default
it conservatively); the write-up and the case for the conservative default
chosen here are in the issue thread.

Out of scope: `saturation_v2`'s `computeReplicaCapacityFallback` also reads
`rm.KvCacheUsage` directly for its fallback capacity estimate and has the
same theoretical exposure. It's a narrower, already-flagged approximation
(comment in that file acknowledges its unit mismatch caveat), and this PR
doesn't touch it — flagging here so it isn't lost.

## Testing

New regression tests, verified to fail against the pre-fix `analyzeVariant`/
`AnalyzeModelSaturation` (confirmed locally by reverting the analyzer change
and re-running):

- `internal/saturationv1/analyzer_test.go`:
  `TestAnalyzeVariant_MissingMetricsTreatedAsSaturated` — a replica with
  `KvCacheUsageMissing`/`QueueLengthMissing` set is classified saturated
  despite reading 0, and its placeholder is excluded from the usage average.
  `TestAnalyzeModelSaturation_MissingMetricsBlockUnsafeScaleDown` — a
  2-replica model where one replica's metrics are missing reports
  `ScaleDownSafe=false` (pre-fix, it read `true`).
- `internal/collector/replica_metrics_test.go`: new subtests under
  `TestCollectReplicaMetrics_Freshness` verifying `KvCacheUsageMissing`/
  `QueueLengthMissing` are set exactly when the corresponding Prometheus
  query has no value for the pod, and clear when both are present.

```
go test ./internal/saturationv1/... ./internal/collector/... ./internal/domain/...
ok  	.../internal/saturationv1
ok  	.../internal/collector
ok  	.../internal/collector/locator
ok  	.../internal/collector/registration
ok  	.../internal/collector/source
ok  	.../internal/collector/source/pod
ok  	.../internal/collector/source/prometheus
ok  	.../internal/domain
```

`go build ./...` and `go vet ./...` are clean. The full `go test ./...` has
four pre-existing failures unrelated to this change
(`internal/actuator`, `internal/controller`, `internal/controller/indexers`,
`internal/engines/saturation`, `test/e2e`) — all fail in `BeforeSuite` on
missing envtest/kubebuilder binaries or an absent cluster, not on anything
this PR touches.
