# Rebalancing PRD

## Controlled Rebalancing Policy

### Objective

Prevent service disruption caused by frequent pod migrations. Rebalancing must occur **only when the system state clearly benefits from it** and when disruption risk is minimal.

---

### 1. Rebalancing Control Flag

Environment variable:

```bash
REBALANCING_ENABLED=true
```

Default:

```bash
true
```

Behavior:

| Value | Behavior                                  |
| ----- | ----------------------------------------- |
| true  | Scheduler may execute rebalance algorithm |
| false | Rebalancing logic completely bypassed     |

Implementation:

```text
if !REBALANCING_ENABLED:
    return
```

---

### 2. Rebalancing Cooldown

Prevent continuous rebalancing cycles.

```bash
REBALANCE_COOLDOWN_SECONDS=1800
```

Default: **30 minutes**

Algorithm:

```text
if now - last_rebalance_time < REBALANCE_COOLDOWN_SECONDS:
    skip_rebalance
```

Store `last_rebalance_time` in:

```text
redis / scheduler memory / etcd
```

---

### 3. Rebalancing Trigger Conditions

Rebalancing must only run when at least one **system imbalance signal** is detected.

#### 3.1 Node Utilization Imbalance

Trigger if:

```text
max_cpu_util - min_cpu_util > 0.35
```

or

```text
max_mem_util - min_mem_util > 0.35
```

Meaning one node is overloaded while others are idle.

#### 3.2 Free Node Detected

Trigger if:

```text
node_utilization < 0.25
```

for both CPU and memory.

Indicates cluster underutilization and opportunity to redistribute pods.

#### 3.3 Latency Hotspot

Trigger if service pair latency exceeds threshold:

```text
P95_latency(S,X) > LATENCY_THRESHOLD
```

Example:

```text
LATENCY_THRESHOLD = 40ms
```

Indicates chatty services placed on distant nodes.

---

### 4. Disruption Budget Protection

Never violate Kubernetes PDB.

Before moving a pod:

```text
if service_available_pods <= min_available:
    skip_migration
```

Also enforce:

```text
MAX_PODS_MOVED_PER_CYCLE=3
```

Prevents large migrations.

---

### 5. Migration Cost Filter

Moving a pod has cost.

Calculate:

```text
MigrationCost =
    restart_time
  + warmup_time
  + cache_loss_penalty
```

Only migrate if:

```text
ExpectedBenefit > MigrationCost
```

Benefit sources:

- CPU balancing
- reduced network latency
- reduced node risk concentration

---

### 6. Safe Migration Constraints

Never migrate pods that are:

```text
stateful
daemonset
control-plane
recently restarted
```

Minimum pod age:

```text
MIN_POD_AGE_FOR_REBALANCE = 10 minutes
```

---

### 7. Anti-Thrashing Lock

Prevent pod oscillation.

Maintain:

```text
pod_last_moved_time
```

Rule:

```text
if now - pod_last_moved_time < 1 hour:
    skip
```

---

### 8. Rebalancing Strategy

When rebalancing triggers:

1. Discover nodes
2. Compute node scores
3. Detect free nodes
4. Identify overloaded nodes
5. Select offload candidates
6. Prioritize **talkative service pairs**
7. Compute migration plan
8. Execute limited migrations

---

### 9. Execution Limits

Per cycle limits:

```text
MAX_PODS_MOVED_PER_CYCLE = 3
MAX_REBALANCE_DURATION = 30s
```

Abort cycle if exceeded.

---

### 10. Observability

Expose metrics:

```text
scheduler_rebalance_cycles_total
scheduler_rebalance_skipped_total
scheduler_pods_migrated_total
scheduler_rebalance_duration_seconds
```

---

### 11. Safety Priority Order

Always prioritize:

1. **Service availability**
2. **PDB compliance**
3. **Cluster stability**
4. **Latency improvements**
5. **Load balancing**

Rebalancing must **never degrade availability**.
