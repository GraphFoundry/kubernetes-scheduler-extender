package models

// NodeState represents the versioned state of a node in Redis
// Version is MANDATORY for optimistic locking
type NodeState struct {
	Name            string   `json:"name"`
	Arch            string   `json:"arch"`
	OS              string   `json:"os"`
	CPUAllocatable  int64    `json:"cpu_allocatable_m"`   // milliCPU
	MemAllocatable  int64    `json:"mem_allocatable_bytes"`
	CPUUsed         int64    `json:"cpu_used_m"`
	MemUsed         int64    `json:"mem_used_bytes"`
	PodsUsed        int      `json:"pods_used"`
	PodsAllocatable int      `json:"pods_allocatable"`
	Ready           bool     `json:"ready"`
	Taints          []string `json:"taints"`
	Version         int64    `json:"version"` // Monotonic version for optimistic locking
	Labels          map[string]string `json:"labels,omitempty"`
}

// PodIntent represents the scheduling intent for a pod
type PodIntent struct {
	UID            string            `json:"uid"`
	Namespace      string            `json:"namespace"`
	Name           string            `json:"name"`
	CPURequest     int64             `json:"cpu_request_m"`
	MemRequest     int64             `json:"mem_request_bytes"`
	Arch           string            `json:"arch"`
	OS             string            `json:"os"`
	Priority       int32             `json:"priority"`
	Scheduler      string            `json:"scheduler"`
	NodeSelector   map[string]string `json:"node_selector,omitempty"`
	Tolerations    []string          `json:"tolerations,omitempty"`
}

// SchedulingResult represents the outcome of a scheduling attempt
type SchedulingResult struct {
	PodUID      string `json:"pod_uid"`
	NodeName    string `json:"node_name"`
	Success     bool   `json:"success"`
	Reason      string `json:"reason,omitempty"`
	Score       int    `json:"score"`
	CPUReserved int64  `json:"cpu_reserved_m"`
	MemReserved int64  `json:"mem_reserved_bytes"`
	Timestamp   int64  `json:"timestamp"`
}

// ScoredNode represents a node with its scheduling score
type ScoredNode struct {
	Name  string
	Score float64
	State *NodeState
}

// FilterReason represents why a node was filtered out
type FilterReason string

const (
	ReasonNotReady          FilterReason = "node_not_ready"
	ReasonArchMismatch      FilterReason = "architecture_mismatch"
	ReasonOSMismatch        FilterReason = "os_mismatch"
	ReasonInsufficientCPU   FilterReason = "insufficient_cpu"
	ReasonInsufficientMemory FilterReason = "insufficient_memory"
	ReasonPodLimitReached   FilterReason = "pod_limit_reached"
	ReasonTaintNoToleration FilterReason = "taint_not_tolerated"
	ReasonNodeSelectorFail  FilterReason = "node_selector_mismatch"
	ReasonLockFailed        FilterReason = "lock_acquisition_failed"
	ReasonVersionMismatch   FilterReason = "version_mismatch"
	ReasonReservationFailed FilterReason = "reservation_failed"
	ReasonAPIValidationFail FilterReason = "api_validation_failed"
	ReasonBindFailed        FilterReason = "bind_failed"
)

// NodeLock represents a distributed lock on a node
type NodeLock struct {
	NodeName  string
	UUID      string
	ExpiresAt int64
}
