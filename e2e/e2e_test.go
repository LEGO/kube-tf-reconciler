package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LEGO/kube-tf-reconciler/internal/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/support/kind"
	"sigs.k8s.io/e2e-framework/third_party/helm"
)

func TestE2E(t *testing.T) {
	testutils.IntegrationTest(t)
	
	kindClusterName := "krec-cluster" //envconf.RandomName("my-cluster", 16)
	operatorImage := "ghcr.io/lego/kube-tf-reconciler/kube-tf-reconciler:latest"
	operatorName := envconf.RandomName("krec", 16)
	operatorNs := "krec"

	ctx := context.Background()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	sha := testutils.GetGitSHAContext(ctx)

	k := kind.NewCluster(kindClusterName)
	_, err := k.Create(ctx)
	require.NoError(t, err)

	client, err := klient.New(k.KubernetesRestConfig())
	require.NoError(t, err)
	err = k.WaitForControlPlane(ctx, client)
	require.NoError(t, err)
	//defer k.Destroy(ctx)

	err = testutils.DockerCmdContext(ctx, "build", "-t", operatorImage, testutils.RootFolder(), "--target", "krec", "--build-arg", "SHA="+sha, "--build-arg", "DATE="+time.Now().Format(time.RFC3339))
	require.NoError(t, err)
	err = k.LoadImage(ctx, operatorImage)
	assert.NoError(t, err)
	err = testutils.SetupCRDs(client, ctx, testutils.CRDFolder(), "*")
	assert.NoError(t, err)
	err = testutils.CreateNamespace(client, ctx, operatorNs)
	assert.NoError(t, err)

	err = testutils.RunHelmInstall(k.GetKubeconfig(),
		helm.WithChart(filepath.Join(testutils.RootFolder(), "charts", "terraform-reconciler")),
		helm.WithNamespace(operatorNs),
		helm.WithArgs("--set image.tag=latest"),
		helm.WithArgs("--skip-crds"),
		helm.WithName(operatorName),
		helm.WithWait(),
		helm.WithTimeout("10s"))
	assert.NoError(t, err)
	t.Cleanup(func() {
		err = testutils.RunHelmUninstall(k.GetKubeconfig(), helm.WithNamespace(operatorNs), helm.WithName(operatorName))
		t.Log(err)
		err = testutils.TeardownCRDs(client, ctx, testutils.CRDFolder(), "*")
		t.Log(err)
		err = testutils.DeleteNamespace(client, ctx, operatorNs)
		t.Log(err)
	})
	go func() {
		err = testutils.PrintPodLogs(client, ctx, operatorNs, "app.kubernetes.io/name=krec", os.Stdout)
		assert.NoError(t, err)
	}()

	t.Run("test 1", func(t *testing.T) {

	})
}
