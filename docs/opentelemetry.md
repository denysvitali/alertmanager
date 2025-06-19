# OpenTelemetry Configuration for AlertManager

AlertManager supports OpenTelemetry distributed tracing to provide observability into alert processing, notifications, and cluster coordination. This document describes how to configure and use tracing.

## Environment Variables

AlertManager uses the standard OpenTelemetry environment variables for configuration:

### Basic Configuration

| Environment Variable | Description | Default |
|---------------------|-------------|---------|
| `OTEL_SERVICE_NAME` | Service name for traces | `alertmanager` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP endpoint URL | None (tracing disabled) |
| `OTEL_RESOURCE_ATTRIBUTES` | Additional resource attributes | None |
| `OTEL_SDK_DISABLED` | Completely disable OpenTelemetry SDK | `false` |

### Exporter Configuration

| Environment Variable | Description | Example |
|---------------------|-------------|---------|
| `OTEL_EXPORTER_OTLP_HEADERS` | Headers for OTLP requests | `api-key=your-key` |
| `OTEL_EXPORTER_OTLP_TIMEOUT` | Request timeout | `30s` |
| `OTEL_EXPORTER_OTLP_COMPRESSION` | Compression algorithm | `gzip` |

### Sampling Configuration

| Environment Variable | Description | Example |
|---------------------|-------------|---------|
| `OTEL_TRACES_SAMPLER` | Sampling strategy | `traceidratio` |
| `OTEL_TRACES_SAMPLER_ARG` | Sampler argument | `0.1` (10% sampling) |

## Command Line Options

Enable or disable tracing using the `--tracing.enabled` flag:

```bash
# Enable tracing (requires OTEL_EXPORTER_OTLP_ENDPOINT)
./alertmanager --tracing.enabled=true

# Disable tracing (default)
./alertmanager --tracing.enabled=false

# Completely disable OpenTelemetry SDK (overrides --tracing.enabled)
OTEL_SDK_DISABLED=true ./alertmanager
```

## Examples

### Example 1: Jaeger

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT="http://jaeger:14268/api/traces"
export OTEL_SERVICE_NAME="alertmanager"
export OTEL_TRACES_SAMPLER="traceidratio"
export OTEL_TRACES_SAMPLER_ARG="0.1"

./alertmanager --tracing.enabled=true
```

### Example 2: OTLP over gRPC

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT="http://otel-collector:4317"
export OTEL_EXPORTER_OTLP_PROTOCOL="grpc"
export OTEL_SERVICE_NAME="alertmanager"
export OTEL_RESOURCE_ATTRIBUTES="deployment.environment=production,service.version=v0.27.0"

./alertmanager --tracing.enabled=true
```

### Example 3: OTLP over HTTP with Authentication

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT="https://api.honeycomb.io"
export OTEL_EXPORTER_OTLP_HEADERS="x-honeycomb-team=your-api-key"
export OTEL_SERVICE_NAME="alertmanager"
export OTEL_TRACES_SAMPLER="always_on"

./alertmanager --tracing.enabled=true
```

### Example 4: Docker Compose

```yaml
version: '3.8'
services:
  alertmanager:
    image: prom/alertmanager:latest
    command:
      - '--config.file=/etc/alertmanager/alertmanager.yml'
      - '--tracing.enabled=true'
    environment:
      - OTEL_EXPORTER_OTLP_ENDPOINT=http://jaeger:14268/api/traces
      - OTEL_SERVICE_NAME=alertmanager
      - OTEL_TRACES_SAMPLER=traceidratio
      - OTEL_TRACES_SAMPLER_ARG=0.1
    depends_on:
      - jaeger

  jaeger:
    image: jaegertracing/all-in-one:latest
    ports:
      - "16686:16686"
      - "14268:14268"
```

## Trace Spans

AlertManager creates spans for the following operations:

### Alert Processing
- `alert.ingestion` - Alert ingestion via API
- `alert.validation` - Alert validation
- `alert.routing` - Routing decisions through the routing tree
- `alert.grouping` - Alert grouping and batching
- `alert.inhibition` - Inhibition rule processing

### Notifications
- `notification.{type}.send` - Individual notification attempts
- `notification.template.render` - Template rendering
- `notification.retry` - Retry logic and backoff

### Silence Management
- `silence.mutes` - Checking if alerts are silenced
- `silence.set` - Creating or updating silences

### Cluster Operations
- `cluster.join` - Joining the cluster
- `cluster.peer_join` - Peer joining the cluster
- `cluster.peer_leave` - Peer leaving the cluster
- `cluster.notify_msg` - Gossip message handling
- `cluster.get_broadcasts` - Broadcast message retrieval

### HTTP Handlers
- `http.{handler}` - HTTP request handling with method and status

## Trace Attributes

Spans include relevant attributes for filtering and analysis:

| Attribute | Description | Example |
|-----------|-------------|---------|
| `alert.name` | Alert name | `HighCPU` |
| `alert.count` | Number of alerts | `5` |
| `notification.receiver` | Receiver name | `team-frontend` |
| `notification.type` | Notification type | `slack` |
| `http.method` | HTTP method | `POST` |
| `http.status_code` | HTTP status code | `200` |
| `cluster.peer.address` | Peer IP address | `10.0.1.5:9094` |

## Performance Impact

When enabled, tracing adds minimal overhead:
- CPU: < 1% in typical scenarios
- Memory: ~10MB for trace buffering
- Network: Depends on sampling rate and trace export frequency

Use sampling (`OTEL_TRACES_SAMPLER=traceidratio`) in high-traffic environments.

## Troubleshooting

### Common Issues

1. **No traces appearing**
   - Verify `OTEL_EXPORTER_OTLP_ENDPOINT` is set and reachable
   - Check that `--tracing.enabled=true` is set
   - Ensure `OTEL_SDK_DISABLED` is not set to `true`
   - Verify the exporter endpoint accepts OTLP format

2. **High resource usage**
   - Reduce sampling rate: `OTEL_TRACES_SAMPLER_ARG=0.01`
   - Increase export interval if supported by your exporter
   - Consider setting `OTEL_SDK_DISABLED=true` to completely disable tracing

3. **Missing spans**
   - Check for sampling - some traces may be dropped
   - Verify the operation is actually being triggered

4. **Completely disable tracing**
   - Set `OTEL_SDK_DISABLED=true` to override all other tracing settings
   - This disables the OpenTelemetry SDK entirely

### Debug Logging

Enable debug logging to see tracing initialization:

```bash
./alertmanager --log.level=debug --tracing.enabled=true
```

Look for log messages like:
```
level=INFO msg="Initializing OpenTelemetry tracing"
level=INFO msg="OpenTelemetry tracing initialized"
```

### Testing Configuration

Test your OTLP endpoint manually:

```bash
curl -X POST ${OTEL_EXPORTER_OTLP_ENDPOINT} \
  -H "Content-Type: application/x-protobuf" \
  -d ""
```

## Integration Examples

### Grafana + Tempo

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT="http://tempo:3200/v1/traces"
export OTEL_SERVICE_NAME="alertmanager"
```

### New Relic

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT="https://otlp.nr-data.net:4317"
export OTEL_EXPORTER_OTLP_HEADERS="api-key=YOUR_LICENSE_KEY"
```

### Datadog

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT="https://api.datadoghq.com"
export OTEL_EXPORTER_OTLP_HEADERS="dd-api-key=YOUR_API_KEY"
```

For more information on OpenTelemetry configuration, see the [OpenTelemetry documentation](https://opentelemetry.io/docs/).
