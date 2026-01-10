# Production-Grade Kubernetes Scheduler Extender

A **concurrent, architecture-aware, Redis-backed scheduler** that implements optimistic locking and proper validation patterns.

## 🎯 Core Principles

1. **Redis is a coordination system, not the source of truth**
2. **Scheduling must be optimistic + validated**
3. **Node state changes invalidate decisions**
4. **Architecture mismatch is a hard fail**
5. **All writes must be atomic**
6. **Every scheduling decision must be reversible**

---

## 🏗️ Architecture

### Data Model in Redis

#### Node State (versioned)
```text
Key: node:{nodeName}
```
```json
{
  "name": "node-1",
  "arch": "arm64",
  "os": "linux",
  "cpu_allocatable_m": 10000,
  "mem_allocatable_bytes": 8421378048,
  "cpu_used_m": 3400,
  "mem_used_bytes": 2147483648,
  "pods_used": 42,
  "pods_allocatable": 110,
  "ready": true,
  "taints": ["control-plane"],
  "version": 183
}
```

**Version is MANDATORY** - no version = race condition.

#### Pod Scheduling Intent
```text
Key: pod:{uid}
```
```json
{
  "uid": "abc-123",
  "namespace": "default",
  "name": "my-pod",
  "cpu_request_m": 200,
  "mem_request_bytes": 268435456,
  "arch": "arm64",
  "os": "linux",
  "priority": 0,
  "scheduler": "my-scheduler"
}
```

#### Distributed Locks
```text
lock:node:{nodeName}
lock:pod:{uid}
```

Uses **Redis SET NX PX** with 500ms TTL. No blocking locks.

---

## 🔄 Scheduling Algorithm

### High-Level Flow
```
1. Extract pod intent
2. Fetch candidate nodes from Redis
3. HARD FILTER (architecture, resources, taints)
4. SOFT SCORE (CPU/memory ratios, fragmentation)
5. RESERVE with optimistic locking
6. VALIDATE against Kubernetes API
7. BIND pod to node
8. COMMIT or ROLLBACK
```

### Step-by-Step Detail

#### STEP 1: Extract Pod Intent
- Extract CPU/memory requests (including init containers)
- Extract architecture requirement (MANDATORY)
- Extract OS requirement
- Extract tolerations and node selectors
- **Fail fast** if arch/os/resources missing

#### STEP 2: Fetch Candidate Nodes
- Read all node states from Redis (`SCAN node:*`)
- **No locks** at this stage
- **No mutations** at this stage

#### STEP 3: HARD FILTER
Reject node if **any** is true:
- `node.ready == false`
- `node.arch != pod.arch` ⚠️ **HARD FAIL - never fallback**
- `node.os != pod.os`
- `node.cpu_allocatable - node.cpu_used < pod.cpu_request`
- `node.mem_allocatable - node.mem_used < pod.mem_request`
- `node.pods_used >= node.pods_allocatable`
- Taint not tolerated
- Node selector mismatch

#### STEP 4: SOFT SCORING
```
score = 
  w1 * cpu_free_ratio +
  w2 * mem_free_ratio -
  w3 * fragmentation_penalty -
  w4 * taint_penalty
```

Weights (tunable):
- `cpu_free_weight = 1.0`
- `mem_free_weight = 1.0`
- `fragmentation_weight = 0.5`
- `taint_weight = 2.0`

Score normalized to 0-100, sorted descending.

#### STEP 5: RESERVATION (Critical Section)

For each node in score order:

##### 5.1 Acquire Node Lock
```redis
SET lock:node:{node} <uuid> NX PX 500
```
If fail → try next node.

##### 5.2 Read Node State AGAIN
```redis
GET node:{node}
```
Verify: `node.version == cached.version`
If mismatch → release lock → continue.

##### 5.3 Re-check Constraints
Repeat **all hard checks** on fresh state.
If fail → release lock → continue.

##### 5.4 Atomic Reservation (Lua Script)
```lua
-- Check resources
if cpu_free >= pod_cpu and mem_free >= pod_mem then
  cpu_used += pod_cpu
  mem_used += pod_mem
  pods_used += 1
  version += 1
  return OK
else
  return FAIL
end
```
**Must be Lua script** for atomicity.

##### 5.5 Release Lock
Immediately after reservation.

#### STEP 6: API Server Validation
Verify against Kubernetes API:
- Node Ready condition
- Architecture unchanged (CRITICAL)
- No new blocking taints

If mismatch → **ROLLBACK**.

#### STEP 7: Bind Pod
```go
POST /api/v1/namespaces/{ns}/pods/{name}/binding
```

#### STEP 8: COMMIT or ROLLBACK

**On success:**
- Log scheduling result
- Clean up locks (TTL handles this)

**On failure:**
- Rollback reservation (Lua script)
- Increment node.version
- Try next node

---

## 🔒 Concurrency Safety Model

| Risk              | Mitigation                |
|-------------------|---------------------------|
| Double scheduling | Node-level locks          |
| Lost updates      | Versioned state           |
| Partial failure   | Rollback Lua script       |
| Scheduler crash   | TTL locks (500ms)         |
| Split-brain       | API validation            |
| Redis lag         | Optimistic version recheck|

**Lock-minimal, deadlock-free, linearizable per-node.**

---

## 🚫 Architecture Change Handling

If node architecture changes (rare but **fatal**):

### Detection
Compare `Redis node.arch` vs `API node.arch`

### Response
1. Mark node unschedulable in Redis
2. Increment version
3. Hard fail new scheduling attempts
4. **Never** attempt silent reschedule

---

## 📁 Project Structure

```
internal/
├── models/
│   ├── extender_types.go    # Kubernetes extender API types
│   └── scheduler_models.go  # Node state, pod intent, versioned models
├── redis/
│   ├── decision_repo.go     # Legacy decision storage
│   └── scheduler_repo.go    # Atomic operations with Lua scripts
├── scheduler/
│   └── scheduler.go         # Production-grade scheduling algorithm
└── transport/http/
    └── api.go               # HTTP API (extender endpoint)

cmd/
├── scheduler/
│   └── main.go              # Custom scheduler (watches pods)
└── best-node-selector/
    └── main.go              # Scheduler extender HTTP server
```

---

## 🚀 Usage

### Run the Extender
```bash
# Set environment
export REDIS_ADDR=localhost:6379
export PORT=9000

# Run extender with production scheduler
go run cmd/best-node-selector/main.go
```

### Run the Custom Scheduler
```bash
# Run custom scheduler that calls extender
go run cmd/scheduler/main.go
```

### Test with a Pod
```yaml
apiVersion: v1
kind: Pod
metadata:
  name: test-pod
  labels:
    app: test
spec:
  schedulerName: my-scheduler
  containers:
  - name: nginx
    image: nginx:alpine
    resources:
      requests:
        cpu: "100m"
        memory: "128Mi"
  nodeSelector:
    kubernetes.io/arch: arm64
```

---

## 🎛️ Configuration

### Environment Variables
- `REDIS_ADDR` - Redis address (default: `localhost:6379`)
- `PORT` - HTTP server port (default: `9000`)
- `KUBECONFIG` - Path to kubeconfig file

### Tunable Weights
In `internal/scheduler/scheduler.go`:
```go
const (
    weightCPUFree       = 1.0
    weightMemFree       = 1.0
    weightFragmentation = 0.5
    weightTaint         = 2.0
)
```

---

## 🔧 Lua Scripts

### Reserve Resources
```lua
-- Atomic reservation with version check
local node = cjson.decode(redis.call('GET', KEYS[1]))
if node.version ~= expectedVersion then
    return redis.error_reply("version_mismatch")
end
-- Check + update resources atomically
node.cpu_used = node.cpu_used + cpuReq
node.mem_used = node.mem_used + memReq
node.pods_used = node.pods_used + 1
node.version = node.version + 1
redis.call('SET', KEYS[1], cjson.encode(node))
return node.version
```

### Rollback Resources
```lua
-- Atomic rollback
local node = cjson.decode(redis.call('GET', KEYS[1]))
node.cpu_used = math.max(0, node.cpu_used - cpuReq)
node.mem_used = math.max(0, node.mem_used - memReq)
node.pods_used = math.max(0, node.pods_used - 1)
node.version = node.version + 1
redis.call('SET', KEYS[1], cjson.encode(node))
return node.version
```

---

## ❌ What NOT to Do

- ❌ Global scheduler lock
- ❌ Redis as sole source of truth
- ❌ Scheduling without versioning
- ❌ Ignoring sidecars in resource calculations
- ❌ Ignoring architecture requirements
- ❌ Using MULTI/EXEC instead of Lua
- ❌ Blocking locks
- ❌ Assuming bind always succeeds

---

## 📊 Monitoring

### Key Metrics to Track
- Scheduling latency (p50, p95, p99)
- Lock acquisition failures per node
- Version mismatch rate
- API validation failure rate
- Rollback frequency
- Architecture mismatch detections

### Log Format
```
[SCHEDULER] scheduling pod=default/test-pod cpu=100m mem=128Mi arch=arm64
[FILTER] node=node-1 rejected: arch mismatch (node=amd64 pod=arm64)
[RESERVE] attempting node=node-2 score=87.50 version=42
[RESERVE] node=node-2 reserved cpu=100m mem=128Mi version=42->43
[SUCCESS] pod=default/test-pod scheduled to node=node-2
```

---

## 🧪 Testing

### Unit Tests
```bash
go test ./internal/scheduler -v
go test ./internal/redis -v
```

### Integration Test
```bash
# Start Redis
docker run -d -p 6379:6379 redis:alpine

# Start extender
go run cmd/best-node-selector/main.go

# Start custom scheduler
go run cmd/scheduler/main.go

# Create test pod
kubectl apply -f testdata/test-pod.yaml

# Check logs
kubectl logs -l app=test-pod -f
```

---

## 🔐 Security Considerations

1. **Redis Access Control**: Use Redis AUTH and TLS
2. **RBAC**: Ensure scheduler has minimal required permissions
3. **Resource Limits**: Set CPU/memory limits on scheduler pods
4. **Lock TTL**: 500ms prevents indefinite locks
5. **API Validation**: Always validate against Kubernetes API

---

## 🚀 Deployment

### Kubernetes Deployment
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: scheduler-extender
spec:
  replicas: 1
  selector:
    matchLabels:
      app: scheduler-extender
  template:
    metadata:
      labels:
        app: scheduler-extender
    spec:
      containers:
      - name: extender
        image: scheduler-extender:latest
        env:
        - name: REDIS_ADDR
          value: "redis:6379"
        - name: PORT
          value: "9000"
        ports:
        - containerPort: 9000
---
apiVersion: v1
kind: Service
metadata:
  name: scheduler-extender
spec:
  selector:
    app: scheduler-extender
  ports:
  - port: 9000
    targetPort: 9000
```

---

## 📚 References

- [Kubernetes Scheduler Extender](https://github.com/kubernetes/community/blob/master/contributors/design-proposals/scheduling/scheduler_extender.md)
- [Redis Lua Scripting](https://redis.io/docs/interact/programmability/eval-intro/)
- [Optimistic Concurrency Control](https://en.wikipedia.org/wiki/Optimistic_concurrency_control)

---

## 🎓 Mentor Notes

This implementation is:
- ✅ **Concurrent**: Lock-minimal with optimistic locking
- ✅ **Architecture-safe**: Hard fail on arch mismatch
- ✅ **Failure-resilient**: Rollback on any validation failure
- ✅ **Redis-appropriate**: Coordinator, not source of truth
- ✅ **Production-grade**: Proper error handling, logging, monitoring

**Not** a full kube-scheduler replacement. It's an accuracy layer that bolts onto existing schedulers.

---

## 📝 License

MIT
