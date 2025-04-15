package testutils

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	rt "runtime"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
	"sigs.k8s.io/e2e-framework/third_party/helm"
)

func RootFolder() string {
	_, file, _, ok := rt.Caller(0) // Get the caller's file path
	if !ok {
		return ""
	}
	fp, err := filepath.Abs(filepath.Join(file, "..", "..", ".."))
	fmt.Printf("Root folder: %s", fp)
	if err != nil {
		panic(err)
	}
	return fp
}

func CRDFolder() string {
	return filepath.Join(RootFolder(), "crds")
}

func DockerCmd(args ...string) error {
	// Build the operator image
	cmd := exec.Command("docker", args...)
	// output to stdout
	err := cmd.Run()
	out, _ := cmd.CombinedOutput()
	log.Printf("output: %s", out)
	if err != nil {
		return fmt.Errorf("failed to run docker command: %w", err)
	}
	return nil
}

func RunHelmInstall(kubeconfig string, opt ...helm.Option) error {
	h := helm.New(kubeconfig)
	err := h.RunInstall(opt...)
	if err != nil {
		return fmt.Errorf("failed to run helm install: %w", err)
	}
	return nil
}

func RunHelmUninstall(kubeconfig string, name string) error {
	h := helm.New(kubeconfig)
	err := h.RunUninstall(helm.WithName(name))
	if err != nil {
		return fmt.Errorf("failed to run helm uninstall: %w", err)
	}
	return nil
}

func GetGitSHA() string {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	return string(out)
}

// PrintPodLogs prints the logs of the first pod that matches the label selector
func PrintPodLogs(c klient.Client, ctx context.Context, ns, labelSelector string, writer io.Writer) error {
	client := kubernetes.NewForConfigOrDie(c.RESTConfig())
	// Get the operator pod
	podList := &corev1.PodList{}
	err := wait.For(conditions.New(c.Resources(ns)).ResourceListN(podList, 1, resources.WithLabelSelector(labelSelector)),
		wait.WithTimeout(time.Second*30))
	if err != nil {
		return fmt.Errorf("failed to get operator pod: %w", err)
	}

	if len(podList.Items) == 0 {
		return fmt.Errorf("no operator pod found")
	}

	firstPod := podList.Items[0]

	l := client.CoreV1().Pods(ns).GetLogs(firstPod.Name, &corev1.PodLogOptions{
		Container: "",
		Follow:    true,
	})

	logs, err := l.Stream(ctx)
	if err != nil {
		return fmt.Errorf("failed to get logs: %w", err)
	}

	if _, err := bufio.NewReader(logs).WriteTo(writer); err != nil {
		log.Printf("failed to write logs: %v", err)
	}

	return nil
}

func WaitPod(c klient.Client, ctx context.Context, ns, name string) error {
	err := wait.For(conditions.New(c.Resources()).
		PodRunning(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}),
		wait.WithTimeout(time.Second*30), wait.WithImmediate(), wait.WithContext(ctx))

	return err
}

func GetEvents(c klient.Client, name string) (*corev1.EventList, error) {
	el := &corev1.EventList{}
	err := wait.For(conditions.New(c.Resources()).
		ResourceListN(el, 1, resources.WithFieldSelector(fmt.Sprintf("involvedObject.name=%s", name))), wait.WithTimeout(time.Second*10))

	return el, err
}

func GetSecret(c klient.Client, name, namespace string) (*corev1.Secret, error) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	err := wait.For(conditions.New(c.Resources()).
		ResourceMatch(secret, func(object k8s.Object) bool { return true }), wait.WithTimeout(time.Second*10))
	return secret, err
}

func GetFirstFoundEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		logf.Log.Error(err, "Failed to read directory", "path", basePath)
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}

func SetupCRDs(c klient.Client, ctx context.Context, crdPath, pattern string) error {
	r, err := resources.New(c.RESTConfig())
	if err != nil {
		return err
	}
	return decoder.ApplyWithManifestDir(ctx, r, crdPath, pattern, []resources.CreateOption{})
}

func TeardownCRDs(c klient.Client, ctx context.Context, crdPath, pattern string) error {
	r, err := resources.New(c.RESTConfig())
	if err != nil {
		return err
	}
	return decoder.DeleteWithManifestDir(ctx, r, crdPath, pattern, []resources.DeleteOption{})
}

func CreateNamespace(c klient.Client, ctx context.Context, name string, opts ...envfuncs.CreateNamespaceOpts) error {
	namespace := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	for _, opt := range opts {
		opt(c, &namespace)
	}
	if err := c.Resources().Create(ctx, &namespace); err != nil {
		return fmt.Errorf("create namespace func: %w", err)
	}
	return nil
}
