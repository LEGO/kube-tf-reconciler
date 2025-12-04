package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	WorkspacePhase = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "kube_tf_workspace_phase",
			Help: "State of Terraform workspace in specific phase (value is 1 for the current phase)",
		},
		[]string{"namespace", "workspace", "phase"},
	)

	WorkspacePhaseTimestamp = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "kube_tf_workspace_phase_timestamp",
			Help: "Unix timestamp when this phase was set, used because multiple pods can work on the same workspace",
		},
		[]string{"namespace", "workspace", "phase"},
	)

	WorkspaceReconciliations = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kube_tf_workspace_reconciliations_total",
			Help: "Total number of reconciliation attempts for each workspace",
		},
		[]string{"namespace", "workspace"},
	)
)

func init() {
	metrics.Registry.Unregister(WorkspacePhase)
	metrics.Registry.Unregister(WorkspaceReconciliations)

	metrics.Registry.MustRegister(
		WorkspacePhase,
		WorkspaceReconciliations,
		WorkspacePhaseTimestamp,
	)
}

func SetWorkspacePhase(namespace, workspace, phase string) {
	WorkspacePhase.DeletePartialMatch(prometheus.Labels{
		"namespace": namespace,
		"workspace": workspace,
	})

	WorkspacePhaseTimestamp.DeletePartialMatch(prometheus.Labels{
		"namespace": namespace,
		"workspace": workspace,
	})

	if phase != "" {
		WorkspacePhase.WithLabelValues(namespace, workspace, phase).Set(1)
		WorkspacePhaseTimestamp.WithLabelValues(namespace, workspace, phase).SetToCurrentTime()
	}
}

func CleanupWorkspaceMetrics(namespace, workspace string) {
	WorkspacePhase.DeletePartialMatch(prometheus.Labels{
		"namespace": namespace,
		"workspace": workspace,
	})
}

func RecordReconciliation(namespace, workspace string) {
	WorkspaceReconciliations.WithLabelValues(namespace, workspace).Inc()
}
