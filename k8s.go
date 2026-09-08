// k8sClient wraps the in-cluster Kubernetes API access this agent uses to
// answer commands -- always its own ServiceAccount's credentials
// (rest.InClusterConfig()), never anything supplied by InfraHub's
// backend. InfraHub never sees these credentials or any client-go
// internals; only the safe, small PodInfo/ServerVersionResult shapes
// defined in protocol.go ever leave this process.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	metricsclientset "k8s.io/metrics/pkg/client/clientset/versioned"
)

type k8sClient struct {
	clientset *kubernetes.Clientset
	// metrics is nil when metrics-server isn't installed or reachable --
	// every caller treats that as "no metrics available" (nil CPU/Memory
	// fields), never a fatal error.
	metrics *metricsclientset.Clientset
}

func newK8sClient() (*k8sClient, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("load in-cluster config (is this running inside a pod?): %w", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build kubernetes client: %w", err)
	}
	metrics, err := metricsclientset.NewForConfig(cfg)
	if err != nil {
		metrics = nil
	}
	return &k8sClient{clientset: clientset, metrics: metrics}, nil
}

// ServerVersion answers the "server_version" command.
func (c *k8sClient) ServerVersion(ctx context.Context) (string, error) {
	_ = ctx // client-go's Discovery client has no context-aware variant here
	v, err := c.clientset.Discovery().ServerVersion()
	if err != nil {
		return "", err
	}
	return v.String(), nil
}

// ListPods answers the "list_pods" command -- namespace == "" lists every
// namespace. Metrics are always best-effort: a pod simply has nil
// CPU/Memory fields when metrics-server isn't installed, never a
// fabricated value.
func (c *k8sClient) ListPods(ctx context.Context, namespace string) ([]PodInfo, error) {
	pods, err := c.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	type usage struct{ cpuMillicores, memoryBytes int64 }
	metricsByPod := map[string]usage{}
	if c.metrics != nil {
		if podMetrics, err := c.metrics.MetricsV1beta1().PodMetricses(namespace).List(ctx, metav1.ListOptions{}); err == nil {
			for _, pm := range podMetrics.Items {
				var u usage
				for _, cont := range pm.Containers {
					if q := cont.Usage.Cpu(); q != nil {
						u.cpuMillicores += q.MilliValue()
					}
					if q := cont.Usage.Memory(); q != nil {
						u.memoryBytes += q.Value()
					}
				}
				metricsByPod[pm.Namespace+"/"+pm.Name] = u
			}
		}
	}

	result := make([]PodInfo, 0, len(pods.Items))
	for _, pod := range pods.Items {
		var ready, total, restarts int32
		for _, cs := range pod.Status.ContainerStatuses {
			total++
			if cs.Ready {
				ready++
			}
			restarts += cs.RestartCount
		}
		info := PodInfo{
			Namespace: pod.Namespace, PodName: pod.Name, NodeName: pod.Spec.NodeName,
			Phase: string(pod.Status.Phase), ReadyContainers: ready, TotalContainers: total, RestartCount: restarts,
		}
		if pod.Status.StartTime != nil {
			info.StartedAt = pod.Status.StartTime.Time.UTC().Format(time.RFC3339)
		}
		if u, ok := metricsByPod[pod.Namespace+"/"+pod.Name]; ok {
			cpu, mem := u.cpuMillicores, u.memoryBytes
			info.CPUMillicores, info.MemoryBytes = &cpu, &mem
		}
		result = append(result, info)
	}
	return result, nil
}

// FetchLogsSince answers the "fetch_logs_since" command -- a single
// bounded, non-follow read of everything logged at or after since
// (the zero time means "from the beginning of the container's current log
// buffer"). Every line is timestamped, matching the backend's expected
// `<RFC3339Nano> <text>` capture format exactly.
func (c *k8sClient) FetchLogsSince(ctx context.Context, namespace, podName string, since time.Time) (string, error) {
	opts := &corev1.PodLogOptions{Timestamps: true}
	if !since.IsZero() {
		t := metav1.NewTime(since)
		opts.SinceTime = &t
	}
	stream, err := c.clientset.CoreV1().Pods(namespace).GetLogs(podName, opts).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	data, err := io.ReadAll(stream)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ListNodes answers the "list_nodes" command -- every node's capacity/
// allocatable figures plus best-effort usage (metrics-server for CPU/
// memory, the kubelet's own stats/summary proxy for storage) and pod
// count. Never fails just because usage figures aren't available -- only
// the base node List/Pods List calls (both required) can fail this call
// outright.
func (c *k8sClient) ListNodes(ctx context.Context) ([]NodeInfo, error) {
	nodes, err := c.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	podCountByNode := map[string]int32{}
	if pods, err := c.clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{}); err == nil {
		for _, p := range pods.Items {
			if p.Spec.NodeName != "" {
				podCountByNode[p.Spec.NodeName]++
			}
		}
	}

	type usage struct{ cpuMillicores, memoryBytes int64 }
	usageByNode := map[string]usage{}
	if c.metrics != nil {
		if nodeMetrics, err := c.metrics.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{}); err == nil {
			for _, nm := range nodeMetrics.Items {
				var u usage
				if q := nm.Usage.Cpu(); q != nil {
					u.cpuMillicores = q.MilliValue()
				}
				if q := nm.Usage.Memory(); q != nil {
					u.memoryBytes = q.Value()
				}
				usageByNode[nm.Name] = u
			}
		}
	}

	result := make([]NodeInfo, 0, len(nodes.Items))
	for _, node := range nodes.Items {
		info := NodeInfo{
			Name: node.Name, KubeletVersion: node.Status.NodeInfo.KubeletVersion, OSImage: node.Status.NodeInfo.OSImage,
			PodCount: podCountByNode[node.Name],
		}
		for _, cond := range node.Status.Conditions {
			if cond.Type == corev1.NodeReady {
				info.Ready = cond.Status == corev1.ConditionTrue
				break
			}
		}
		for label := range node.Labels {
			if role, ok := strings.CutPrefix(label, "node-role.kubernetes.io/"); ok {
				info.Roles = append(info.Roles, role)
			}
		}
		if q, ok := node.Status.Capacity[corev1.ResourceCPU]; ok {
			info.CPUCapacityMillicores = q.MilliValue()
		}
		if q, ok := node.Status.Allocatable[corev1.ResourceCPU]; ok {
			info.CPUAllocatableMillicores = q.MilliValue()
		}
		if q, ok := node.Status.Capacity[corev1.ResourceMemory]; ok {
			info.MemoryCapacityBytes = q.Value()
		}
		if q, ok := node.Status.Allocatable[corev1.ResourceMemory]; ok {
			info.MemoryAllocatableBytes = q.Value()
		}
		if q, ok := node.Status.Capacity[corev1.ResourcePods]; ok {
			info.PodCapacity = q.Value()
		}
		if u, ok := usageByNode[node.Name]; ok {
			cpu, mem := u.cpuMillicores, u.memoryBytes
			info.CPUUsageMillicores, info.MemoryUsageBytes = &cpu, &mem
		}
		if capBytes, usedBytes, ok := c.nodeStorageStats(ctx, node.Name); ok {
			info.StorageCapacityBytes, info.StorageUsageBytes = &capBytes, &usedBytes
		}
		result = append(result, info)
	}
	return result, nil
}

// nodeStorageStats is best-effort: the kubelet's stats/summary endpoint is
// reached through the API server's node proxy (requires the "nodes/proxy"
// RBAC verb -- see deploy/manifest.yaml). Any failure (RBAC not granted,
// node unreachable, unexpected response shape) simply means no storage
// figures for this one node, never a fatal error for the whole ListNodes
// call.
func (c *k8sClient) nodeStorageStats(ctx context.Context, nodeName string) (capacityBytes, usedBytes int64, ok bool) {
	raw, err := c.clientset.CoreV1().RESTClient().Get().
		Resource("nodes").Name(nodeName).SubResource("proxy").Suffix("stats/summary").
		Do(ctx).Raw()
	if err != nil {
		return 0, 0, false
	}
	var summary struct {
		Node struct {
			Fs struct {
				CapacityBytes *int64 `json:"capacityBytes"`
				UsedBytes     *int64 `json:"usedBytes"`
			} `json:"fs"`
		} `json:"node"`
	}
	if err := json.Unmarshal(raw, &summary); err != nil {
		return 0, 0, false
	}
	if summary.Node.Fs.CapacityBytes == nil || summary.Node.Fs.UsedBytes == nil {
		return 0, 0, false
	}
	return *summary.Node.Fs.CapacityBytes, *summary.Node.Fs.UsedBytes, true
}

// ClusterResourceSummary answers the "cluster_resource_summary" command --
// cluster-wide counts of exactly the resource kinds this agent's RBAC (see
// deploy/manifest.yaml) grants read access to. Unlike node usage figures
// above, a failure here (e.g. RBAC not yet upgraded to match this agent
// version) fails the whole call -- every one of these List calls is
// expected to succeed given the manifest's RBAC, so a failure signals a
// real misconfiguration worth surfacing rather than silently under-
// counting.
func (c *k8sClient) ClusterResourceSummary(ctx context.Context) (ClusterResourceSummary, error) {
	var summary ClusterResourceSummary

	namespaces, err := c.clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return ClusterResourceSummary{}, fmt.Errorf("list namespaces: %w", err)
	}
	summary.Namespaces = int32(len(namespaces.Items))

	nodes, err := c.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return ClusterResourceSummary{}, fmt.Errorf("list nodes: %w", err)
	}
	summary.Nodes = int32(len(nodes.Items))

	pods, err := c.clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return ClusterResourceSummary{}, fmt.Errorf("list pods: %w", err)
	}
	summary.Pods = int32(len(pods.Items))

	deployments, err := c.clientset.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return ClusterResourceSummary{}, fmt.Errorf("list deployments: %w", err)
	}
	summary.Deployments = int32(len(deployments.Items))

	statefulSets, err := c.clientset.AppsV1().StatefulSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return ClusterResourceSummary{}, fmt.Errorf("list statefulsets: %w", err)
	}
	summary.StatefulSets = int32(len(statefulSets.Items))

	daemonSets, err := c.clientset.AppsV1().DaemonSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return ClusterResourceSummary{}, fmt.Errorf("list daemonsets: %w", err)
	}
	summary.DaemonSets = int32(len(daemonSets.Items))

	services, err := c.clientset.CoreV1().Services("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return ClusterResourceSummary{}, fmt.Errorf("list services: %w", err)
	}
	summary.Services = int32(len(services.Items))

	pvcs, err := c.clientset.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return ClusterResourceSummary{}, fmt.Errorf("list persistentvolumeclaims: %w", err)
	}
	summary.PersistentVolumeClaims = int32(len(pvcs.Items))

	return summary, nil
}

// StreamLogs answers the "stream_logs" command -- a live, follow tail,
// calling onLine once per line until ctx is cancelled (the backend's
// "stop_stream" command, or the browser disconnecting) or the pod's log
// stream itself ends.
func (c *k8sClient) StreamLogs(ctx context.Context, namespace, podName string, onLine func(string)) error {
	stream, err := c.clientset.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Follow: true, Timestamps: true,
	}).Stream(ctx)
	if err != nil {
		return err
	}
	defer stream.Close()

	scanner := bufio.NewScanner(stream)
	// A single log line can exceed bufio.Scanner's 64KiB default (e.g. a
	// large JSON blob printed on one line) -- 1MiB is generous without
	// being unbounded.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		onLine(scanner.Text())
	}
	return scanner.Err()
}
