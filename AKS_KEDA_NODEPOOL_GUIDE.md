# KEDA + AKS Node Pool Autoscaling for CloudNativePG

This guide shows how to configure KEDA to scale CloudNativePG clusters on dedicated AKS node pools with automatic node scaling.

## Architecture Overview

```
KEDA (scales CNPG replicas) → Pod Pending → Cluster Autoscaler → AKS scales node pool
```

## Step 1: Create Dedicated AKS Node Pool for Databases

```bash
# Create a dedicated node pool with autoscaling enabled
az aks nodepool add \
  --resource-group myResourceGroup \
  --cluster-name myAKSCluster \
  --name dbpool \
  --node-count 2 \
  --min-count 2 \
  --max-count 10 \
  --enable-cluster-autoscaler \
  --node-vm-size Standard_E8s_v5 \
  --node-osdisk-size 256 \
  --node-osdisk-type Ephemeral \
  --labels workload=database \
  --node-taints database=postgres:NoSchedule \
  --mode User \
  --os-sku Ubuntu \
  --zones 1 2 3

# Optional: Set up spot instances for cost savings (for non-production)
az aks nodepool add \
  --resource-group myResourceGroup \
  --cluster-name myAKSCluster \
  --name dbpoolspot \
  --priority Spot \
  --eviction-policy Delete \
  --spot-max-price -1 \
  --node-count 0 \
  --min-count 0 \
  --max-count 5 \
  --enable-cluster-autoscaler \
  --node-vm-size Standard_E8s_v5 \
  --labels workload=database-spot \
  --node-taints database=postgres:NoSchedule \
  --node-taints kubernetes.azure.com/scalesetpriority=spot:NoSchedule
```

## Step 2: Configure CloudNativePG Cluster with Node Affinity

```yaml
# postgres-cluster-aks.yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: postgres-cluster
  namespace: database
spec:
  instances: 3
  
  # PostgreSQL image
  imageName: ghcr.io/cloudnative-pg/postgresql:16.1
  
  # Synchronous replicas
  minSyncReplicas: 1
  maxSyncReplicas: 2
  
  # Node affinity for dedicated database nodes
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
        - matchExpressions:
          - key: workload
            operator: In
            values:
            - database
            - database-spot  # Include spot nodes if available
    # Anti-affinity to spread replicas across nodes
    podAntiAffinity:
      preferredDuringSchedulingIgnoredDuringExecution:
      - weight: 100
        podAffinityTerm:
          labelSelector:
            matchExpressions:
            - key: cnpg.io/cluster
              operator: In
              values:
              - postgres-cluster
          topologyKey: kubernetes.io/hostname
  
  # Tolerations for database node taints
  tolerations:
  - key: database
    operator: Equal
    value: postgres
    effect: NoSchedule
  - key: kubernetes.azure.com/scalesetpriority
    operator: Equal
    value: spot
    effect: NoSchedule
  
  # Resource requests/limits to help cluster autoscaler
  resources:
    requests:
      memory: "4Gi"
      cpu: "2"
    limits:
      memory: "8Gi"
      cpu: "4"
  
  # Storage using Azure Premium SSD
  storage:
    size: 100Gi
    storageClass: managed-csi-premium
  
  # PostgreSQL configuration
  postgresql:
    parameters:
      max_connections: "200"
      shared_buffers: "2GB"
      effective_cache_size: "6GB"
  
  # Monitoring
  monitoring:
    enabled: true
```

Apply the cluster:
```bash
kubectl create namespace database
kubectl apply -f postgres-cluster-aks.yaml
```

## Step 3: Configure KEDA with Resource-Aware Scaling

```yaml
# keda-scaler-aks.yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: postgres-cluster-scaler
  namespace: database
spec:
  scaleTargetRef:
    apiVersion: postgresql.cnpg.io/v1
    kind: Cluster
    name: postgres-cluster
  
  # Start with reasonable defaults
  minReplicaCount: 3
  maxReplicaCount: 15  # Should align with node pool capacity
  
  # Longer polling for cost optimization
  pollingInterval: 60
  cooldownPeriod: 600  # 10 minutes to avoid node churn
  
  # Advanced scaling to coordinate with cluster autoscaler
  advanced:
    restoreToOriginalReplicaCount: false
    horizontalPodAutoscalerConfig:
      behavior:
        scaleUp:
          # Slow scale up to give cluster autoscaler time
          stabilizationWindowSeconds: 300
          selectPolicy: Max
          policies:
          - type: Pods
            value: 1
            periodSeconds: 300  # Add 1 pod every 5 minutes
          - type: Percent
            value: 20
            periodSeconds: 600  # Or 20% every 10 minutes
        scaleDown:
          # Very slow scale down to avoid node termination
          stabilizationWindowSeconds: 1800  # 30 minutes
          selectPolicy: Min
          policies:
          - type: Pods
            value: 1
            periodSeconds: 900  # Remove 1 pod every 15 minutes
  
  triggers:
  # Connection-based scaling
  - type: prometheus
    metadata:
      serverAddress: http://prometheus.monitoring.svc:9090
      metricName: connection_utilization
      threshold: "60"  # Lower threshold for earlier scaling
      query: |
        (sum(cnpg_postgres_connections_total{cluster="postgres-cluster"}) / 
         sum(cnpg_postgres_connections_max{cluster="postgres-cluster"})) * 100
  
  # CPU utilization across the cluster
  - type: prometheus
    metadata:
      serverAddress: http://prometheus.monitoring.svc:9090
      metricName: cluster_cpu_usage
      threshold: "50"  # Scale before nodes are saturated
      query: |
        avg(rate(container_cpu_usage_seconds_total{pod=~"postgres-cluster-.*"}[5m])) * 100
```

## Step 4: Configure Cluster Autoscaler Settings

```yaml
# cluster-autoscaler-config.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: cluster-autoscaler-status
  namespace: kube-system
data:
  # Optimize for database workloads
  nodes.max-node-provision-time: "30m"
  nodes.scale-down-delay-after-add: "30m"
  nodes.scale-down-unneeded-time: "30m"
  nodes.scale-down-utilization-threshold: "0.5"
  # Prevent scale down of nodes with databases
  nodes.skip-nodes-with-local-storage: "true"
  nodes.skip-nodes-with-system-pods: "true"
```

## Step 5: Priority Classes for Database Pods

```yaml
# priority-class.yaml
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata:
  name: database-critical
value: 1000
globalDefault: false
description: "Priority class for database workloads"
---
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata:
  name: database-high
value: 900
globalDefault: false
description: "Priority class for database replicas"
```

Update CNPG cluster to use priority:
```yaml
spec:
  # ... other settings ...
  priorityClassName: database-critical
  
  # For replicas, you might use a different priority
  replica:
    priorityClassName: database-high
```

## Step 6: Monitoring Node Pool Scaling

```bash
# Watch node pool scaling
watch -n 5 'az aks nodepool show \
  --resource-group myResourceGroup \
  --cluster-name myAKSCluster \
  --name dbpool \
  --query "{current: agentPoolProfiles[0].count, min: agentPoolProfiles[0].minCount, max: agentPoolProfiles[0].maxCount}" \
  -o table'

# Monitor cluster autoscaler events
kubectl get events -n kube-system | grep cluster-autoscaler

# Check pending pods that trigger scaling
kubectl get pods -n database -o wide | grep Pending

# View cluster autoscaler status
kubectl -n kube-system logs -l app=cluster-autoscaler -f
```

## Step 7: Cost Optimization Strategies

### A. Time-based Node Pool Scaling
```bash
# Scale down node pool during off-hours
# Create automation account or use Azure Functions

# Scale down for weekends
az aks nodepool update \
  --resource-group myResourceGroup \
  --cluster-name myAKSCluster \
  --name dbpool \
  --min-count 1 \
  --max-count 3

# Scale up for weekdays
az aks nodepool update \
  --resource-group myResourceGroup \
  --cluster-name myAKSCluster \
  --name dbpool \
  --min-count 2 \
  --max-count 10
```

### B. Use KEDA Cron Triggers
```yaml
triggers:
# Reduce replicas during off-hours
- type: cron
  metadata:
    timezone: America/New_York
    start: 0 20 * * 1-5  # 8 PM weekdays
    end: 0 6 * * 1-5    # 6 AM weekdays
    desiredReplicas: "2"

# Weekend scaling
- type: cron
  metadata:
    timezone: America/New_York
    start: 0 20 * * 5   # Friday 8 PM
    end: 0 6 * * 1      # Monday 6 AM
    desiredReplicas: "1"
```

## Step 8: Advanced Configuration for Production

```yaml
# production-postgres-aks.yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: postgres-prod
  namespace: database
spec:
  instances: 5
  
  # High availability across zones
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
        - matchExpressions:
          - key: workload
            operator: In
            values: ["database"]
          - key: kubernetes.io/os
            operator: In
            values: ["linux"]
    podAntiAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
      - labelSelector:
          matchLabels:
            cnpg.io/cluster: postgres-prod
        topologyKey: topology.kubernetes.io/zone
  
  # Pod Disruption Budget
  minAvailable: 2
  
  # Resource allocation
  resources:
    requests:
      memory: "8Gi"
      cpu: "4"
      ephemeral-storage: "10Gi"
    limits:
      memory: "16Gi"
      cpu: "8"
      ephemeral-storage: "20Gi"
  
  # Topology spread constraints
  topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: topology.kubernetes.io/zone
    whenUnsatisfiable: DoNotSchedule
    labelSelector:
      matchLabels:
        cnpg.io/cluster: postgres-prod
```

## Troubleshooting

### Pods Stuck in Pending
```bash
# Check why pods are pending
kubectl describe pod -n database postgres-cluster-4

# Common issues:
# - No nodes match affinity/tolerations
# - Insufficient resources
# - Node pool at max capacity

# Force cluster autoscaler to act
kubectl scale cluster postgres-cluster -n database --replicas=10
```

### Node Pool Not Scaling
```bash
# Check cluster autoscaler logs
kubectl logs -n kube-system -l component=cluster-autoscaler

# Verify node pool autoscaling is enabled
az aks nodepool show \
  --resource-group myResourceGroup \
  --cluster-name myAKSCluster \
  --name dbpool \
  --query enableAutoScaling

# Check AKS cluster autoscaler profile
az aks show \
  --resource-group myResourceGroup \
  --name myAKSCluster \
  --query autoScalerProfile
```

### Cost Monitoring
```bash
# Enable cost analysis for the node pool
az aks nodepool update \
  --resource-group myResourceGroup \
  --cluster-name myAKSCluster \
  --name dbpool \
  --tags Environment=Production Workload=Database CostCenter=Engineering

# Use Azure Cost Management to track database node pool costs
```

## Best Practices

1. **Node Pool Sizing**
   - Size VMs to fit 2-4 PostgreSQL instances per node
   - Use memory-optimized VMs (E-series) for databases
   - Enable ephemeral OS disks for better performance

2. **Scaling Coordination**
   - Set KEDA scale up slower than cluster autoscaler provisions nodes
   - Set KEDA scale down much slower than cluster autoscaler timeout
   - Use pod disruption budgets to prevent data loss

3. **Cost Management**
   - Use spot instances for read replicas (with tolerations)
   - Implement time-based scaling for predictable workloads
   - Monitor unused node capacity

4. **High Availability**
   - Spread instances across availability zones
   - Use separate node pools for primary and replicas
   - Implement proper backup strategies

This setup ensures that as KEDA scales your PostgreSQL instances, AKS automatically provisions or removes nodes from your dedicated database pool, maintaining optimal resource utilization and cost efficiency.