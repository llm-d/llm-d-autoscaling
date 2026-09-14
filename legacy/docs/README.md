# Workload-Variant-Autoscaler Documentation

Welcome to the WVA documentation! This directory contains comprehensive guides for users, developers, and operators.

## Documentation Structure

### User Guide


- **[Installation Guide](https://llm-d.ai/docs/guides/workload-autoscaling)** - Installing WVA on your cluster
- **[Configuration](https://llm-d.ai/docs/architecture/advanced/autoscaling/workload-variant-autoscaling#configuration)** - Configuring WVA for your workloads
- **[Architecture](https://llm-d.ai/docs/architecture/advanced/autoscaling)** - Understanding how WVA works under the hood

### Design

- **[Modeling & Optimization](design/modeling-optimization.md)** - Queue theory models and optimization algorithms
- **[Controller Behavior](design/controller-behavior.md)** - Event handling and reconciliation behavior (outdated)
- **[Architecture Diagrams](https://llm-d.ai/docs/architecture/advanced/autoscaling/workload-variant-autoscaling#design)** - System architecture and workflows
- **[Unified Configuration System](developer-guide/configuration.md)** - Configuration reference for all WVA components
- **[Metrics & Health Monitoring](developer-guide/metrics-health-monitoring.md)** - Exposed metrics and health check endpoints
- **[Saturation Scaling Configuration](developer-guide/saturation-scaling-config.md)** - Tuning the saturation-based scaling algorithm
- **[Quota Limiter](developer-guide/quota-limiter.md)** - Operator-declared per-accelerator GPU caps (cluster/namespace scope)
- **[Throughput Analyzer](developer-guide/throughput-analyzer.md)** - How the throughput analyzer works
- **[Queue Model Analyzer](developer-guide/slo-queuemodel.md)** - SLO-aware queueing model
- **[Pod Scraping Source](developer-guide/pod-scraping-source.md)** - Direct pod metric scraping
- **[Prometheus Integration](developer-guide/prometheus.md)** - Prometheus metrics and configuration

### Developer Guide

- **[Development Setup](developer-guide/development.md)** - Setting up your dev environment
- **[Testing](developer-guide/testing.md)** - Running tests and CI workflows
- **[Debugging](developer-guide/debugging.md)** - Debugging techniques and tools
- **[Contributing](../CONTRIBUTING.md)** - How to contribute to the project

### Benchmark Guide

- **[Benchmark Guide](developer-guide/benchmark-guide.md)** - Running WVA scaling benchmarks
- **[The `run-benchmark` Skill](developer-guide/run-benchmark-skill.md)** - Claude Code skill that drives the `benchmark/` KEDA test bed lifecycle

## Quick Links

- [Main README](../README.md)
- [Kubernetes Deployment](../deploy/kubernetes/README.md)
- [OpenShift Deployment](../deploy/openshift/README.md)
- [Local Development with Kind Emulator](../deploy/kind-emulator/README.md)


## Need Help?

- Check the [Troubleshooting Guide](developer-guide/troubleshooting.md)
- Open a [GitHub Issue](https://github.com/llm-d/llm-d-workload-variant-autoscaler/issues)
- Join community meetings
