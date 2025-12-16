package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"scheduler-extender/internal/models"

	goredis "github.com/redis/go-redis/v9"
	"github.com/google/uuid"
)

const (
	lockTTL = 500 * time.Millisecond
	nodeLockKeyPrefix = "lock:node:"
	podLockKeyPrefix  = "lock:pod:"
	nodeKeyPrefix     = "node:"
	podIntentPrefix   = "pod:"
)

// Lua script for atomic reservation with version check
const reserveLuaScript = `
local nodeKey = KEYS[1]
local nodeData = redis.call('GET', nodeKey)

if not nodeData then
    return redis.error_reply("node_not_found")
end

local node = cjson.decode(nodeData)
local cpuReq = tonumber(ARGV[1])
local memReq = tonumber(ARGV[2])
local expectedVersion = tonumber(ARGV[3])

-- Version check
if node.version ~= expectedVersion then
    return redis.error_reply("version_mismatch")
end

-- Resource check
local cpuFree = node.cpu_allocatable_m - node.cpu_used_m
local memFree = node.mem_allocatable_bytes - node.mem_used_bytes

if cpuFree < cpuReq or memFree < memReq or node.pods_used >= node.pods_allocatable then
    return redis.error_reply("insufficient_resources")
end

-- Atomic update
node.cpu_used_m = node.cpu_used_m + cpuReq
node.mem_used_bytes = node.mem_used_bytes + memReq
node.pods_used = node.pods_used + 1
node.version = node.version + 1

redis.call('SET', nodeKey, cjson.encode(node))
return node.version
`

// Lua script for atomic rollback
const rollbackLuaScript = `
local nodeKey = KEYS[1]
local nodeData = redis.call('GET', nodeKey)

if not nodeData then
    return redis.error_reply("node_not_found")
end

local node = cjson.decode(nodeData)
local cpuReq = tonumber(ARGV[1])
local memReq = tonumber(ARGV[2])

-- Rollback resources
node.cpu_used_m = math.max(0, node.cpu_used_m - cpuReq)
node.mem_used_bytes = math.max(0, node.mem_used_bytes - memReq)
node.pods_used = math.max(0, node.pods_used - 1)
node.version = node.version + 1

redis.call('SET', nodeKey, cjson.encode(node))
return node.version
`

// SchedulerRepository handles all Redis operations for scheduler
type SchedulerRepository struct {
	rdb             *goredis.Client
	reserveScript   *goredis.Script
	rollbackScript  *goredis.Script
}

func NewSchedulerRepository(addr string) *SchedulerRepository {
	rdb := goredis.NewClient(&goredis.Options{
		Addr: addr,
	})

	return &SchedulerRepository{
		rdb:            rdb,
		reserveScript:  goredis.NewScript(reserveLuaScript),
		rollbackScript: goredis.NewScript(rollbackLuaScript),
	}
}

func (r *SchedulerRepository) Close() error {
	return r.rdb.Close()
}

// GetNodeState retrieves node state from Redis
func (r *SchedulerRepository) GetNodeState(ctx context.Context, nodeName string) (*models.NodeState, error) {
	key := nodeKeyPrefix + nodeName
	data, err := r.rdb.Get(ctx, key).Result()
	if err == goredis.Nil {
		return nil, fmt.Errorf("node not found: %s", nodeName)
	}
	if err != nil {
		return nil, err
	}

	var state models.NodeState
	if err := json.Unmarshal([]byte(data), &state); err != nil {
		return nil, err
	}

	return &state, nil
}

// SetNodeState updates node state in Redis
func (r *SchedulerRepository) SetNodeState(ctx context.Context, state *models.NodeState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}

	key := nodeKeyPrefix + state.Name
	return r.rdb.Set(ctx, key, data, 0).Err()
}

// ListNodeStates retrieves all node states
func (r *SchedulerRepository) ListNodeStates(ctx context.Context) ([]*models.NodeState, error) {
	pattern := nodeKeyPrefix + "*"
	
	var cursor uint64
	var states []*models.NodeState

	for {
		keys, next, err := r.rdb.Scan(ctx, cursor, pattern, 50).Result()
		if err != nil {
			return nil, err
		}

		for _, key := range keys {
			data, err := r.rdb.Get(ctx, key).Result()
			if err != nil {
				continue // Skip expired/race
			}

			var state models.NodeState
			if err := json.Unmarshal([]byte(data), &state); err != nil {
				continue
			}

			states = append(states, &state)
		}

		cursor = next
		if cursor == 0 {
			break
		}
	}

	return states, nil
}

// LockNode acquires a distributed lock on a node
func (r *SchedulerRepository) LockNode(ctx context.Context, nodeName string) (string, error) {
	lockKey := nodeLockKeyPrefix + nodeName
	lockUUID := uuid.New().String()

	ok, err := r.rdb.SetNX(ctx, lockKey, lockUUID, lockTTL).Result()
	if err != nil {
		return "", err
	}

	if !ok {
		return "", fmt.Errorf("lock already held")
	}

	return lockUUID, nil
}

// UnlockNode releases a distributed lock
func (r *SchedulerRepository) UnlockNode(ctx context.Context, nodeName, lockUUID string) error {
	lockKey := nodeLockKeyPrefix + nodeName
	
	// Only delete if the UUID matches (prevent unlocking someone else's lock)
	script := `
		if redis.call("GET", KEYS[1]) == ARGV[1] then
			return redis.call("DEL", KEYS[1])
		else
			return 0
		end
	`
	
	return r.rdb.Eval(ctx, script, []string{lockKey}, lockUUID).Err()
}

// ReserveResources atomically reserves resources on a node with version check
func (r *SchedulerRepository) ReserveResources(
	ctx context.Context,
	nodeName string,
	cpuMillis int64,
	memBytes int64,
	expectedVersion int64,
) (newVersion int64, err error) {
	
	nodeKey := nodeKeyPrefix + nodeName
	
	result, err := r.reserveScript.Run(
		ctx,
		r.rdb,
		[]string{nodeKey},
		cpuMillis,
		memBytes,
		expectedVersion,
	).Result()

	if err != nil {
		return 0, err
	}

	version, ok := result.(int64)
	if !ok {
		return 0, fmt.Errorf("unexpected result type")
	}

	return version, nil
}

// RollbackResources atomically rolls back a reservation
func (r *SchedulerRepository) RollbackResources(
	ctx context.Context,
	nodeName string,
	cpuMillis int64,
	memBytes int64,
) (newVersion int64, err error) {
	
	nodeKey := nodeKeyPrefix + nodeName
	
	result, err := r.rollbackScript.Run(
		ctx,
		r.rdb,
		[]string{nodeKey},
		cpuMillis,
		memBytes,
	).Result()

	if err != nil {
		return 0, err
	}

	version, ok := result.(int64)
	if !ok {
		return 0, fmt.Errorf("unexpected result type")
	}

	return version, nil
}

// SavePodIntent stores pod scheduling intent
func (r *SchedulerRepository) SavePodIntent(ctx context.Context, pod *models.PodIntent) error {
	data, err := json.Marshal(pod)
	if err != nil {
		return err
	}

	key := podIntentPrefix + pod.UID
	return r.rdb.Set(ctx, key, data, 10*time.Minute).Err()
}

// GetPodIntent retrieves pod scheduling intent
func (r *SchedulerRepository) GetPodIntent(ctx context.Context, uid string) (*models.PodIntent, error) {
	key := podIntentPrefix + uid
	data, err := r.rdb.Get(ctx, key).Result()
	if err == goredis.Nil {
		return nil, fmt.Errorf("pod intent not found: %s", uid)
	}
	if err != nil {
		return nil, err
	}

	var intent models.PodIntent
	if err := json.Unmarshal([]byte(data), &intent); err != nil {
		return nil, err
	}

	return &intent, nil
}
