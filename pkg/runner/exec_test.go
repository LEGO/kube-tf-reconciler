package runner

import (
	"testing"

	tfreconcilev1alpha1 "github.com/LEGO/kube-tf-reconciler/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestChecksum(t *testing.T) {
	e := New("/Users/dkFleThe/src/kube-tf-reconciler/.testdata")

	check, err := e.CalculateChecksum(&tfreconcilev1alpha1.Workspace{
		ObjectMeta: v1.ObjectMeta{
			Name:      "workspace1",
			Namespace: "test-ns1",
		},
	})
	assert.NoError(t, err)
	assert.Equal(t, "af553d87c9dd96ff86579fdb87b9711a5fdd31cca2ca1c563d149b4cfc0becac", check)
}
