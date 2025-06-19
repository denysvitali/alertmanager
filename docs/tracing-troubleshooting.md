# OpenTelemetry Tracing Troubleshooting Guide

This guide helps diagnose and resolve common issues with OpenTelemetry tracing in AlertManager.

## Quick Diagnostics

### 1. Check if Tracing is Enabled

Look for these log messages during AlertManager startup:

```bash
# Tracing enabled and working
level=INFO msg="Initializing OpenTelemetry tracing"
level=INFO msg="OpenTelemetry tracing initialized"

# Tracing disabled
level=INFO msg="OpenTelemetry tracing disabled"

# Tracing enabled but exporter failed
level=WARN msg="Failed to create trace exporter, using no-op tracer" error="..."
```

### 2. Verify Configuration

Check your environment variables:
```bash
echo $OTEL_EXPORTER_OTLP_ENDPOINT
echo $OTEL_SERVICE_NAME
echo $OTEL_TRACES_SAMPLER
```

Check command line flags:
```bash
ps aux | grep alertmanager | grep -o '\--tracing.enabled[=a-z]*'
```

## Common Issues

### Issue: No Traces Appearing

**Symptoms:**
- AlertManager starts successfully
- No traces in your tracing backend (Jaeger, Tempo, etc.)
- No error messages in logs

**Possible Causes & Solutions:**

1. **Tracing not enabled**
   ```bash
   # Check if --tracing.enabled=true is set
   ./alertmanager --tracing.enabled=true
   ```

2. **Missing OTLP endpoint**
   ```bash
   export OTEL_EXPORTER_OTLP_ENDPOINT="http://your-endpoint:4318"
   ```

3. **Sampling set to 0**
   ```bash
   # Check sampling configuration
   export OTEL_TRACES_SAMPLER=always_on
   # OR for probabilistic sampling
   export OTEL_TRACES_SAMPLER=traceidratio
   export OTEL_TRACES_SAMPLER_ARG=1.0  # 100% for testing
   ```

4. **Network connectivity issues**
   ```bash
   # Test endpoint reachability
   curl -v $OTEL_EXPORTER_OTLP_ENDPOINT
   ```

5. **Wrong endpoint format**
   ```bash
   # OTLP HTTP should include /v1/traces path
   export OTEL_EXPORTER_OTLP_ENDPOINT="http://jaeger:14268/api/traces"
   
   # OTLP gRPC endpoints typically don't include path
   export OTEL_EXPORTER_OTLP_ENDPOINT="http://collector:4317"
   ```

### Issue: High Resource Usage

**Symptoms:**
- Increased CPU usage
- Increased memory consumption
- High network traffic

**Solutions:**

1. **Reduce sampling rate**
   ```bash
   export OTEL_TRACES_SAMPLER=traceidratio
   export OTEL_TRACES_SAMPLER_ARG=0.01  # 1% sampling
   ```

2. **Optimize export settings**
   ```bash
   export OTEL_BSP_MAX_QUEUE_SIZE=512
   export OTEL_BSP_SCHEDULE_DELAY=5000
   export OTEL_BSP_EXPORT_TIMEOUT=30000
   ```

3. **Monitor resource usage**
   ```bash
   # Check AlertManager metrics endpoint
   curl http://localhost:9093/metrics | grep -E "(memory|cpu)"
   ```

### Issue: Partial Traces / Missing Spans

**Symptoms:**
- Some operations show up in traces, others don't
- Incomplete trace trees

**Possible Causes & Solutions:**

1. **Sampling drops some traces**
   - Increase sampling rate temporarily for debugging
   - Use head-based sampling for critical paths

2. **Context not propagated correctly**
   - Check for operations that don't use the context parameter
   - Verify middleware is properly configured

3. **Errors in span creation**
   - Enable debug logging: `--log.level=debug`
   - Look for telemetry-related error messages

### Issue: Export Failures

**Symptoms:**
- Traces generated but not reaching backend
- Error messages about export failures

**Debug Steps:**

1. **Check OTLP endpoint health**
   ```bash
   # For HTTP endpoints
   curl -X POST $OTEL_EXPORTER_OTLP_ENDPOINT \
     -H "Content-Type: application/x-protobuf" \
     -d ""
   
   # Should return 200 or 405, not connection errors
   ```

2. **Verify authentication**
   ```bash
   # Test with headers
   curl -X POST $OTEL_EXPORTER_OTLP_ENDPOINT \
     -H "Content-Type: application/x-protobuf" \
     -H "Authorization: Bearer $TOKEN" \
     -d ""
   ```

3. **Check protocol mismatch**
   ```bash
   # HTTP vs gRPC protocol issues
   export OTEL_EXPORTER_OTLP_PROTOCOL=grpc
   # OR
   export OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
   ```

### Issue: Wrong Service Name in Traces

**Symptoms:**
- Traces appear with incorrect service name
- Cannot filter traces by expected service name

**Solution:**
```bash
export OTEL_SERVICE_NAME="alertmanager"
# OR with custom name
export OTEL_SERVICE_NAME="alertmanager-production"
```

### Issue: Missing Trace Context in Logs

**Symptoms:**
- Cannot correlate logs with traces
- Missing trace_id and span_id in log entries

**Solution:**
Ensure AlertManager is using structured logging and trace correlation:
```bash
./alertmanager --log.format=json --log.level=info
```

## Advanced Debugging

### 1. Enable OTEL Debug Logging

Add debug environment variables:
```bash
export OTEL_LOG_LEVEL=debug
export OTEL_LOGS_EXPORTER=console
```

### 2. Use OTEL Collector for Debugging

Deploy an OTEL Collector with debug exporters:

```yaml
# otel-debug-config.yaml
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
      http:
        endpoint: 0.0.0.0:4318

exporters:
  logging:
    loglevel: debug
  file:
    path: /tmp/traces.json

service:
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [logging, file]
```

### 3. Network Analysis

Capture OTLP traffic:
```bash
# Monitor OTLP HTTP traffic
sudo tcpdump -i any -A port 4318

# Monitor OTLP gRPC traffic  
sudo tcpdump -i any -A port 4317
```

### 4. Test with Manual Traces

Create a simple test to verify the exporter works:

```bash
# Using otel-cli (if available)
otel-cli exec --service alertmanager-test \
  --name "test-span" \
  echo "Testing OTLP export"
```

## Performance Optimization

### 1. Batch Settings

Optimize batch export settings:
```bash
export OTEL_BSP_MAX_QUEUE_SIZE=2048
export OTEL_BSP_SCHEDULE_DELAY=5000  # 5 seconds
export OTEL_BSP_EXPORT_TIMEOUT=30000  # 30 seconds
export OTEL_BSP_MAX_EXPORT_BATCH_SIZE=512
```

### 2. Resource Attributes

Minimize resource attributes for better performance:
```bash
export OTEL_RESOURCE_ATTRIBUTES="service.name=alertmanager,service.version=v0.27.0"
```

### 3. Sampling Strategies

Use appropriate sampling for your environment:

```bash
# Production: Low sampling rate
export OTEL_TRACES_SAMPLER=traceidratio
export OTEL_TRACES_SAMPLER_ARG=0.01

# Development: High sampling rate
export OTEL_TRACES_SAMPLER=traceidratio
export OTEL_TRACES_SAMPLER_ARG=1.0

# Critical systems: Always sample errors
export OTEL_TRACES_SAMPLER=parentbased_traceidratio
export OTEL_TRACES_SAMPLER_ARG=0.1
```

## Monitoring Tracing Health

### 1. AlertManager Metrics

Monitor these metrics to track tracing health:

```promql
# Trace export errors (if exposed)
increase(otel_exporter_errors_total[5m])

# Memory usage impact
process_resident_memory_bytes{job="alertmanager"}

# CPU usage impact
rate(process_cpu_seconds_total{job="alertmanager"}[5m])
```

### 2. OTEL Collector Metrics

If using OTEL Collector, monitor:

```promql
# Received spans
otelcol_receiver_accepted_spans_total

# Export failures
otelcol_exporter_send_failed_spans_total

# Queue length
otelcol_processor_batch_batch_send_size_bucket
```

## Getting Help

### 1. Log Analysis

When reporting issues, include:
- AlertManager startup logs
- Any error messages mentioning "telemetry" or "otel"
- Environment variables (sanitize sensitive data)
- Network connectivity test results

### 2. Minimal Reproduction

Create a minimal reproduction case:
```bash
# Minimal test setup
export OTEL_EXPORTER_OTLP_ENDPOINT="http://localhost:4318"
export OTEL_SERVICE_NAME="alertmanager-test"
export OTEL_TRACES_SAMPLER=always_on

./alertmanager --tracing.enabled=true --log.level=debug
```

### 3. Community Resources

- [AlertManager GitHub Issues](https://github.com/prometheus/alertmanager/issues)
- [OpenTelemetry Community](https://opentelemetry.io/community/)
- [CNCF Slack #opentelemetry](https://cloud-native.slack.com/)

## Checklist for New Deployments

Before deploying tracing in production:

- [ ] Test OTLP endpoint connectivity
- [ ] Verify authentication and authorization
- [ ] Set appropriate sampling rates
- [ ] Monitor resource usage impact
- [ ] Configure alerts for export failures
- [ ] Document trace collection URLs for teams
- [ ] Test trace data retention policies
- [ ] Verify trace data privacy compliance

## Known Limitations

1. **Cluster gossip protocol tracing**: Limited visibility into memberlist internals
2. **Template rendering**: May have high span volume for complex templates
3. **High-frequency operations**: Consider sampling for operations like silence checks
4. **Context propagation**: Some third-party libraries may not propagate context

For the latest information and updates, see the [AlertManager documentation](https://prometheus.io/docs/alerting/latest/alertmanager/).
