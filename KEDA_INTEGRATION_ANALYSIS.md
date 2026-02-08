# KEDA Integration Analysis for CloudNativePG

## Executive Summary

CloudNativePG already has the necessary foundation for KEDA integration through its scale subresource support. KEDA can scale CNPG clusters today without any code changes, but adding PostgreSQL-specific scalers would provide more intelligent autoscaling.

## Current State

### CNPG Scaling Capabilities
- ✅ Scale subresource enabled on Cluster CRD
- ✅ Safe scale-up/down operations respecting PostgreSQL constraints
- ✅ Prometheus metrics exposed for monitoring
- ✅ Validation webhooks ensuring configuration consistency
- ❌ No built-in autoscaling
- ❌ No KEDA-specific integration

### Key Constraints
1. Minimum instances: 1
2. `maxSyncReplicas` must be < total instances
3. Scale-down operations must not break synchronous replication
4. PVC lifecycle must be properly managed

## KEDA Integration Options

### Option 1: External KEDA Configuration (No CNPG Changes Required)

Use KEDA's existing scalers with CNPG's scale subresource:

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: cnpg-cluster-scaler
spec:
  scaleTargetRef:
    apiVersion: postgresql.cnpg.io/v1
    kind: Cluster
    name: my-cluster
  minReplicaCount: 2
  maxReplicaCount: 5
  triggers:
  - type: prometheus
    metadata:
      serverAddress: http://prometheus:9090
      metricName: cnpg_postgres_replication_lag_seconds
      threshold: '30'
      query: |
        max(cnpg_postgres_replication_lag_seconds{cluster="my-cluster"})
```

**Pros:**
- Works today without changes
- Uses standard KEDA patterns
- Leverages existing CNPG metrics

**Cons:**
- Requires manual KEDA configuration
- Generic scalers may not understand PostgreSQL-specific concerns

### Option 2: Custom KEDA Scaler for PostgreSQL

Develop a PostgreSQL-specific KEDA scaler that understands:
- Connection pool saturation
- Replication lag
- Query performance metrics
- Transaction queue depth

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: cnpg-cluster-scaler
spec:
  scaleTargetRef:
    apiVersion: postgresql.cnpg.io/v1
    kind: Cluster
    name: my-cluster
  minReplicaCount: 2
  maxReplicaCount: 5
  triggers:
  - type: postgresql-cnpg
    metadata:
      clusterName: my-cluster
      mode: readReplicas  # or 'all'
      metrics:
      - replicationLag: 30s
      - connectionSaturation: 80%
      - activeConnections: 100
```

**Pros:**
- PostgreSQL-aware scaling decisions
- Better integration with CNPG features
- Could respect sync replica constraints

**Cons:**
- Requires developing a new KEDA scaler
- Maintenance of external component

### Option 3: Native KEDA Support in CNPG

Add KEDA awareness directly to CNPG:

1. **New CRD fields:**
```go
type ClusterSpec struct {
    // ... existing fields ...
    
    // Autoscaling configuration
    Autoscaling *AutoscalingSpec `json:"autoscaling,omitempty"`
}

type AutoscalingSpec struct {
    // Enable KEDA autoscaling
    Enabled bool `json:"enabled"`
    
    // Minimum number of instances
    MinInstances int `json:"minInstances"`
    
    // Maximum number of instances
    MaxInstances int `json:"maxInstances"`
    
    // Scaling metrics
    Metrics []ScalingMetric `json:"metrics"`
}

type ScalingMetric struct {
    Type string `json:"type"` // replicationLag, connections, cpu, memory
    Threshold string `json:"threshold"`
}
```

2. **Controller changes:**
   - Create/update KEDA ScaledObject when autoscaling is enabled
   - Ensure scaling respects PostgreSQL constraints
   - Handle cleanup when autoscaling is disabled

**Pros:**
- Seamless user experience
- PostgreSQL-specific defaults
- Integrated lifecycle management

**Cons:**
- Adds KEDA dependency (optional)
- More complex implementation

## Recommended Approach

### Phase 1: Documentation and Examples (Immediate)
- Document how to use KEDA with CNPG today
- Provide example ScaledObject configurations
- Create scaling best practices guide

### Phase 2: Custom KEDA Scaler (Short-term)
- Develop postgresql-cnpg scaler for KEDA
- Focus on read-replica scaling initially
- Integrate with CNPG's metrics

### Phase 3: Native Integration (Long-term)
- Add optional autoscaling spec to Cluster CRD
- Manage KEDA ScaledObject lifecycle
- Provide sensible PostgreSQL-specific defaults

## Implementation Requirements

### For Native Integration (Option 3):

1. **API Changes:**
   - Add autoscaling fields to ClusterSpec
   - Update CRD validation

2. **Controller Changes:**
   - Watch for autoscaling configuration changes
   - Create/update/delete ScaledObject resources
   - Add RBAC permissions for ScaledObject management

3. **Webhook Changes:**
   - Validate autoscaling configuration
   - Ensure min/max constraints are valid

4. **Documentation:**
   - Autoscaling configuration guide
   - Metric selection best practices
   - Troubleshooting guide

### Key Files to Modify:
- `api/v1/cluster_types.go` - Add autoscaling spec
- `internal/controller/cluster_controller.go` - Add ScaledObject reconciliation
- `internal/webhook/v1/cluster_webhook.go` - Add validation
- `config/rbac/role.yaml` - Add KEDA permissions

## Metrics for Scaling Decisions

### Available CNPG Metrics:
1. `cnpg_postgres_replication_lag_seconds` - Replication lag
2. `cnpg_postgres_connections_total` - Connection count
3. `cnpg_postgres_connections_max` - Max connections
4. `cnpg_postgres_xlog_position_bytes` - WAL position
5. `cnpg_postgres_stat_database_*` - Database statistics

### Recommended Scaling Triggers:
1. **Read Replica Scaling:**
   - High replication lag
   - Read query queue depth
   - Connection pool saturation

2. **General Scaling:**
   - CPU/Memory utilization
   - Active connection ratio
   - Transaction throughput

## Conclusion

KEDA integration with CloudNativePG is feasible today using KEDA's ScaledObject with the existing scale subresource. For a better user experience, implementing native KEDA support would provide PostgreSQL-aware autoscaling with proper constraint handling.