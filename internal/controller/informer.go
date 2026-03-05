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
	failureDomainLabel string
	stopCh             chan struct{}
	stopOnce           sync.Once
}

func (nw *NodeWatcher) Stop() {
	nw.stopOnce.Do(func() { close(nw.stopCh) })
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

func (nw *NodeWatcher) GetNodeZone(nodeName string) string {
	nw.mu.RLock()
	defer nw.mu.RUnlock()

	if n, ok := nw.nodes[nodeName]; ok {
		return n.Zone
	}
	return ""
}

func (nw *NodeWatcher) onNodeEvent(obj interface{}) {
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
	nw.nodes[node.Name] = info
	nw.mu.Unlock()

	slog.Debug("node updated", "name", node.Name, "zone", info.Zone, "ready", info.Ready)
}

func (nw *NodeWatcher) onNodeDelete(obj interface{}) {
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
	nw.mu.Unlock()

	slog.Info("node removed", "name", node.Name)
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
		AddFunc:    func(obj interface{}) { nw.onNodeEvent(obj) },
		UpdateFunc: func(_, obj interface{}) { nw.onNodeEvent(obj) },
		DeleteFunc: func(obj interface{}) { nw.onNodeDelete(obj) },
	}); err != nil {
		slog.Warn("failed to register node event handler", "error", err)
	}

	factory.Start(nw.stopCh)
	factory.WaitForCacheSync(nw.stopCh)

	return nw
}
