// The wire protocol between this agent and InfraHub's backend -- a small,
// stable, JSON-over-WebSocket command/response protocol. This is a
// deliberate, hand-kept-in-sync duplicate of the backend's own copy
// (vmcontrolcenter/backend/internal/services/k8s_client.go): the two are
// genuinely separate deployables (this agent runs inside a user's
// cluster, versioned and upgraded independently of the backend), so a
// shared Go module would only add coupling without much benefit for a
// protocol this small. If you change one side, change the other.
package main

import "encoding/json"

// CommandType enumerates every command the backend can send.
type CommandType string

const (
	CmdServerVersion          CommandType = "server_version"
	CmdListPods               CommandType = "list_pods"
	CmdFetchLogsSince         CommandType = "fetch_logs_since"
	CmdStreamLogs             CommandType = "stream_logs"
	CmdStopStream             CommandType = "stop_stream"
	CmdListNodes              CommandType = "list_nodes"
	CmdClusterResourceSummary CommandType = "cluster_resource_summary"
)

// Command is one backend -> agent message.
type Command struct {
	ID        string      `json:"id"`
	Type      CommandType `json:"type"`
	Namespace string      `json:"namespace,omitempty"`
	PodName   string      `json:"pod_name,omitempty"`
	Since     string      `json:"since,omitempty"`
}

// MessageType enumerates every message this agent can send back.
type MessageType string

const (
	MsgResult  MessageType = "result"
	MsgLogLine MessageType = "log_line"
	MsgDone    MessageType = "done"
	MsgError   MessageType = "error"
)

// Message is one agent -> backend message.
type Message struct {
	ID      string          `json:"id"`
	Type    MessageType     `json:"type"`
	Data    json.RawMessage `json:"data,omitempty"`
	Line    string          `json:"line,omitempty"`
	Message string          `json:"message,omitempty"`
}

// PodInfo is what "list_pods" returns, one entry per pod.
type PodInfo struct {
	Namespace       string `json:"namespace"`
	PodName         string `json:"pod_name"`
	NodeName        string `json:"node_name,omitempty"`
	Phase           string `json:"phase"`
	ReadyContainers int32  `json:"ready_containers"`
	TotalContainers int32  `json:"total_containers"`
	RestartCount    int32  `json:"restart_count"`
	CPUMillicores   *int64 `json:"cpu_millicores,omitempty"`
	MemoryBytes     *int64 `json:"memory_bytes,omitempty"`
	StartedAt       string `json:"started_at,omitempty"`
}

// ServerVersionResult is what "server_version" returns.
type ServerVersionResult struct {
	Version string `json:"version"`
}

// NodeInfo is what "list_nodes" returns, one entry per cluster node.
// Usage fields (CPU/Memory/Storage) are always best-effort: nil when
// metrics-server or the node's kubelet stats API isn't reachable, never a
// fabricated value -- matching PodInfo's own convention.
type NodeInfo struct {
	Name                     string   `json:"name"`
	Ready                    bool     `json:"ready"`
	Roles                    []string `json:"roles,omitempty"`
	KubeletVersion           string   `json:"kubelet_version,omitempty"`
	OSImage                  string   `json:"os_image,omitempty"`
	CPUCapacityMillicores    int64    `json:"cpu_capacity_millicores"`
	CPUAllocatableMillicores int64    `json:"cpu_allocatable_millicores"`
	CPUUsageMillicores       *int64   `json:"cpu_usage_millicores,omitempty"`
	MemoryCapacityBytes      int64    `json:"memory_capacity_bytes"`
	MemoryAllocatableBytes   int64    `json:"memory_allocatable_bytes"`
	MemoryUsageBytes         *int64   `json:"memory_usage_bytes,omitempty"`
	StorageCapacityBytes     *int64   `json:"storage_capacity_bytes,omitempty"`
	StorageUsageBytes        *int64   `json:"storage_usage_bytes,omitempty"`
	PodCapacity              int64    `json:"pod_capacity,omitempty"`
	PodCount                 int32    `json:"pod_count"`
}

// ClusterResourceSummary is what "cluster_resource_summary" returns --
// cluster-wide counts of the resource kinds this agent's RBAC is scoped to
// read. Deliberately excludes Secrets/ConfigMaps: the ClusterRole in
// deploy/manifest.yaml never grants access to them, so a compromised or
// curious backend can't enumerate a cluster's secret material through this
// agent, even just names.
type ClusterResourceSummary struct {
	Namespaces             int32 `json:"namespaces"`
	Nodes                  int32 `json:"nodes"`
	Pods                   int32 `json:"pods"`
	Deployments            int32 `json:"deployments"`
	StatefulSets           int32 `json:"stateful_sets"`
	DaemonSets             int32 `json:"daemon_sets"`
	Services               int32 `json:"services"`
	PersistentVolumeClaims int32 `json:"persistent_volume_claims"`
}
