package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
)

// countingTopology is fakeTopology that also counts its reads.
type countingTopology struct {
	mu    sync.Mutex
	reads int
	topo  *controllerclient.Topology
	err   error
}

func (c *countingTopology) Topology(context.Context) (*controllerclient.Topology, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
	return c.topo, c.err
}

func (c *countingTopology) readCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}

// projectionBundle carries n enabled ad-hoc definitions selecting every agent.
func projectionBundle(n int) exportBundle {
	bundle := exportBundle{Version: exportBundleVersion}
	for i := range n {
		bundle.CheckDefinitions = append(bundle.CheckDefinitions, definitionResponse{
			ID: uuid.NewString(), Name: "edge-tcp-" + string(rune('a'+i)), SourceSelection: "all",
			DestinationKind: "adhoc", DestinationAddress: "8.8.8.8:53", CheckType: "tcp", Plane: "pod", Enabled: true,
		})
	}
	return bundle
}

func TestImportReadsTheProjectionTopologyOncePerImport(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		fx := newExportServer(t, "admin")
		topo := &countingTopology{topo: topologyWith(1, "zone-a")}
		fx.server.topology = topo

		bundle := projectionBundle(3)
		code, res := doImport(t, &fx, importRequest{DryRun: dryRun, Bundle: &bundle})
		if code != http.StatusOK {
			t.Fatalf("dryRun=%t: import = %d, want 200", dryRun, code)
		}
		if res.CheckDefinitions.Created != 3 {
			t.Errorf("dryRun=%t: definitions created = %d, want 3: %+v", dryRun, res.CheckDefinitions.Created, res.CheckDefinitions)
		}
		if got := topo.readCount(); got != 1 {
			t.Errorf("dryRun=%t: topology reads = %d for one import of 3 enabled definitions, want 1", dryRun, got)
		}
	}
}

func TestImportRefusesEveryDefinitionOverTheProjectionLimit(t *testing.T) {
	fx := newExportServer(t, "admin")
	fx.server.topology = &countingTopology{topo: topologyWith(maxProjectedSeries+1, "zone-a")}

	bundle := projectionBundle(2)
	code, res := doImport(t, &fx, importRequest{Bundle: &bundle})
	if code != http.StatusOK {
		t.Fatalf("import = %d, want 200", code)
	}
	if res.CheckDefinitions.Created != 0 || len(res.CheckDefinitions.Errors) != 2 {
		t.Fatalf("definitions = %+v, want both refused", res.CheckDefinitions)
	}
	for _, e := range res.CheckDefinitions.Errors {
		if !strings.Contains(e.Reason, tooManySeriesMsg) {
			t.Errorf("error %+v, want the projection refusal", e)
		}
	}
}

// A topology that cannot be read fails open for every definition, and each bypass is counted.
func TestImportProjectionGuardFailsOpenPerDefinition(t *testing.T) {
	fx := newExportServer(t, "admin")
	topo := &countingTopology{err: errors.New("no leader answered")}
	fx.server.topology = topo
	before := testutil.ToFloat64(fx.server.metrics.ProjectionGuardFailOpen.WithLabelValues())

	bundle := projectionBundle(2)
	code, res := doImport(t, &fx, importRequest{Bundle: &bundle})
	if code != http.StatusOK {
		t.Fatalf("import = %d, want 200", code)
	}
	if res.CheckDefinitions.Created != 2 {
		t.Errorf("definitions created = %d, want 2 (fail open): %+v", res.CheckDefinitions.Created, res.CheckDefinitions)
	}
	if got := testutil.ToFloat64(fx.server.metrics.ProjectionGuardFailOpen.WithLabelValues()) - before; got != 2 {
		t.Errorf("projection fail-open count grew by %v, want 2 (one per enabled definition)", got)
	}
	if got := topo.readCount(); got != 1 {
		t.Errorf("topology reads = %d, want 1: a failed read is not retried within one import", got)
	}
}
