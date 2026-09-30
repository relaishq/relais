# Observability Guide: Redis Streams Consumer Groups

This guide explains how to import the provided Grafana dashboard and Prometheus alert rules, and how to validate metrics for Redis Streams consumer groups in Relais.

## Assets

- Grafana dashboard JSON: `docs/observability/grafana/redis-groups.json`
- Prometheus alert rules: `docs/observability/alerts/redis-groups.yml`

## Prometheus Setup

1. Place the alert rules file where Prometheus can read it, and reference it from your configuration:

```yaml
rule_files:
  - /etc/prometheus/rules/relais/redis-groups.yml
```

2. Reload Prometheus to pick up the rules (via `/-/reload` if enabled) or restart the service.

3. Ensure Prometheus scrapes Relais:

```yaml
scrape_configs:
  - job_name: 'relais'
    metrics_path: /metrics
    static_configs:
      - targets: ['relais-host:8080']
```

## Grafana Setup

1. In Grafana, go to Dashboards → Import.
2. Upload `docs/observability/grafana/redis-groups.json`.
3. When prompted, select your Prometheus datasource (variable `DS_PROMETHEUS`).
4. Save the dashboard. It should populate within a few minutes if Prometheus is scraping metrics.

## Validation Checklist

- HTTP metrics endpoint is reachable on the core: `http://<host>:<port>/metrics`.
- Key metric families are present:
  - `relais_redis_group_reads_total{stream}`
  - `relais_redis_group_read_messages_total{stream}`
  - `relais_redis_group_acks_total{stream}`
  - `relais_redis_group_ack_messages_total{stream}`
  - `relais_redis_group_ensure_total{result}`
  - `relais_redis_group_claims_total{result}`
  - `relais_redis_group_claim_messages_total`
- Example queries to try in Grafana/Prometheus:
  - `sum by (stream) (rate(relais_redis_group_reads_total[5m]))`
  - `sum by (result) (rate(relais_redis_group_ensure_total[5m]))`
  - `sum by (result) (rate(relais_redis_group_claims_total[5m]))`
  - `rate(relais_redis_group_claim_messages_total[5m])`

## Alert Rules Overview

- Ensure group errors: fires when `relais_redis_group_ensure_total{result="error"}` has non-zero rate.
- Claim errors: fires when `relais_redis_group_claims_total{result="error"}` has non-zero rate.
- Elevated empty claims: warns when `relais_redis_group_claims_total{result="empty"}` rate is sustained above threshold.

Tune thresholds based on expected traffic patterns and claim behavior.

## Simulating Alerts (Optional)

- Ensure errors: temporarily misconfigure group settings or block Redis permissions to trigger ensure failures.
- Claim errors: bring a consumer down during processing or induce Redis connectivity issues.
- Empty claims: set an aggressive `minIdle` with no eligible pending entries to observe empty claims (warning-level by default).

## Troubleshooting

- Missing metrics:
  - Verify Relais Streams + Groups config (environment variables in `README.md`).
  - Confirm Prometheus is scraping the core’s `/metrics` endpoint.
  - Check Relais logs for storage/Redis errors.
- Grafana panels empty:
  - Ensure the selected time range covers recent activity.
  - Confirm the dashboard is using the correct Prometheus datasource.
