//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/rh-ecosystem-edge/k8s-dra-topology-coordinator/internal/controller"
)

const (
	runLabelKey   = "e2e.nodepartition.dra.k8s.io/run"
	ownerLabelKey = "e2e.nodepartition.dra.k8s.io/owned"
)

type harness struct {
	t                     *testing.T
	cfg                   Config
	client                kubernetes.Interface
	dynamic               dynamic.Interface
	baselineDeviceClasses map[string]struct{}
	createdDeviceClasses  map[string]struct{}
	namespaceCreated      bool
}

func ownedLabels(runID string) map[string]string {
	return map[string]string{
		runLabelKey:   runID,
		ownerLabelKey: "true",
	}
}

func newHarness(t *testing.T, cfg Config) (*harness, error) {
	t.Helper()
	clientConfig, err := clientcmd.BuildConfigFromFlags("", cfg.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("build Kubernetes config: %w", err)
	}
	client, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client: %w", err)
	}
	dynamicClient, err := dynamic.NewForConfig(clientConfig)
	if err != nil {
		return nil, fmt.Errorf("create dynamic Kubernetes client: %w", err)
	}
	h := &harness{
		t:                     t,
		cfg:                   cfg,
		client:                client,
		dynamic:               dynamicClient,
		baselineDeviceClasses: make(map[string]struct{}),
		createdDeviceClasses:  make(map[string]struct{}),
	}
	if err := h.prepare(context.Background()); err != nil {
		return nil, err
	}
	t.Cleanup(func() { h.cleanup(context.Background()) })
	return h, nil
}

func (h *harness) prepare(ctx context.Context) error {
	if err := h.removeStaleWebhookConfigurations(ctx); err != nil {
		return err
	}
	classes, err := h.client.ResourceV1().DeviceClasses().List(ctx, metav1.ListOptions{
		LabelSelector: controller.CoordinatorDriverName + "/managed=true",
	})
	if err != nil {
		return fmt.Errorf("list existing managed DeviceClasses: %w", err)
	}
	for _, name := range h.newManagedDeviceClassNames(classes) {
		h.baselineDeviceClasses[name] = struct{}{}
	}
	if len(h.baselineDeviceClasses) > 0 {
		return fmt.Errorf("disposable-cluster precondition failed: managed DeviceClasses already exist: %v", sortedKeys(h.baselineDeviceClasses))
	}

	ns, err := h.client.CoreV1().Namespaces().Get(ctx, h.cfg.Namespace, metav1.GetOptions{})
	if err == nil {
		return fmt.Errorf("disposable-cluster precondition failed: namespace %q already exists with labels %v", ns.Name, ns.Labels)
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("check E2E namespace %q: %w", h.cfg.Namespace, err)
	}
	if _, err := h.client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: h.cfg.Namespace, Labels: ownedLabels(h.cfg.RunID)},
	}, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create E2E namespace %q: %w", h.cfg.Namespace, err)
	}
	h.namespaceCreated = true
	return nil
}

func (h *harness) removeStaleWebhookConfigurations(ctx context.Context) error {
	configs, err := h.client.AdmissionregistrationV1().MutatingWebhookConfigurations().List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=nodepartition,app.kubernetes.io/component=controller",
	})
	if err != nil {
		return fmt.Errorf("list stale webhook configurations: %w", err)
	}
	for _, config := range configs.Items {
		if config.Name == h.cfg.Release {
			continue
		}
		if err := h.client.AdmissionregistrationV1().MutatingWebhookConfigurations().Delete(ctx, config.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale webhook configuration %q: %w", config.Name, err)
		}
	}
	return nil
}

func (h *harness) newManagedDeviceClassNames(classes *resourcev1.DeviceClassList) []string {
	if classes == nil {
		return nil
	}
	names := make([]string, 0, len(classes.Items))
	for _, class := range classes.Items {
		if _, exists := h.baselineDeviceClasses[class.Name]; exists {
			continue
		}
		names = append(names, class.Name)
	}
	sort.Strings(names)
	return names
}

func (h *harness) trackManagedDeviceClasses(ctx context.Context) error {
	classes, err := h.client.ResourceV1().DeviceClasses().List(ctx, metav1.ListOptions{
		LabelSelector: controller.CoordinatorDriverName + "/managed=true",
	})
	if err != nil {
		return err
	}
	for _, name := range h.newManagedDeviceClassNames(classes) {
		h.createdDeviceClasses[name] = struct{}{}
	}
	return nil
}

func (h *harness) cleanup(ctx context.Context) {
	if err := h.trackManagedDeviceClasses(ctx); err != nil {
		h.t.Logf("cleanup: list managed DeviceClasses: %v", err)
	}
	for _, name := range sortedKeys(h.createdDeviceClasses) {
		if err := h.client.ResourceV1().DeviceClasses().Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			h.t.Logf("cleanup: delete DeviceClass %s: %v", name, err)
		}
	}
	if h.namespaceCreated {
		if err := h.client.CoreV1().Namespaces().Delete(ctx, h.cfg.Namespace, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			h.t.Logf("cleanup: delete namespace %s: %v", h.cfg.Namespace, err)
			return
		}
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			_, err := h.client.CoreV1().Namespaces().Get(ctx, h.cfg.Namespace, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return
			}
			if err != nil {
				h.t.Logf("cleanup: check namespace %s deletion: %v", h.cfg.Namespace, err)
			}
			time.Sleep(500 * time.Millisecond)
		}
		h.t.Logf("cleanup: namespace %s still exists after deletion timeout", h.cfg.Namespace)
	}
}

func (h *harness) helm(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "helm", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("helm %s: %w\n%s", strings.Join(args, " "), err, output)
	}
	return nil
}

func (h *harness) chartPath() (string, error) {
	if path := os.Getenv("E2E_CHART_PATH"); path != "" {
		return path, nil
	}
	path, err := filepath.Abs(filepath.Join("..", "..", "deploy", "helm", "nodepartition"))
	if err != nil {
		return "", err
	}
	return path, nil
}

func (h *harness) install(ctx context.Context, values ...string) error {
	chart, err := h.chartPath()
	if err != nil {
		return err
	}
	args := []string{
		"install", h.cfg.Release, chart,
		"--namespace", h.cfg.Namespace,
		"--create-namespace",
		"--set", "controller.image.tag=dev",
		"--set", "controller.image.pullPolicy=IfNotPresent",
		"--set", "driverName=" + h.cfg.DriverName,
		"--set", "controller.leaderElection.id=" + h.leaderElectionID(),
		"--set", "controller.webhook.tls.mode=" + h.cfg.TLSMode,
	}
	if h.cfg.TLSMode == "cert-manager" {
		args = append(args,
			"--set", "controller.webhook.tls.certManager.issuerName="+h.cfg.TLSIssuerName,
			"--set", "controller.webhook.tls.certManager.issuerKind="+h.cfg.TLSIssuerKind,
		)
	}
	if h.cfg.TLSMode == "manual" {
		args = append(args, "--set", "controller.webhook.tls.caBundle="+h.cfg.TLSCABundle)
	}
	for _, value := range values {
		args = append(args, "--set", value)
	}
	return h.helm(ctx, args...)
}

func (h *harness) leaderElectionID() string {
	id := h.cfg.RunID + "-leader"
	if len(id) > 63 {
		id = id[:63]
	}
	return strings.Trim(id, "-")
}

func (h *harness) applyTopologyRules(ctx context.Context) error {
	rules := []struct {
		name string
		data map[string]string
	}{
		{
			name: h.cfg.RunID + "-mock-numa-rule",
			data: map[string]string{
				"attribute":    "mock-accel.example.com/numaNode",
				"type":         "int",
				"driver":       "mock-accel.example.com",
				"mapsTo":       "numaNode",
				"partitioning": "group",
			},
		},
		{
			name: h.cfg.RunID + "-mock-pci-rule",
			data: map[string]string{
				"attribute":    "mock-accel.example.com/pciAddress",
				"type":         "string",
				"driver":       "mock-accel.example.com",
				"mapsTo":       "pcieRoot",
				"partitioning": "group",
			},
		},
	}
	if h.cfg.RequireCPU {
		rules = append(rules,
			struct {
				name string
				data map[string]string
			}{
				name: h.cfg.RunID + "-cpu-numa-rule",
				data: map[string]string{
					"attribute":    "dra.cpu/numaNodeID",
					"type":         "int",
					"driver":       "dra.cpu",
					"mapsTo":       "numaNode",
					"partitioning": "group",
				},
			},
			struct {
				name string
				data map[string]string
			}{
				name: h.cfg.RunID + "-cpu-socket-rule",
				data: map[string]string{
					"attribute":    "dra.cpu/socketID",
					"type":         "int",
					"driver":       "dra.cpu",
					"mapsTo":       "socket",
					"partitioning": "group",
				},
			},
		)
	}
	for _, rule := range rules {
		if _, err := h.client.CoreV1().ConfigMaps(h.cfg.Namespace).Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      rule.name,
				Namespace: h.cfg.Namespace,
				Labels:    mergeLabels(ownedLabels(h.cfg.RunID), map[string]string{controller.TopologyRuleLabel: "true"}),
			},
			Data: rule.data,
		}, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create topology rule %s: %w", rule.name, err)
		}
	}
	return nil
}

func (h *harness) uninstall(ctx context.Context) error {
	return h.helm(ctx, "uninstall", h.cfg.Release, "--namespace", h.cfg.Namespace)
}

func (h *harness) waitFor(ctx context.Context, description string, condition func(context.Context) (bool, error)) error {
	deadline := time.Now().Add(h.cfg.Timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		ready, err := condition(ctx)
		if err != nil {
			lastErr = err
		} else if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if lastErr != nil {
		return fmt.Errorf("timed out waiting for %s: %w", description, lastErr)
	}
	return fmt.Errorf("timed out waiting for %s", description)
}

func sortedKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func mergeLabels(first, second map[string]string) map[string]string {
	merged := make(map[string]string, len(first)+len(second))
	for key, value := range first {
		merged[key] = value
	}
	for key, value := range second {
		merged[key] = value
	}
	return merged
}
