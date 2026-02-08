# Setting Up KEDA Autoscaling for CloudNativePG

This guide shows how to configure KEDA to automatically scale your CloudNativePG clusters based on metrics.

## Prerequisites

1. **Kubernetes cluster** with CloudNativePG installed
2. **KEDA** installed in your cluster
3. **Prometheus** collecting CNPG metrics (optional, for metric-based scaling)

## Step 1: Install KEDA

```bash
# Using Helm
helm repo add kedacore https://kedacore.github.io/charts
helm repo update
helm install keda kedacore/keda --namespace keda --create-namespace

# Or using kubectl
kubectl apply --server-side -f https://github.com/kedacore/keda/releases/download/v2.13.0/keda-2.13.0.yaml
```

## Step 2: Create a CloudNativePG Cluster

```yaml
# postgres-cluster.yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: postgres-cluster
  namespace: default
spec:
  instances: 3  # Initial replica count (will be managed by KEDA)
  
  # PostgreSQL version
  imageName: ghcr.io/cloudnative-pg/postgresql:16.1
  
  # Ensure proper sync replica configuration
  minSyncReplicas: 1
  maxSyncReplicas: 2
  
  # Basic PostgreSQL configuration
  postgresql:
    parameters:
      max_connections: "200"
      shared_buffers: "256MB"
  
  # Enable monitoring for Prometheus metrics
  monitoring:
    enabled: true
    customQueriesConfigMap:
      - name: cnpg-default-monitoring
        key: queries
  
  # Storage configuration
  storage:
    size: 10Gi
    storageClass: standard
```

Apply it:
```bash
kubectl apply -f postgres-cluster.yaml
```

## Step 3: Configure KEDA ScaledObject

### Option A: Simple CPU-based Scaling

```yaml
# keda-cpu-scaling.yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: postgres-cpu-scaler
  namespace: default
spec:
  # Target the CNPG Cluster
  scaleTargetRef:
    apiVersion: postgresql.cnpg.io/v1
    kind: Cluster
    name: postgres-cluster
  
  # Scaling limits
  minReplicaCount: 2  # Minimum 2 replicas
  maxReplicaCount: 8  # Maximum 8 replicas
  
  # Cool down period
  cooldownPeriod: 300  # 5 minutes
  
  triggers:
  # CPU-based scaling
  - type: cpu
    metadataFromEnv: []
    metadata:
      type: Utilization
      value: "70"  # Scale up at 70% CPU
```

Apply it:
```bash
kubectl apply -f keda-cpu-scaling.yaml
```

### Option B: Connection-based Scaling (with Prometheus)

First, ensure Prometheus is scraping CNPG metrics:

```yaml
# prometheus-servicemonitor.yaml (if using Prometheus Operator)
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: cnpg-cluster-metrics
  namespace: default
spec:
  selector:
    matchLabels:
      cnpg.io/cluster: postgres-cluster
  endpoints:
  - port: metrics
    interval: 30s
```

Then create the KEDA ScaledObject:

```yaml
# keda-connection-scaling.yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: postgres-connection-scaler
  namespace: default
spec:
  scaleTargetRef:
    apiVersion: postgresql.cnpg.io/v1
    kind: Cluster
    name: postgres-cluster
  
  minReplicaCount: 2
  maxReplicaCount: 8
  
  # Polling interval
  pollingInterval: 30
  
  # Advanced scaling behavior
  advanced:
    horizontalPodAutoscalerConfig:
      behavior:
        scaleUp:
          stabilizationWindowSeconds: 60
          policies:
          - type: Pods
            value: 1
            periodSeconds: 60
        scaleDown:
          stabilizationWindowSeconds: 300
          policies:
          - type: Pods
            value: 1
            periodSeconds: 300
  
  triggers:
  # Scale based on connection utilization
  - type: prometheus
    metadata:
      serverAddress: http://prometheus.monitoring.svc:9090
      metricName: postgres_connections_utilization
      threshold: "70"  # Scale at 70% connection utilization
      query: |
        (sum(cnpg_postgres_connections_total{cluster="postgres-cluster"}) / 
         sum(cnpg_postgres_connections_max{cluster="postgres-cluster"})) * 100
```

Apply it:
```bash
kubectl apply -f keda-connection-scaling.yaml
```

### Option C: Multi-metric Scaling

```yaml
# keda-multi-metric-scaling.yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: postgres-multi-scaler
  namespace: default
spec:
  scaleTargetRef:
    apiVersion: postgresql.cnpg.io/v1
    kind: Cluster
    name: postgres-cluster
  
  minReplicaCount: 3
  maxReplicaCount: 10
  
  triggers:
  # CPU utilization
  - type: cpu
    metadata:
      type: Utilization
      value: "60"
  
  # Memory utilization
  - type: memory
    metadata:
      type: Utilization
      value: "70"
  
  # Active connections (requires Prometheus)
  - type: prometheus
    metadata:
      serverAddress: http://prometheus.monitoring.svc:9090
      metricName: active_connections
      threshold: "100"  # Scale when > 100 active connections
      query: |
        sum(cnpg_postgres_connections_total{cluster="postgres-cluster",state="active"})
  
  # Replication lag (requires Prometheus)
  - type: prometheus
    metadata:
      serverAddress: http://prometheus.monitoring.svc:9090
      metricName: replication_lag
      threshold: "5"  # Scale when lag > 5 seconds
      query: |
        max(cnpg_postgres_replication_lag_seconds{cluster="postgres-cluster"})
```

## Step 4: Verify KEDA is Working

```bash
# Check KEDA ScaledObject status
kubectl get scaledobject postgres-cpu-scaler
kubectl describe scaledobject postgres-cpu-scaler

# Watch the HPA created by KEDA
kubectl get hpa
kubectl describe hpa keda-hpa-postgres-cpu-scaler

# Monitor cluster scaling
kubectl get cluster postgres-cluster -w

# Check KEDA operator logs
kubectl logs -n keda deployment/keda-operator -f

# Generate load to trigger scaling
kubectl run -it --rm pgbench --image=postgres:16 -- \
  pgbench -h postgres-cluster-rw -U postgres -i -s 10 postgres
```

## Step 5: Monitor Scaling Events

```bash
# View scaling events
kubectl get events --field-selector involvedObject.name=postgres-cluster

# Check cluster status
kubectl describe cluster postgres-cluster

# View pod creation/deletion
kubectl get pods -l cnpg.io/cluster=postgres-cluster -w
```

## Important Considerations

### 1. Respect CNPG Constraints
- Never scale below `maxSyncReplicas + 1` instances
- Account for `minSyncReplicas` in your minimum replica count
- Allow time for replication to catch up during scale events

### 2. Scaling Behavior
```yaml
advanced:
  horizontalPodAutoscalerConfig:
    behavior:
      scaleUp:
        stabilizationWindowSeconds: 60  # Prevent flapping
        policies:
        - type: Pods
          value: 1  # Add 1 pod at a time
          periodSeconds: 60
      scaleDown:
        stabilizationWindowSeconds: 300  # Wait 5 minutes before scaling down
        policies:
        - type: Pods
          value: 1  # Remove 1 pod at a time
          periodSeconds: 300
```

### 3. Choosing Metrics
- **CPU/Memory**: Good for general workload scaling
- **Connections**: Better for connection-heavy workloads
- **Replication Lag**: Important for read-heavy workloads
- **Custom Queries**: Use pg_stat_* tables for specific metrics

### 4. Testing Your Configuration
```bash
# Generate CPU load
kubectl exec -it postgres-cluster-1 -- \
  psql -U postgres -c "SELECT pg_sleep(1) FROM generate_series(1,1000);"

# Generate connection load
for i in {1..50}; do
  kubectl run -it --rm pgbench-$i --image=postgres:16 -- \
    pgbench -h postgres-cluster-rw -U postgres -T 300 postgres &
done
```

## Troubleshooting

### KEDA Not Scaling
```bash
# Check ScaledObject status
kubectl describe scaledobject postgres-cpu-scaler

# Check HPA status
kubectl get hpa

# View KEDA metrics server
kubectl logs -n keda deployment/keda-operator-metrics-apiserver

# Check if metrics are available
kubectl get --raw /apis/metrics.k8s.io/v1beta1/namespaces/default/pods
```

### Prometheus Connection Issues
```bash
# Test Prometheus query
kubectl exec -it deployment/prometheus -- \
  wget -O- 'http://localhost:9090/api/v1/query?query=cnpg_postgres_connections_total'

# Check ServiceMonitor
kubectl get servicemonitor
kubectl describe servicemonitor cnpg-cluster-metrics
```

## Example: Production-Ready Configuration

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: postgres-production-scaler
  namespace: production
spec:
  scaleTargetRef:
    apiVersion: postgresql.cnpg.io/v1
    kind: Cluster
    name: postgres-cluster
  
  # Conservative scaling limits
  minReplicaCount: 3  # HA minimum
  maxReplicaCount: 12  # Resource limit
  
  # Polling and cooldown
  pollingInterval: 30
  cooldownPeriod: 600  # 10 minutes
  
  # Gradual scaling
  advanced:
    restoreToOriginalReplicaCount: false
    horizontalPodAutoscalerConfig:
      behavior:
        scaleUp:
          stabilizationWindowSeconds: 120
          selectPolicy: Max
          policies:
          - type: Pods
            value: 2
            periodSeconds: 120
          - type: Percent
            value: 50
            periodSeconds: 180
        scaleDown:
          stabilizationWindowSeconds: 600
          selectPolicy: Min
          policies:
          - type: Pods
            value: 1
            periodSeconds: 300
  
  triggers:
  # Primary metric: Connection pressure
  - type: prometheus
    metadata:
      serverAddress: http://prometheus.monitoring.svc:9090
      metricName: connection_pressure
      threshold: "65"
      query: |
        (sum(cnpg_postgres_connections_total{cluster="postgres-cluster"}) / 
         sum(cnpg_postgres_connections_max{cluster="postgres-cluster"})) * 100
  
  # Secondary metric: Query performance
  - type: prometheus
    metadata:
      serverAddress: http://prometheus.monitoring.svc:9090
      metricName: slow_query_rate
      threshold: "10"
      query: |
        sum(rate(pg_stat_statements_calls{cluster="postgres-cluster",mean_time_ms>100}[5m]))
  
  # Fallback: CPU pressure
  - type: cpu
    metadata:
      type: Utilization
      value: "75"
```

This configuration provides a production-ready autoscaling setup that:
- Maintains HA with minimum 3 replicas
- Scales gradually to avoid disruption
- Uses multiple metrics for better decision making
- Has conservative cooldown periods to prevent flapping