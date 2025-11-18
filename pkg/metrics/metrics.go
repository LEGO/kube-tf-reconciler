package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	WorkspacePhase = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "kube_tf_workspace_phase",
			Help: "Current phase of the Terraform workspace (0=Idle, 1=Planning, 2=Applying, 3=Completed, 4=Errored)",
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
