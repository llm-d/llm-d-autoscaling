# Monitoring

WVA's user-facing monitoring assets — the operational and benchmark Grafana
dashboards, the alerting rules, and the setup instructions that go with them —
live in the [llm-d](https://github.com/llm-d/llm-d) repository alongside the
rest of the deployment documentation:

- [Observability overview](https://github.com/llm-d/llm-d/blob/main/docs/operations/observability/README.md)
- [Setup](https://github.com/llm-d/llm-d/blob/main/docs/operations/observability/setup.md)
- [Metrics](https://github.com/llm-d/llm-d/blob/main/docs/operations/observability/metrics.md)
- [Alerting](https://github.com/llm-d/llm-d/blob/main/docs/operations/observability/alerting.md)

The dashboard and Prometheus stack that this repository installs are for
development and CI, not for production clusters. They are documented in
[developer-guide/monitoring.md](../developer-guide/monitoring.md).
