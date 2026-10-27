//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rh-ecosystem-edge/k8s-dra-topology-coordinator/internal/controller"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	mockAccelDriver = "mock-accel.example.com"
	cpuDriver       = "dra.cpu"
)

func requireE2EConfig(t *testing.T, profiles ...string) Config {
	t.Helper()
	if os.Getenv("E2E_KUBECONFIG") == "" {
		t.Skip("E2E_KUBECONFIG is not set")
	}
	cfg, err := currentLiveConfig()
	require.NoError(t, err)
	for _, profile := range profiles {
		if cfg.Profile == profile {
			return cfg
		}
	}
	t.Skipf("profile %q is not in %v", cfg.Profile, profiles)
	return Config{}
}

func prepareE2E(t *testing.T, cfg Config) (*harness, context.Context) {
	t.Helper()
	h, err := newHarness(t, cfg)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, h.applyTopologyRules(ctx))
	return h, ctx
}

func setupE2E(t *testing.T, cfg Config, values ...string) (*harness, context.Context) {
	t.Helper()
	h, ctx := prepareE2E(t, cfg)
	require.NoError(t, h.install(ctx, values...))
	registerHelmCleanup(t, h)
	return h, ctx
}

func registerHelmCleanup(t *testing.T, h *harness) {
	t.Helper()
	t.Cleanup(func() {
		if err := h.uninstall(context.Background()); err != nil {
			t.Logf("cleanup: helm uninstall: %v", err)
		}
	})
}

func TestE2EControllerCatalog(t *testing.T) {
	cfg := requireE2EConfig(t, "controller", "cross-driver", "full")
	h, ctx := setupE2E(t, cfg, "partitionMode=auto")

	require.NoError(t, h.waitFor(ctx, "controller deployment readiness", func(ctx context.Context) (bool, error) {
		deployments, err := h.client.AppsV1().Deployments(h.cfg.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/component=controller",
		})
		if err != nil || len(deployments.Items) == 0 {
			return false, err
		}
		deployment := deployments.Items[0]
		replicas := int32(1)
		if deployment.Spec.Replicas != nil {
			replicas = *deployment.Spec.Replicas
		}
		return deployment.Status.ReadyReplicas >= replicas, nil
	}))

	require.NoError(t, h.waitFor(ctx, "mock-device ResourceSlices", func(ctx context.Context) (bool, error) {
		count, err := h.countDriverSlices(ctx, mockAccelDriver)
		return count > 0, err
	}))
	if h.cfg.RequireCPU {
		require.NoError(t, h.waitFor(ctx, "CPU ResourceSlices", func(ctx context.Context) (bool, error) {
			count, err := h.countDriverSlices(ctx, cpuDriver)
			return count > 0, err
		}))
	}

	require.NoError(t, h.waitFor(ctx, "managed DeviceClasses", func(ctx context.Context) (bool, error) {
		classes, err := h.client.ResourceV1().DeviceClasses().List(ctx, metav1.ListOptions{
			LabelSelector: controller.CoordinatorDriverName + "/managed=true",
		})
		if err != nil {
			return false, err
		}
		return len(classes.Items) > 0, h.trackManagedDeviceClasses(ctx)
	}))

	classes, err := h.client.ResourceV1().DeviceClasses().List(ctx, metav1.ListOptions{
		LabelSelector: controller.CoordinatorDriverName + "/managed=true",
	})
	require.NoError(t, err)
	require.NotEmpty(t, classes.Items)
	for _, class := range classes.Items {
		require.NotEmpty(t, class.Spec.Selectors, "DeviceClass %s has no CEL selector", class.Name)
		require.NotEmpty(t, class.Spec.Config, "DeviceClass %s has no opaque coordinator config", class.Name)
	}
	if h.cfg.RequireCPU {
		require.NoError(t, h.assertCrossDriverCatalog(ctx))
	}
}

func TestE2EFixedPartitionMode(t *testing.T) {
	cfg := requireE2EConfig(t, "controller", "cross-driver", "full")
	h, ctx := setupE2E(t, cfg, "partitionMode=partitions")

	require.NoError(t, h.waitFor(ctx, "fixed partition DeviceClasses", func(ctx context.Context) (bool, error) {
		classes, err := h.client.ResourceV1().DeviceClasses().List(ctx, metav1.ListOptions{
			LabelSelector: controller.CoordinatorDriverName + "/managed=true",
		})
		if err != nil {
			return false, err
		}
		if len(classes.Items) == 0 {
			return false, h.trackManagedDeviceClasses(ctx)
		}
		for _, class := range classes.Items {
			if class.Labels[controller.CoordinatorDriverName+"/partitionType"] == "" {
				return false, nil
			}
			if class.Labels[controller.CoordinatorDriverName+"/grouping"] != "" {
				return false, fmt.Errorf("fixed partition mode emitted grouping class %s", class.Name)
			}
		}
		return true, h.trackManagedDeviceClasses(ctx)
	}))
}

func TestE2EResourceSliceAndRuleLifecycle(t *testing.T) {
	cfg := requireE2EConfig(t, "controller", "cross-driver", "full")
	h, ctx := setupE2E(t, cfg, "partitionMode=auto")

	require.NoError(t, h.waitFor(ctx, "mock ResourceSlice", func(ctx context.Context) (bool, error) {
		count, err := h.countDriverSlices(ctx, mockAccelDriver)
		return count > 0, err
	}))
	mockSlices, err := h.client.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	var nodeName string
	for _, slice := range mockSlices.Items {
		if slice.Spec.Driver == mockAccelDriver && slice.Spec.NodeName != nil {
			nodeName = *slice.Spec.NodeName
			break
		}
	}
	require.NotEmpty(t, nodeName, "mock-device must publish a node-scoped ResourceSlice")

	const syntheticDriver = "e2e.synthetic.example.com"
	numaNode := int64(0)
	syntheticName := "synthetic-" + h.cfg.RunID
	_, err = h.client.ResourceV1().ResourceSlices().Create(ctx, &resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: syntheticName, Labels: ownedLabels(h.cfg.RunID)},
		Spec: resourcev1.ResourceSliceSpec{
			Driver:   syntheticDriver,
			Pool:     resourcev1.ResourcePool{Name: "synthetic-pool", Generation: 1, ResourceSliceCount: 1},
			NodeName: &nodeName,
			Devices: []resourcev1.Device{{Name: "synthetic-0", Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				"e2e.synthetic.example.com/numaNode": {IntValue: &numaNode},
			}}},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = h.client.ResourceV1().ResourceSlices().Delete(context.Background(), syntheticName, metav1.DeleteOptions{})
	})

	class := waitForProfileClass(t, h, ctx, syntheticDriver)
	initial := partitionConfigFromDeviceClass(t, class)
	require.NotEmpty(t, initial.SubResources)

	ruleName := h.cfg.RunID + "-synthetic-rule"
	rule := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ruleName,
			Namespace: h.cfg.Namespace,
			Labels: mergeLabels(ownedLabels(h.cfg.RunID), map[string]string{
				controller.TopologyRuleLabel: "true",
			}),
		},
		Data: map[string]string{
			"attribute":    "e2e.synthetic.example.com/numaNode",
			"type":         "int",
			"driver":       syntheticDriver,
			"mapsTo":       "numaNode",
			"partitioning": "info",
		},
	}
	_, err = h.client.CoreV1().ConfigMaps(h.cfg.Namespace).Create(ctx, rule, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, h.waitFor(ctx, "synthetic topology rule", func(ctx context.Context) (bool, error) {
		current, err := h.getProfileClass(ctx, syntheticDriver)
		if err != nil {
			return false, err
		}
		for _, subresource := range partitionConfigFromDeviceClass(h.t, current).SubResources {
			if subresource.DeviceClass == syntheticDriver && len(subresource.Selectors) > 0 {
				return true, nil
			}
		}
		return false, nil
	}))

	rule.Labels = mergeLabels(ownedLabels(h.cfg.RunID), nil)
	_, err = h.client.CoreV1().ConfigMaps(h.cfg.Namespace).Update(ctx, rule, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, h.waitFor(ctx, "topology rule label removal", func(ctx context.Context) (bool, error) {
		current, err := h.getProfileClass(ctx, syntheticDriver)
		if err != nil {
			return false, err
		}
		for _, subresource := range partitionConfigFromDeviceClass(h.t, current).SubResources {
			if subresource.DeviceClass == syntheticDriver {
				for _, selector := range subresource.Selectors {
					if strings.Contains(selector, syntheticDriver+"/numaNode") {
						return false, nil
					}
				}
				return true, nil
			}
		}
		return false, nil
	}))

	require.NoError(t, h.client.ResourceV1().ResourceSlices().Delete(ctx, syntheticName, metav1.DeleteOptions{}))
	require.NoError(t, h.waitFor(ctx, "synthetic DeviceClass cleanup", func(ctx context.Context) (bool, error) {
		_, err := h.getProfileClass(ctx, syntheticDriver)
		return apierrors.IsNotFound(err), nil
	}))
}

func TestE2EAdmissionExpandsManagedResourceClaim(t *testing.T) {
	cfg := requireE2EConfig(t, "admission", "full")
	h, ctx := setupE2E(t, cfg, "partitionMode=auto", "controller.networkPolicy.enabled=true")

	class := waitForPartitionClass(t, h, ctx)
	require.Equal(t, cfg.DriverName, class.Spec.Config[0].Opaque.Driver)
	require.NoError(t, h.waitFor(ctx, "webhook service and CA wiring", h.webhookReady))
	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim-" + h.cfg.RunID, Namespace: h.cfg.Namespace, Labels: ownedLabels(h.cfg.RunID)},
		Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
			Name:    "partition",
			Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: class.Name, Count: 1},
		}}}},
	}
	created, err := h.client.ResourceV1().ResourceClaims(h.cfg.Namespace).Create(ctx, claim, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, created.Annotations[controller.CoordinatorDriverName+"/expanded-requests"])
	require.NotEmpty(t, created.Spec.Devices.Requests)
	require.NotEqual(t, "partition", created.Spec.Devices.Requests[0].Name)
	assert.Empty(t, created.Spec.Devices.Config, "container offerings must not inject VFIO configuration")

	var mapping map[string][]string
	require.NoError(t, json.Unmarshal([]byte(created.Annotations[controller.CoordinatorDriverName+"/expanded-requests"]), &mapping))
	require.NotEmpty(t, mapping["partition"])
	for _, request := range created.Spec.Devices.Requests {
		require.NotNil(t, request.Exactly)
		require.NotEmpty(t, request.Exactly.DeviceClassName)
	}

	updated := created.DeepCopy()
	updated.Labels["e2e.nodepartition.dra.k8s.io/updated"] = "true"
	updated, err = h.client.ResourceV1().ResourceClaims(h.cfg.Namespace).Update(ctx, updated, metav1.UpdateOptions{})
	require.NoError(t, err)
	assert.Equal(t, created.Spec.Devices.Requests, updated.Spec.Devices.Requests)

	nativeClassName := "native-" + h.cfg.RunID
	_, err = h.client.ResourceV1().DeviceClasses().Create(ctx, &resourcev1.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{Name: nativeClassName, Labels: ownedLabels(h.cfg.RunID)},
		Spec: resourcev1.DeviceClassSpec{Config: []resourcev1.DeviceClassConfiguration{{DeviceConfiguration: resourcev1.DeviceConfiguration{
			Opaque: &resourcev1.OpaqueDeviceConfiguration{Driver: "native.example.com", Parameters: runtime.RawExtension{Raw: []byte(`{}`)}},
		}}}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = h.client.ResourceV1().DeviceClasses().Delete(context.Background(), nativeClassName, metav1.DeleteOptions{})
	})
	nativeClaim, err := h.client.ResourceV1().ResourceClaims(h.cfg.Namespace).Create(ctx, &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "native-claim-" + h.cfg.RunID, Namespace: h.cfg.Namespace, Labels: ownedLabels(h.cfg.RunID)},
		Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
			Name: "native", Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: nativeClassName, Count: 1},
		}}}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	assert.Equal(t, "native", nativeClaim.Spec.Devices.Requests[0].Name)
	assert.Empty(t, nativeClaim.Annotations[controller.CoordinatorDriverName+"/expanded-requests"])

	invalidClassName := "invalid-" + h.cfg.RunID
	_, err = h.client.ResourceV1().DeviceClasses().Create(ctx, &resourcev1.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{Name: invalidClassName, Labels: map[string]string{controller.CoordinatorDriverName + "/managed": "true"}},
		Spec: resourcev1.DeviceClassSpec{Config: []resourcev1.DeviceClassConfiguration{{DeviceConfiguration: resourcev1.DeviceConfiguration{
			// Keep the payload valid JSON: Kubernetes 1.37 panics while recording
			// managed fields for malformed opaque JSON instead of returning an API error.
			Opaque: &resourcev1.OpaqueDeviceConfiguration{Driver: cfg.DriverName, Parameters: runtime.RawExtension{Raw: []byte(`{"kind":"SomethingElse","foo":"bar"}`)}},
		}}}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = h.client.ResourceV1().DeviceClasses().Delete(context.Background(), invalidClassName, metav1.DeleteOptions{})
	})
	_, err = h.client.ResourceV1().ResourceClaims(h.cfg.Namespace).Create(ctx, &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-claim-" + h.cfg.RunID, Namespace: h.cfg.Namespace, Labels: ownedLabels(h.cfg.RunID)},
		Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
			Name: "invalid", Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: invalidClassName, Count: 1},
		}}}},
	}, metav1.CreateOptions{})
	require.Error(t, err, "malformed managed catalog entries must fail closed")

	directPod, err := h.client.CoreV1().Pods(h.cfg.Namespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "direct-pod-" + h.cfg.RunID, Namespace: h.cfg.Namespace, Labels: ownedLabels(h.cfg.RunID)},
		Spec: corev1.PodSpec{
			ResourceClaims: []corev1.PodResourceClaim{{Name: "partition", ResourceClaimName: &created.Name}},
			Containers:     []corev1.Container{{Name: "consumer", Image: "registry.k8s.io/pause:3.10", Resources: corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "partition", Request: "partition"}}}}},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, directPod.Spec.Containers[0].Resources.Claims)
	assert.NotEqual(t, "partition", directPod.Spec.Containers[0].Resources.Claims[0].Request)
}

func TestE2EAdmissionRewritesPodTemplateClaim(t *testing.T) {
	cfg := requireE2EConfig(t, "admission", "full")
	h, ctx := setupE2E(t, cfg, "partitionMode=auto")

	class := waitForPartitionClass(t, h, ctx)
	templateName := "template-" + h.cfg.RunID
	_, err := h.client.ResourceV1().ResourceClaimTemplates(h.cfg.Namespace).Create(ctx, &resourcev1.ResourceClaimTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: templateName, Namespace: h.cfg.Namespace, Labels: ownedLabels(h.cfg.RunID)},
		Spec: resourcev1.ResourceClaimTemplateSpec{Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
				Name:    "partition",
				Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: class.Name, Count: 1},
			}}},
		}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	claimRef := "partition"
	pod, err := h.client.CoreV1().Pods(h.cfg.Namespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-" + h.cfg.RunID, Namespace: h.cfg.Namespace, Labels: ownedLabels(h.cfg.RunID)},
		Spec: corev1.PodSpec{
			ResourceClaims: []corev1.PodResourceClaim{{Name: claimRef, ResourceClaimTemplateName: &templateName}},
			Containers: []corev1.Container{{
				Name:  "consumer",
				Image: "registry.k8s.io/pause:3.10",
				Resources: corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{
					Name:    claimRef,
					Request: "partition",
				}}},
			}},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, pod.Spec.Containers[0].Resources.Claims)
	require.NotEqual(t, "partition", pod.Spec.Containers[0].Resources.Claims[0].Request)
}

func TestE2EGroupingAndIntentLifecycle(t *testing.T) {
	cfg := requireE2EConfig(t, "controller", "full")
	h, ctx := prepareE2E(t, cfg)
	groupingName := h.cfg.RunID + "-group"
	grouping := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      h.cfg.RunID + "-grouping",
			Namespace: h.cfg.Namespace,
			Labels: mergeLabels(ownedLabels(h.cfg.RunID), map[string]string{
				controller.DeviceGroupingLabel: "true",
			}),
		},
		Data: map[string]string{
			"name":          groupingName,
			"alignment":     "numaNode",
			"mode":          "container",
			"displayName":   "E2E grouping",
			"description":   "E2E grouping description",
			"intentLevel":   "workload",
			"categories":    "ai,inference",
			"workloadTypes": "serving",
			"visibility":    "public",
			"devices":       "- class: mock-accel.example.com\n  count: 1\n",
		},
	}
	_, err := h.client.CoreV1().ConfigMaps(h.cfg.Namespace).Create(ctx, grouping, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, h.install(ctx, "partitionMode=groupings"))
	registerHelmCleanup(t, h)

	class := waitForGroupingClass(t, h, ctx, groupingName)
	assertIntentMetadata(t, class)

	updated := grouping.DeepCopy()
	updated.Data["intentLevel"] = "invalid"
	updated.Data["displayName"] = "invalid-update-marker"
	_, err = h.client.CoreV1().ConfigMaps(h.cfg.Namespace).Update(ctx, updated, metav1.UpdateOptions{})
	require.NoError(t, err)
	logMarker := fmt.Sprintf("Failed to update device grouping from %s/%s", h.cfg.Namespace, grouping.Name)
	require.NoError(t, h.waitFor(ctx, "invalid grouping update processing", func(ctx context.Context) (bool, error) {
		return h.controllerLogsContain(ctx, logMarker)
	}))
	require.NoError(t, h.waitFor(ctx, "last valid grouping metadata", func(ctx context.Context) (bool, error) {
		current, err := h.client.ResourceV1().DeviceClasses().Get(ctx, groupingName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return current.Labels[controller.CoordinatorDriverName+"/intent-level"] == "workload", nil
	}))

	require.NoError(t, h.client.CoreV1().ConfigMaps(h.cfg.Namespace).Delete(ctx, grouping.Name, metav1.DeleteOptions{}))
	require.NoError(t, h.waitFor(ctx, "grouping DeviceClass cleanup", func(ctx context.Context) (bool, error) {
		_, err := h.client.ResourceV1().DeviceClasses().Get(ctx, groupingName, metav1.GetOptions{})
		return apierrors.IsNotFound(err), nil
	}))
}

func TestE2ELeaderFailover(t *testing.T) {
	cfg := requireE2EConfig(t, "ha", "full")
	h, ctx := setupE2E(t, cfg, "partitionMode=auto", "controller.replicaCount=2")
	t.Cleanup(func() {
		if err := h.client.CoordinationV1().Leases("kube-system").Delete(context.Background(), h.leaderElectionID(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("cleanup: delete leader lease: %v", err)
		}
	})

	require.NoError(t, h.waitFor(ctx, "two ready controller replicas", func(ctx context.Context) (bool, error) {
		deployments, err := h.client.AppsV1().Deployments(h.cfg.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/component=controller",
		})
		if err != nil || len(deployments.Items) == 0 {
			return false, err
		}
		return deployments.Items[0].Status.ReadyReplicas >= 2, nil
	}))
	class := waitForPartitionClass(t, h, ctx)
	baselineClaim := createAdmissionProbeClaim(t, h, ctx, class, "ha-baseline-"+h.cfg.RunID)
	for i := 0; i < 3; i++ {
		probe := createAdmissionProbeClaim(t, h, ctx, class, fmt.Sprintf("ha-probe-%d-%s", i, h.cfg.RunID))
		assert.Equal(t, baselineClaim.Spec.Devices.Requests, probe.Spec.Devices.Requests,
			"all replicas must produce deterministic expansion")
	}

	var oldLeader string
	require.NoError(t, h.waitFor(ctx, "leader lease", func(ctx context.Context) (bool, error) {
		lease, err := h.client.CoordinationV1().Leases("kube-system").Get(ctx, h.leaderElectionID(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
			return false, nil
		}
		oldLeader = *lease.Spec.HolderIdentity
		return true, nil
	}))

	leaderDeleted := false
	pods, err := h.client.CoreV1().Pods(h.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/component=controller",
	})
	require.NoError(t, err)
	for _, pod := range pods.Items {
		if pod.Name == oldLeader {
			require.NoError(t, h.client.CoreV1().Pods(h.cfg.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}))
			leaderDeleted = true
			break
		}
	}
	require.True(t, leaderDeleted, "leader lease holder %q must identify a controller pod", oldLeader)

	require.NoError(t, h.waitFor(ctx, "leader lease handoff", func(ctx context.Context) (bool, error) {
		lease, err := h.client.CoordinationV1().Leases("kube-system").Get(ctx, h.leaderElectionID(), metav1.GetOptions{})
		if err != nil || lease.Spec.HolderIdentity == nil {
			return false, err
		}
		return *lease.Spec.HolderIdentity != oldLeader, nil
	}))
	require.NoError(t, h.waitFor(ctx, "managed output after failover", func(ctx context.Context) (bool, error) {
		classes, err := h.client.ResourceV1().DeviceClasses().List(ctx, metav1.ListOptions{
			LabelSelector: controller.CoordinatorDriverName + "/managed=true",
		})
		if err != nil {
			return false, err
		}
		return len(classes.Items) > 0, h.trackManagedDeviceClasses(ctx)
	}))
	afterFailover := createAdmissionProbeClaim(t, h, ctx, class, "ha-after-"+h.cfg.RunID)
	assert.Equal(t, baselineClaim.Spec.Devices.Requests, afterFailover.Spec.Devices.Requests,
		"new leader must preserve admission expansion after handoff")
}

func TestE2EKubeVirtAdmission(t *testing.T) {
	cfg := requireE2EConfig(t, "kubevirt", "full")
	h, ctx := prepareE2E(t, cfg)
	var err error
	resources, err := h.client.Discovery().ServerResourcesForGroupVersion("kubevirt.io/v1")
	if err != nil {
		if cfg.RequireKubeVirt {
			require.NoError(t, err, "KubeVirt API must be installed for this profile")
		}
		t.Skipf("KubeVirt API is unavailable: %v", err)
	}
	hasVMI := false
	for _, resource := range resources.APIResources {
		if resource.Name == "virtualmachineinstances" {
			hasVMI = true
			break
		}
	}
	if !hasVMI {
		if cfg.RequireKubeVirt {
			t.Fatal("KubeVirt API does not expose virtualmachineinstances")
		}
		t.Skip("KubeVirt API does not expose virtualmachineinstances")
	}

	groupingName := h.cfg.RunID + "-vm"
	_, err = h.client.CoreV1().ConfigMaps(h.cfg.Namespace).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      h.cfg.RunID + "-vm-grouping",
			Namespace: h.cfg.Namespace,
			Labels: mergeLabels(ownedLabels(h.cfg.RunID), map[string]string{
				controller.DeviceGroupingLabel: "true",
			}),
		},
		Data: map[string]string{
			"name":    groupingName,
			"mode":    "passthrough",
			"devices": "- class: mock-accel.example.com\n  count: 1\n  passthrough: true\n",
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, h.install(ctx, "partitionMode=groupings"))
	registerHelmCleanup(t, h)

	class := waitForGroupingClass(t, h, ctx, groupingName)
	config := partitionConfigFromDeviceClass(t, class)
	require.Equal(t, controller.OfferingModePassthrough, config.Mode)
	templateName := "vm-template-" + h.cfg.RunID
	_, err = h.client.ResourceV1().ResourceClaimTemplates(h.cfg.Namespace).Create(ctx, &resourcev1.ResourceClaimTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: templateName, Namespace: h.cfg.Namespace, Labels: ownedLabels(h.cfg.RunID)},
		Spec: resourcev1.ResourceClaimTemplateSpec{Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
				Name:    "partition",
				Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: class.Name, Count: 1},
			}}},
		}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	vmi := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachineInstance",
		"metadata": map[string]interface{}{
			"name":      "vmi-" + h.cfg.RunID,
			"namespace": h.cfg.Namespace,
			"labels":    map[string]interface{}{runLabelKey: h.cfg.RunID, ownerLabelKey: "true"},
		},
		"spec": map[string]interface{}{
			"resourceClaims": []interface{}{map[string]interface{}{
				"name":                      "partition",
				"resourceClaimTemplateName": templateName,
			}},
			"domain": map[string]interface{}{
				"resources": map[string]interface{}{"requests": map[string]interface{}{"memory": "1Gi"}},
				"devices":   map[string]interface{}{},
			},
		},
	}}
	vmiGVR := schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances"}
	created, err := h.dynamic.Resource(vmiGVR).Namespace(h.cfg.Namespace).Create(ctx, vmi, metav1.CreateOptions{})
	require.NoError(t, err)
	hostDevices, found, err := unstructured.NestedSlice(created.Object, "spec", "domain", "devices", "hostDevices")
	require.NoError(t, err)
	require.True(t, found, "webhook should add hostDevices to the VMI")
	require.Len(t, hostDevices, 1)
	hostDevice, ok := hostDevices[0].(map[string]interface{})
	require.True(t, ok)
	require.NotEmpty(t, hostDevice["requestName"])
}

func (h *harness) assertCrossDriverCatalog(ctx context.Context) error {
	mockNodes, err := h.driverNodes(ctx, mockAccelDriver)
	if err != nil {
		return err
	}
	cpuNodes, err := h.driverNodes(ctx, cpuDriver)
	if err != nil {
		return err
	}
	shared := false
	for node := range mockNodes {
		if _, ok := cpuNodes[node]; ok {
			shared = true
			break
		}
	}
	if !shared {
		return fmt.Errorf("mock and CPU drivers have no shared node")
	}
	classes, err := h.client.ResourceV1().DeviceClasses().List(ctx, metav1.ListOptions{
		LabelSelector: controller.CoordinatorDriverName + "/managed=true",
	})
	if err != nil {
		return err
	}
	for _, class := range classes.Items {
		profile := class.Labels[controller.CoordinatorDriverName+"/profile"]
		if !strings.Contains(profile, mockAccelDriver) || !strings.Contains(profile, cpuDriver) {
			continue
		}
		config := partitionConfigFromDeviceClass(h.t, &class)
		seen := map[string]bool{}
		for _, subresource := range config.SubResources {
			seen[subresource.DeviceClass] = len(subresource.Selectors) > 0
		}
		if seen[mockAccelDriver] && seen[cpuDriver] {
			return nil
		}
	}
	return fmt.Errorf("no combined DeviceClass with per-driver selectors for %s and %s", mockAccelDriver, cpuDriver)
}

func (h *harness) driverNodes(ctx context.Context, driver string) (map[string]struct{}, error) {
	slices, err := h.client.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	nodes := make(map[string]struct{})
	for _, slice := range slices.Items {
		if slice.Spec.Driver == driver && slice.Spec.NodeName != nil {
			nodes[*slice.Spec.NodeName] = struct{}{}
		}
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("driver %s has no node-scoped ResourceSlices", driver)
	}
	return nodes, nil
}

func (h *harness) webhookReady(ctx context.Context) (bool, error) {
	services, err := h.client.CoreV1().Services(h.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/instance=" + h.cfg.Release,
	})
	if err != nil {
		return false, err
	}
	var webhookService string
	for _, service := range services.Items {
		if strings.HasSuffix(service.Name, "-webhook") {
			webhookService = service.Name
			if service.Spec.ClusterIP == "" {
				return false, nil
			}
			break
		}
	}
	if webhookService == "" {
		return false, nil
	}
	endpoints, err := h.client.CoreV1().Endpoints(h.cfg.Namespace).Get(ctx, webhookService, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	readyAddresses := 0
	for _, subset := range endpoints.Subsets {
		readyAddresses += len(subset.Addresses)
	}
	if readyAddresses == 0 {
		return false, nil
	}
	configs, err := h.client.AdmissionregistrationV1().MutatingWebhookConfigurations().List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/instance=" + h.cfg.Release,
	})
	if err != nil {
		return false, err
	}
	if len(configs.Items) == 0 {
		return false, nil
	}
	for _, config := range configs.Items {
		for _, webhook := range config.Webhooks {
			if webhook.ClientConfig.Service == nil || webhook.ClientConfig.Service.Name != webhookService || len(webhook.ClientConfig.CABundle) == 0 {
				return false, nil
			}
		}
	}
	return true, nil
}

func (h *harness) controllerLogsContain(ctx context.Context, marker string) (bool, error) {
	pods, err := h.client.CoreV1().Pods(h.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/component=controller",
	})
	if err != nil {
		return false, err
	}
	for _, pod := range pods.Items {
		logs, err := h.client.CoreV1().Pods(h.cfg.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: "controller"}).Do(ctx).Raw()
		if err != nil {
			continue
		}
		if strings.Contains(string(logs), marker) {
			return true, nil
		}
	}
	return false, nil
}

func waitForGroupingClass(t *testing.T, h *harness, ctx context.Context, name string) *resourcev1.DeviceClass {
	t.Helper()
	var class *resourcev1.DeviceClass
	require.NoError(t, h.waitFor(ctx, "grouping DeviceClass "+name, func(ctx context.Context) (bool, error) {
		current, err := h.client.ResourceV1().DeviceClasses().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		class = current
		return true, h.trackManagedDeviceClasses(ctx)
	}))
	return class
}

func createAdmissionProbeClaim(t *testing.T, h *harness, ctx context.Context, class *resourcev1.DeviceClass, name string) *resourcev1.ResourceClaim {
	t.Helper()
	claim, err := h.client.ResourceV1().ResourceClaims(h.cfg.Namespace).Create(ctx, &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.cfg.Namespace, Labels: ownedLabels(h.cfg.RunID)},
		Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
			Name: "partition", Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: class.Name, Count: 1},
		}}}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	return claim
}

func waitForProfileClass(t *testing.T, h *harness, ctx context.Context, driver string) *resourcev1.DeviceClass {
	t.Helper()
	var class *resourcev1.DeviceClass
	require.NoError(t, h.waitFor(ctx, "DeviceClass profile "+driver, func(ctx context.Context) (bool, error) {
		current, err := h.getProfileClass(ctx, driver)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		class = current
		return true, h.trackManagedDeviceClasses(ctx)
	}))
	return class
}

func (h *harness) getProfileClass(ctx context.Context, driver string) (*resourcev1.DeviceClass, error) {
	classes, err := h.client.ResourceV1().DeviceClasses().List(ctx, metav1.ListOptions{
		LabelSelector: controller.CoordinatorDriverName + "/managed=true",
	})
	if err != nil {
		return nil, err
	}
	for i := range classes.Items {
		class := &classes.Items[i]
		if !strings.Contains(class.Labels[controller.CoordinatorDriverName+"/profile"], driver) {
			continue
		}
		for _, config := range class.Spec.Config {
			if config.Opaque == nil {
				continue
			}
			var partitionConfig controller.PartitionConfig
			if err := json.Unmarshal(config.Opaque.Parameters.Raw, &partitionConfig); err != nil {
				continue
			}
			for _, subresource := range partitionConfig.SubResources {
				if subresource.DeviceClass == driver {
					return class.DeepCopy(), nil
				}
			}
		}
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "deviceclasses"}, driver)
}

func assertIntentMetadata(t *testing.T, class *resourcev1.DeviceClass) {
	t.Helper()
	assert.Equal(t, "workload", class.Labels[controller.CoordinatorDriverName+"/intent-level"])
	assert.Equal(t, "public", class.Labels[controller.CoordinatorDriverName+"/visibility"])
	assert.Equal(t, "E2E grouping", class.Annotations[controller.CoordinatorDriverName+"/display-name"])
	assert.Equal(t, "ai,inference", class.Annotations[controller.CoordinatorDriverName+"/categories"])
	assert.Equal(t, "serving", class.Annotations[controller.CoordinatorDriverName+"/workload-types"])
	config := partitionConfigFromDeviceClass(t, class)
	assert.Equal(t, controller.OfferingModeContainer, config.Mode)
}

func partitionConfigFromDeviceClass(t *testing.T, class *resourcev1.DeviceClass) controller.PartitionConfig {
	t.Helper()
	for _, config := range class.Spec.Config {
		if config.Opaque == nil {
			continue
		}
		var partitionConfig controller.PartitionConfig
		if err := json.Unmarshal(config.Opaque.Parameters.Raw, &partitionConfig); err == nil && partitionConfig.Kind == "PartitionConfig" {
			return partitionConfig
		}
	}
	t.Fatalf("DeviceClass %s has no PartitionConfig", class.Name)
	return controller.PartitionConfig{}
}

func waitForPartitionClass(t *testing.T, h *harness, ctx context.Context) *resourcev1.DeviceClass {
	t.Helper()
	var class *resourcev1.DeviceClass
	require.NoError(t, h.waitFor(ctx, "managed partition DeviceClass", func(ctx context.Context) (bool, error) {
		classes, err := h.client.ResourceV1().DeviceClasses().List(ctx, metav1.ListOptions{
			LabelSelector: controller.CoordinatorDriverName + "/managed=true",
		})
		if err != nil {
			return false, err
		}
		for i := range classes.Items {
			candidate := &classes.Items[i]
			for _, config := range candidate.Spec.Config {
				if config.Opaque != nil && config.Opaque.Driver == h.cfg.DriverName {
					class = candidate.DeepCopy()
					_ = h.trackManagedDeviceClasses(ctx)
					return true, nil
				}
			}
		}
		return false, nil
	}))
	return class
}

func (h *harness) countDriverSlices(ctx context.Context, driver string) (int, error) {
	slices, err := h.client.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, slice := range slices.Items {
		if slice.Spec.Driver == driver {
			count++
		}
	}
	return count, nil
}
