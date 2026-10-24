package controller

import (
	"context"
	"log/slog"
	"sync"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

type NodeWatcher struct {
	mu                 sync.RWMutex
	nodes              map[string]model.NodeInfo
	schedulable        map[string]bool
	failureDomainLabel string
	stopCh             chan struct{}
	stopOnce           sync.Once

	onNodesChange func()
	onZoneChange  func(nodeName, zone string)
}

func (nw *NodeWatcher) Stop() {
	nw.stopOnce.Do(func() { close(nw.stopCh) })
}

// OnNodesChange registers a callback invoked whenever a node is added, updated or removed. It is
// called synchronously from the informer event handlers, without nw.mu held.
func (nw *NodeWatcher) OnNodesChange(fn func()) {
	nw.mu.Lock()
	nw.onNodesChange = fn
	nw.mu.Unlock()
}

// ExpectedAgents counts the nodes expected to run an agent: every schedulable node, and a cordoned
// one while hasAgent reports its agent registered. The DaemonSet keeps running on a cordoned node,
// so that agent must not offset a missing one elsewhere; a cordoned node whose agent went away is
// drained or powered off for maintenance and is missing nothing.
func (nw *NodeWatcher) ExpectedAgents(hasAgent func(nodeName string) bool) int {
	nw.mu.RLock()
	defer nw.mu.RUnlock()

	count := 0
	for name, ok := range nw.schedulable {
		if ok || hasAgent(name) {
			count++
		}
	}
	return count
}

// notifyNodes reports a node change to the registered callback. Caller must not hold nw.mu.
func (nw *NodeWatcher) notifyNodes() {
	nw.mu.RLock()
	fn := nw.onNodesChange
	nw.mu.RUnlock()
	if fn != nil {
		fn()
	}
}

func (nw *NodeWatcher) GetNodes() []model.NodeInfo {
	nw.mu.RLock()
	defer nw.mu.RUnlock()

	nodes := make([]model.NodeInfo, 0, len(nw.nodes))
	for _, n := range nw.nodes {
		nodes = append(nodes, n)
	}
	return nodes
}

// ZoneFor returns the failure-domain zone for nodeName, or "" if the node is
// unknown or has no zone label.
func (nw *NodeWatcher) ZoneFor(nodeName string) string {
	nw.mu.RLock()
	defer nw.mu.RUnlock()

	if n, ok := nw.nodes[nodeName]; ok {
		return n.Zone
	}
	return ""
}

// OnZoneChange registers a callback invoked whenever a node's resolved zone
// changes (including the first time a zone is observed). It is called
// synchronously from the informer event handlers.
func (nw *NodeWatcher) OnZoneChange(fn func(nodeName, zone string)) {
	nw.mu.Lock()
	nw.onZoneChange = fn
	nw.mu.Unlock()
}

// notifyZone reports a zone change to the registered callback.
// Caller must not hold nw.mu.
func (nw *NodeWatcher) notifyZone(nodeName, zone string) {
	nw.mu.RLock()
	fn := nw.onZoneChange
	nw.mu.RUnlock()
	if fn != nil {
		fn(nodeName, zone)
	}
}

func (nw *NodeWatcher) onNodeEvent(obj any) {
	node, ok := obj.(*corev1.Node)
	if !ok {
		return
	}

	info := model.NodeInfo{
		Name:   node.Name,
		Zone:   node.Labels[nw.failureDomainLabel],
		Labels: node.Labels,
		Ready:  isNodeReady(node),
	}

	nw.mu.Lock()
	prev, existed := nw.nodes[node.Name]
	nw.nodes[node.Name] = info
	nw.schedulable[node.Name] = !node.Spec.Unschedulable
	nw.mu.Unlock()

	slog.Debug("node updated", "name", node.Name, "zone", info.Zone, "ready", info.Ready,
		"schedulable", !node.Spec.Unschedulable)

	/* A zone that was REMOVED is a change like any other.
	   The old guard (`info.Zone != "" && ...`) skipped the notification when a node's
	   topology.kubernetes.io/zone label was deleted or renamed away, so every agent on that node kept
	   the old zone in the registry — served by GET /api/v1/topology and shipped to the whole fleet in
	   AgentMeta.zone, which made peers classify probes as cross-zone against a zone that no longer
	   exists. Registry.UpdateZone already handles "" correctly. */
	zoneChanged := (!existed && info.Zone != "") || (existed && prev.Zone != info.Zone)

	nw.notifyNodes()
	if zoneChanged {
		nw.notifyZone(node.Name, info.Zone)
	}
}

func (nw *NodeWatcher) onNodeDelete(obj any) {
	node, ok := obj.(*corev1.Node)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		node, ok = tombstone.Obj.(*corev1.Node)
		if !ok {
			return
		}
	}

	nw.mu.Lock()
	delete(nw.nodes, node.Name)
	delete(nw.schedulable, node.Name)
	nw.mu.Unlock()

	slog.Info("node removed", "name", node.Name)

	nw.notifyNodes()
}

func isNodeReady(node *corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// NewNodeWatcherWithContext creates a NodeWatcher that stops when ctx is cancelled.
func NewNodeWatcherWithContext(ctx context.Context, clientset kubernetes.Interface, failureDomainLabel string) *NodeWatcher {
	nw := &NodeWatcher{
		nodes:              make(map[string]model.NodeInfo),
		schedulable:        make(map[string]bool),
		failureDomainLabel: failureDomainLabel,
		stopCh:             make(chan struct{}),
	}

	go func() {
		<-ctx.Done()
		nw.Stop()
	}()

	factory := informers.NewSharedInformerFactory(clientset, 0)
	nodeInformer := factory.Core().V1().Nodes().Informer()

	if _, err := nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { nw.onNodeEvent(obj) },
		UpdateFunc: func(_, obj any) { nw.onNodeEvent(obj) },
		DeleteFunc: func(obj any) { nw.onNodeDelete(obj) },
	}); err != nil {
		slog.Warn("failed to register node event handler", "error", err)
	}

	factory.Start(nw.stopCh)
	factory.WaitForCacheSync(nw.stopCh)

	return nw
}
