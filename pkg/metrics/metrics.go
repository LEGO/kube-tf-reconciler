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
)

func init() {
	metrics.Registry.MustRegister(
		WorkspacePhase,
	)
}

func SetWorkspacePhase(namespace, workspace, phase string) {
	WorkspacePhase.DeletePartialMatch(prometheus.Labels{
		"namespace": namespace,
		"workspace": workspace,
	})

	if phase != "" {
		WorkspacePhase.WithLabelValues(namespace, workspace, phase).Set(1)
	}
}

func CleanupWorkspaceMetrics(namespace, workspace string) {
	WorkspacePhase.DeletePartialMatch(prometheus.Labels{
		"namespace": namespace,
		"workspace": workspace,
	})
}
