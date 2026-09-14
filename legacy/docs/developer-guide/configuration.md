## Unified Configuration System

WVA uses a unified configuration system that consolidates all settings into a single `Config` structure. This provides clear precedence rules, type safety, and separation between static (immutable) and dynamic (runtime-updatable) configuration.

### Configuration Structure

The unified `Config` consists of two parts:

1. **StaticConfig**: Immutable settings loaded at startup (require controller restart to change)
   - Infrastructure settings (metrics/probe addresses, leader election)
   - Connection settings (Prometheus URL, TLS certificates)
   - Feature flags

2. **DynamicConfig**: Runtime-updatable settings (can be changed via ConfigMap updates)
   - Optimization interval
   - Saturation scaling thresholds
   - Scale-to-zero configuration
   - GPU limiter selection (saturation ConfigMap `limiters:` list)
   - Prometheus cache settings

### GPU limiter

The GPU resource limiter is selected by the `limiters:` list on the saturation
ConfigMap's `default` entry (there is no CLI flag or env var):

- no `limiters:` list, or a `{type: gpu-inventory}` entry — caps scaling at the
  physically discovered GPU inventory (the default).
- a `{type: quota, ...}` entry — enforces operator-declared per-accelerator-type
  caps at cluster or namespace scope, declared inline.

This is **DynamicConfig**: the engine rebuilds the limiter when the ConfigMap
changes, so edits apply **without a restart**. See the
[Quota Limiter guide](./quota-limiter.md) for the configuration schema, scopes,
validation rules, and pipeline behavior.
