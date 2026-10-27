package controller_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/rh-ecosystem-edge/k8s-dra-topology-coordinator/internal/controller"
)

func TestResolveEnvtestAssets_UsesVersionedDirectory(t *testing.T) {
	base := t.TempDir()
	versioned := filepath.Join(base, "1.34.1-linux-amd64")
	require.NoError(t, os.MkdirAll(versioned, 0750))
	for _, name := range []string{"etcd", "kube-apiserver", "kubectl"} {
		require.NoError(t, os.WriteFile(filepath.Join(versioned, name), []byte("test"), 0600))
	}

	got, err := resolveEnvtestAssets(base)
	require.NoError(t, err)
	assert.Equal(t, versioned, got)
}

func TestResolveEnvtestAssets_UsesExactDirectory(t *testing.T) {
	base := t.TempDir()
	for _, name := range []string{"etcd", "kube-apiserver", "kubectl"} {
		require.NoError(t, os.WriteFile(filepath.Join(base, name), []byte("test"), 0600))
	}

	got, err := resolveEnvtestAssets(base)
	require.NoError(t, err)
	assert.Equal(t, base, got)
}

func intPtr(v int64) *int64   { return &v }
func strPtr(v string) *string { return &v }

var envtestBinaries = []string{"etcd", "kube-apiserver", "kubectl"}

func resolveEnvtestAssets(base string) (string, error) {
	if hasEnvtestBinaries(base) {
		return base, nil
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return "", fmt.Errorf("reading envtest assets directory %q: %w", base, err)
	}
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(base, entry.Name())
		if hasEnvtestBinaries(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("envtest binaries not found in %q; run make setup-envtest", base)
}

func hasEnvtestBinaries(dir string) bool {
	for _, name := range envtestBinaries {
		if info, err := os.Stat(filepath.Join(dir, name)); err != nil || info.IsDir() {
			return false
		}
	}
	return true
}

func envtestAssetsDir() (string, error) {
	if configured := os.Getenv("KUBEBUILDER_ASSETS"); configured != "" {
		return resolveEnvtestAssets(configured)
	}
	base, err := envtest.SetupEnvtestDefaultBinaryAssetsDirectory()
	if err != nil {
		return "", err
	}
	return resolveEnvtestAssets(base)
}

func setupEnvtest(t *testing.T) (kubernetes.Interface, func()) {
	t.Helper()

	assetsDir, err := envtestAssetsDir()
	require.NoError(t, err, "failed to find envtest binary assets directory")

	testEnv := &envtest.Environment{
		BinaryAssetsDirectory: assetsDir,
	}
	cfg, err := testEnv.Start()
	require.NoError(t, err, "failed to start envtest")

	clientset, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err, "failed to create clientset")

	return clientset, func() {
		require.NoError(t, testEnv.Stop(), "failed to stop envtest")
	}
}

func makeGPUDevice(name string, numaNode int64, pcieRoot string) resourcev1.Device {
	return resourcev1.Device{
		Name: name,
		Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
			"nodepartition.dra.k8s.io/numaNode": {IntValue: intPtr(numaNode)},
			"resource.kubernetes.io/pcieRoot":   {StringValue: strPtr(pcieRoot)},
			"nodepartition.dra.k8s.io/socket":   {IntValue: intPtr(numaNode / 2)},
		},
	}
}

func makeNICDevice(name string, numaNode int64, pcieRoot string) resourcev1.Device {
	return resourcev1.Device{
		Name: name,
		Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
			"nodepartition.dra.k8s.io/numaNode": {IntValue: intPtr(numaNode)},
			"resource.kubernetes.io/pcieRoot":   {StringValue: strPtr(pcieRoot)},
		},
	}
}

func createResourceSlice(t *testing.T, ctx context.Context, client kubernetes.Interface,
	name, driver, nodeName, poolName string, devices []resourcev1.Device,
) {
	t.Helper()
	_, err := client.ResourceV1().ResourceSlices().Create(ctx, &resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: resourcev1.ResourceSliceSpec{
			Driver:   driver,
			NodeName: &nodeName,
			Pool: resourcev1.ResourcePool{
				Name:               poolName,
				Generation:         1,
				ResourceSliceCount: 1,
			},
			Devices: devices,
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create ResourceSlice %s", name)
}

// waitForDeviceClasses polls until at least minCount coordinator-managed DeviceClasses exist.
func waitForDeviceClasses(t *testing.T, ctx context.Context, client kubernetes.Interface, minCount int) []resourcev1.DeviceClass { //nolint:unparam // minCount varies in intent even if current tests all use 1
	t.Helper()

	var result []resourcev1.DeviceClass
	deadline := time.After(30 * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %d DeviceClasses, got %d", minCount, len(result))
		case <-ticker.C:
			classes, err := client.ResourceV1().DeviceClasses().List(ctx, metav1.ListOptions{
				LabelSelector: "nodepartition.dra.k8s.io/managed=true",
			})
			if err != nil {
				continue
			}
			if len(classes.Items) >= minCount {
				return classes.Items
			}
			result = classes.Items
		}
	}
}

func waitForGPUInferenceDeviceClass(t *testing.T, ctx context.Context, client kubernetes.Interface, matches func(*resourcev1.DeviceClass) bool) *resourcev1.DeviceClass {
	t.Helper()

	var result *resourcev1.DeviceClass
	require.Eventually(t, func() bool {
		deviceClass, err := client.ResourceV1().DeviceClasses().Get(ctx, "gpu-inference", metav1.GetOptions{})
		if err != nil || !matches(deviceClass) {
			return false
		}
		result = deviceClass
		return true
	}, 30*time.Second, 500*time.Millisecond)
	return result
}

func TestIntegration_IntentMetadataPropagation(t *testing.T) {
	client, teardown := setupEnvtest(t)
	defer teardown()

	ctx := context.Background()
	createResourceSlice(t, ctx, client, "gpu-slice-intent", "gpu.nvidia.com", "node-1", "gpu-pool", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
	})
	createResourceSlice(t, ctx, client, "nic-slice-intent", "rdma.mellanox.com", "node-1", "nic-pool", []resourcev1.Device{
		makeNICDevice("nic-0", 0, "pcie-0"),
	})
	grouping := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gpu-inference",
			Namespace: "default",
			Labels:    map[string]string{controller.DeviceGroupingLabel: "true"},
		},
		Data: map[string]string{
			"name":          "gpu-inference",
			"alignment":     "pcieRoot",
			"devices":       "- class: gpu.nvidia.com\n  count: 1\n- class: rdma.mellanox.com\n  count: 1",
			"displayName":   "GPU inference",
			"description":   "GPU and NIC resources for inference",
			"intentLevel":   "workload",
			"categories":    "ai,inference",
			"workloadTypes": "training,serving",
			"visibility":    "public",
		},
	}
	_, err := client.CoreV1().ConfigMaps("default").Create(ctx, grouping, metav1.CreateOptions{})
	require.NoError(t, err)

	startController := func() (context.CancelFunc, chan error) {
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		ctrl := controller.NewController(client, controller.CoordinatorDriverName, controller.PartitionModeGroupings)
		go func() { done <- ctrl.Run(runCtx) }()
		return cancel, done
	}
	cancel, done := startController()
	defer func() {
		if cancel != nil {
			cancel()
			require.NoError(t, <-done)
		}
	}()

	intentLevelKey := controller.CoordinatorDriverName + "/intent-level"
	visibilityKey := controller.CoordinatorDriverName + "/visibility"
	displayNameKey := controller.CoordinatorDriverName + "/display-name"
	descriptionKey := controller.CoordinatorDriverName + "/description"
	categoriesKey := controller.CoordinatorDriverName + "/categories"
	workloadTypesKey := controller.CoordinatorDriverName + "/workload-types"
	deviceClass := waitForGPUInferenceDeviceClass(t, ctx, client, func(dc *resourcev1.DeviceClass) bool {
		return dc.Labels[controller.CoordinatorDriverName+"/managed"] == "true"
	})
	require.Equal(t, "workload", deviceClass.Labels[intentLevelKey])
	require.Equal(t, "public", deviceClass.Labels[visibilityKey])
	require.Equal(t, "GPU inference", deviceClass.Annotations[displayNameKey])
	require.Equal(t, "GPU and NIC resources for inference", deviceClass.Annotations[descriptionKey])
	require.Equal(t, "ai,inference", deviceClass.Annotations[categoriesKey])
	require.Equal(t, "training,serving", deviceClass.Annotations[workloadTypesKey])
	originalConfig := deviceClass.Spec.Config
	deviceClass.Labels["foreign.example.com/team"] = "platform"
	deviceClass.Annotations["foreign.example.com/owner"] = "controller"
	_, err = client.ResourceV1().DeviceClasses().Update(ctx, deviceClass, metav1.UpdateOptions{})
	require.NoError(t, err)

	grouping, err = client.CoreV1().ConfigMaps("default").Get(ctx, grouping.Name, metav1.GetOptions{})
	require.NoError(t, err)
	grouping.Data["displayName"] = "AI training accelerator"
	_, err = client.CoreV1().ConfigMaps("default").Update(ctx, grouping, metav1.UpdateOptions{})
	require.NoError(t, err)
	deviceClass = waitForGPUInferenceDeviceClass(t, ctx, client, func(dc *resourcev1.DeviceClass) bool {
		return dc.Annotations[displayNameKey] == "AI training accelerator"
	})
	assert.Equal(t, originalConfig, deviceClass.Spec.Config)
	assert.Equal(t, "platform", deviceClass.Labels["foreign.example.com/team"])
	assert.Equal(t, "controller", deviceClass.Annotations["foreign.example.com/owner"])

	grouping, err = client.CoreV1().ConfigMaps("default").Get(ctx, grouping.Name, metav1.GetOptions{})
	require.NoError(t, err)
	delete(grouping.Data, "description")
	delete(grouping.Data, "visibility")
	_, err = client.CoreV1().ConfigMaps("default").Update(ctx, grouping, metav1.UpdateOptions{})
	require.NoError(t, err)
	deviceClass = waitForGPUInferenceDeviceClass(t, ctx, client, func(dc *resourcev1.DeviceClass) bool {
		_, exists := dc.Annotations[descriptionKey]
		_, hasVisibility := dc.Labels[visibilityKey]
		return !exists && !hasVisibility
	})
	assert.Equal(t, originalConfig, deviceClass.Spec.Config)
	assert.Equal(t, "platform", deviceClass.Labels["foreign.example.com/team"])
	assert.Equal(t, "controller", deviceClass.Annotations["foreign.example.com/owner"])

	cancel()
	require.NoError(t, <-done)
	cancel = nil
	require.NoError(t, client.ResourceV1().DeviceClasses().Delete(ctx, "gpu-inference", metav1.DeleteOptions{}))
	cancel, done = startController()
	deviceClass = waitForGPUInferenceDeviceClass(t, ctx, client, func(dc *resourcev1.DeviceClass) bool {
		_, hasDescription := dc.Annotations[descriptionKey]
		_, hasVisibility := dc.Labels[visibilityKey]
		return dc.Labels[intentLevelKey] == "workload" && dc.Annotations[displayNameKey] == "AI training accelerator" && !hasDescription && !hasVisibility
	})
	assert.Equal(t, originalConfig, deviceClass.Spec.Config)
}

func TestIntegration_ControllerCreatesDeviceClasses(t *testing.T) {
	client, teardown := setupEnvtest(t)
	defer teardown()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ctrl := controller.NewController(client, "nodepartition.dra.k8s.io", controller.PartitionModeAuto)
	go func() {
		if err := ctrl.Run(ctx); err != nil && ctx.Err() == nil {
			t.Errorf("controller failed: %v", err)
		}
	}()

	// Create ResourceSlices from two different DRA drivers on the same node
	createResourceSlice(t, ctx, client, "gpu-slice-node1", "gpu.nvidia.com", "node-1", "gpu-pool", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
		makeGPUDevice("gpu-1", 0, "pcie-1"),
		makeGPUDevice("gpu-2", 1, "pcie-2"),
		makeGPUDevice("gpu-3", 1, "pcie-3"),
	})

	createResourceSlice(t, ctx, client, "nic-slice-node1", "rdma.mellanox.com", "node-1", "nic-pool", []resourcev1.Device{
		makeNICDevice("nic-0", 0, "pcie-0"),
		makeNICDevice("nic-1", 1, "pcie-2"),
	})

	// Wait for DeviceClasses to be created
	classes := waitForDeviceClasses(t, ctx, client, 1)

	// Verify DeviceClasses were created with correct labels
	for _, dc := range classes {
		assert.Equal(t, "true", dc.Labels["nodepartition.dra.k8s.io/managed"])
		assert.NotEmpty(t, dc.Spec.Selectors, "DeviceClass should have selectors")
		assert.NotEmpty(t, dc.Spec.Config, "DeviceClass should have config")

		// Verify opaque config contains a valid PartitionConfig
		if len(dc.Spec.Config) > 0 && dc.Spec.Config[0].Opaque != nil {
			var partConfig controller.PartitionConfig
			err := json.Unmarshal(dc.Spec.Config[0].Opaque.Parameters.Raw, &partConfig)
			assert.NoError(t, err, "should unmarshal PartitionConfig")
			assert.Equal(t, "PartitionConfig", partConfig.Kind)
			assert.NotEmpty(t, partConfig.SubResources, "should have sub-resources")
		}
	}
}

func TestIntegration_DeviceClassCleanup(t *testing.T) {
	client, teardown := setupEnvtest(t)
	defer teardown()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ctrl := controller.NewController(client, "nodepartition.dra.k8s.io", controller.PartitionModeAuto)
	go func() {
		if err := ctrl.Run(ctx); err != nil && ctx.Err() == nil {
			t.Errorf("controller failed: %v", err)
		}
	}()

	// Create slices for node-1
	createResourceSlice(t, ctx, client, "gpu-slice-n1", "gpu.nvidia.com", "node-1", "gpu-pool", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
		makeGPUDevice("gpu-1", 0, "pcie-1"),
	})

	// Wait for DeviceClasses
	waitForDeviceClasses(t, ctx, client, 1)

	// Delete the source slice (simulates node removal)
	err := client.ResourceV1().ResourceSlices().Delete(ctx, "gpu-slice-n1", metav1.DeleteOptions{})
	require.NoError(t, err)

	// Wait for DeviceClasses to be cleaned up
	deadline := time.After(30 * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for DeviceClasses to be cleaned up")
		case <-ticker.C:
			classes, err := client.ResourceV1().DeviceClasses().List(ctx, metav1.ListOptions{
				LabelSelector: "nodepartition.dra.k8s.io/managed=true",
			})
			require.NoError(t, err)
			if len(classes.Items) == 0 {
				return // Cleanup successful
			}
		}
	}
}

func TestIntegration_TopologyRulesFromConfigMap(t *testing.T) {
	client, teardown := setupEnvtest(t)
	defer teardown()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Logf("namespace creation: %v", err)
	}

	ctrl := controller.NewController(client, "nodepartition.dra.k8s.io", controller.PartitionModeAuto)
	go func() {
		if err := ctrl.Run(ctx); err != nil && ctx.Err() == nil {
			t.Errorf("controller failed: %v", err)
		}
	}()

	// Create a topology rule ConfigMap for NVLink domain grouping
	_, err = client.CoreV1().ConfigMaps("default").Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nvlink-rule",
			Namespace: "default",
			Labels: map[string]string{
				"nodepartition.dra.k8s.io/topology-rule": "true",
			},
		},
		Data: map[string]string{
			"attribute":    "gpu.nvidia.com/nvlinkDomain",
			"type":         "int",
			"driver":       "gpu.nvidia.com",
			"partitioning": "group",
			"constraint":   "match",
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	time.Sleep(3 * time.Second)

	// Create GPU slices with NVLink domain attributes
	nvlinkDomain0 := int64(0)
	nvlinkDomain1 := int64(1)

	_, err = client.ResourceV1().ResourceSlices().Create(ctx, &resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-slice-nvlink"},
		Spec: resourcev1.ResourceSliceSpec{
			Driver:   "gpu.nvidia.com",
			NodeName: strPtr("node-1"),
			Pool: resourcev1.ResourcePool{
				Name:               "gpu-pool",
				Generation:         1,
				ResourceSliceCount: 1,
			},
			Devices: []resourcev1.Device{
				{
					Name: "gpu-0",
					Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						"nodepartition.dra.k8s.io/numaNode": {IntValue: intPtr(0)},
						"resource.kubernetes.io/pcieRoot":   {StringValue: strPtr("pcie-0")},
						"gpu.nvidia.com/nvlinkDomain":       {IntValue: &nvlinkDomain0},
					},
				},
				{
					Name: "gpu-1",
					Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						"nodepartition.dra.k8s.io/numaNode": {IntValue: intPtr(0)},
						"resource.kubernetes.io/pcieRoot":   {StringValue: strPtr("pcie-1")},
						"gpu.nvidia.com/nvlinkDomain":       {IntValue: &nvlinkDomain0},
					},
				},
				{
					Name: "gpu-2",
					Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						"nodepartition.dra.k8s.io/numaNode": {IntValue: intPtr(1)},
						"resource.kubernetes.io/pcieRoot":   {StringValue: strPtr("pcie-2")},
						"gpu.nvidia.com/nvlinkDomain":       {IntValue: &nvlinkDomain1},
					},
				},
				{
					Name: "gpu-3",
					Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						"nodepartition.dra.k8s.io/numaNode": {IntValue: intPtr(1)},
						"resource.kubernetes.io/pcieRoot":   {StringValue: strPtr("pcie-3")},
						"gpu.nvidia.com/nvlinkDomain":       {IntValue: &nvlinkDomain1},
					},
				},
			},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	// Wait for DeviceClasses
	classes := waitForDeviceClasses(t, ctx, client, 1)

	// Verify at least one DeviceClass has NVLink match constraint in its PartitionConfig
	foundNVLinkConstraint := false
	for _, dc := range classes {
		if len(dc.Spec.Config) > 0 && dc.Spec.Config[0].Opaque != nil {
			var partConfig controller.PartitionConfig
			if err := json.Unmarshal(dc.Spec.Config[0].Opaque.Parameters.Raw, &partConfig); err == nil {
				for _, alignment := range partConfig.Alignments {
					if alignment.Attribute == "gpu.nvidia.com/nvlinkDomain" {
						foundNVLinkConstraint = true
					}
				}
			}
		}
	}
	assert.True(t, foundNVLinkConstraint, "expected NVLink match constraint in DeviceClass PartitionConfig")
}

func TestIntegration_MultipleNodesMultipleDrivers(t *testing.T) {
	client, teardown := setupEnvtest(t)
	defer teardown()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ctrl := controller.NewController(client, "nodepartition.dra.k8s.io", controller.PartitionModeAuto)
	go func() {
		if err := ctrl.Run(ctx); err != nil && ctx.Err() == nil {
			t.Errorf("controller failed: %v", err)
		}
	}()

	// Create slices for 2 nodes, each with GPUs and NICs
	for i := 1; i <= 2; i++ {
		nodeName := fmt.Sprintf("node-%d", i)

		createResourceSlice(t, ctx, client,
			fmt.Sprintf("gpu-slice-%s", nodeName), "gpu.nvidia.com", nodeName, "gpu-pool",
			[]resourcev1.Device{
				makeGPUDevice("gpu-0", 0, "pcie-0"),
				makeGPUDevice("gpu-1", 1, "pcie-1"),
			},
		)

		createResourceSlice(t, ctx, client,
			fmt.Sprintf("nic-slice-%s", nodeName), "rdma.mellanox.com", nodeName, "nic-pool",
			[]resourcev1.Device{
				makeNICDevice("nic-0", 0, "pcie-0"),
				makeNICDevice("nic-1", 1, "pcie-1"),
			},
		)
	}

	// Wait for DeviceClasses (same profile on both nodes → same DeviceClasses)
	classes := waitForDeviceClasses(t, ctx, client, 1)
	assert.NotEmpty(t, classes, "expected DeviceClasses from multi-node topology")
}

func TestIntegration_DriverSpecificAttributeMapping(t *testing.T) {
	client, teardown := setupEnvtest(t)
	defer teardown()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Logf("namespace creation: %v", err)
	}

	ctrl := controller.NewController(client, "nodepartition.dra.k8s.io", controller.PartitionModeAuto)
	go func() {
		if err := ctrl.Run(ctx); err != nil && ctx.Err() == nil {
			t.Errorf("controller failed: %v", err)
		}
	}()

	// Create topology rules that map mock-device driver-specific attributes
	for _, rule := range []struct {
		name, attribute, attrType, mapsTo string
	}{
		{"mock-accel-numa", "mock-accel.example.com/numaNode", "int", "numaNode"},
		{"mock-accel-pci", "mock-accel.example.com/pciAddress", "string", "pcieRoot"},
	} {
		_, err := client.CoreV1().ConfigMaps("default").Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      rule.name,
				Namespace: "default",
				Labels:    map[string]string{"nodepartition.dra.k8s.io/topology-rule": "true"},
			},
			Data: map[string]string{
				"attribute": rule.attribute,
				"type":      rule.attrType,
				"driver":    "mock-accel.example.com",
				"mapsTo":    rule.mapsTo,
			},
		}, metav1.CreateOptions{})
		require.NoError(t, err)
	}

	time.Sleep(3 * time.Second)

	// Create ResourceSlices using driver-specific attribute names
	numa0 := int64(0)
	numa1 := int64(1)
	pci0 := "0000:11:00.0"
	pci1 := "0000:11:00.1"
	pci2 := "0000:21:00.0"
	pci3 := "0000:21:00.1"

	for _, dev := range []struct {
		name, sliceName string
		numa            *int64
		pci             *string
	}{
		{"mock0", "mock-accel-mock0", &numa0, &pci0},
		{"mock1", "mock-accel-mock1", &numa0, &pci1},
		{"mock2", "mock-accel-mock2", &numa1, &pci2},
		{"mock3", "mock-accel-mock3", &numa1, &pci3},
	} {
		nodeName := "node-1"
		_, err := client.ResourceV1().ResourceSlices().Create(ctx, &resourcev1.ResourceSlice{
			ObjectMeta: metav1.ObjectMeta{Name: dev.sliceName},
			Spec: resourcev1.ResourceSliceSpec{
				Driver:   "mock-accel.example.com",
				NodeName: &nodeName,
				Pool: resourcev1.ResourcePool{
					Name:               dev.name,
					Generation:         1,
					ResourceSliceCount: 1,
				},
				Devices: []resourcev1.Device{
					{
						Name: dev.name,
						Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
							"mock-accel.example.com/numaNode":   {IntValue: dev.numa},
							"mock-accel.example.com/pciAddress": {StringValue: dev.pci},
						},
					},
				},
			},
		}, metav1.CreateOptions{})
		require.NoError(t, err)
	}

	// Wait for DeviceClasses from attribute mapping
	classes := waitForDeviceClasses(t, ctx, client, 1)
	assert.NotEmpty(t, classes, "expected DeviceClasses from driver-specific attribute mapping")

	// Verify at least one DeviceClass has a partition type
	foundPartitionType := false
	for _, dc := range classes {
		if pt := dc.Labels["nodepartition.dra.k8s.io/partitionType"]; pt != "" {
			foundPartitionType = true
		}
	}
	assert.True(t, foundPartitionType, "expected partitionType label on DeviceClass")
}

func TestValidPartitionMode(t *testing.T) {
	tests := []struct {
		mode string
		want bool
	}{
		{"auto", true},
		{"partitions", true},
		{"groupings", true},
		{"", false},
		{"invalid", false},
		{"PARTITIONS", false},
		{"Auto", false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("mode=%q", tt.mode), func(t *testing.T) {
			assert.Equal(t, tt.want, controller.ValidPartitionMode(tt.mode))
		})
	}
}

func TestIntegration_PartitionModePartitionsIgnoresGroupings(t *testing.T) {
	client, teardown := setupEnvtest(t)
	defer teardown()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Logf("namespace creation: %v", err)
	}

	// Create a device grouping ConfigMap (should be ignored in partitions mode)
	_, err = client.CoreV1().ConfigMaps("default").Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gpu-nic-pair",
			Namespace: "default",
			Labels:    map[string]string{"nodepartition.dra.k8s.io/device-grouping": "true"},
		},
		Data: map[string]string{
			"name":      "gpu-nic-pair",
			"alignment": "pcieRoot",
			"devices":   "- class: gpu.nvidia.com\n  count: 1\n- class: rdma.mellanox.com\n  count: 1\n",
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	ctrl := controller.NewController(client, "nodepartition.dra.k8s.io", controller.PartitionModePartitions)
	go func() {
		if err := ctrl.Run(ctx); err != nil && ctx.Err() == nil {
			t.Errorf("controller failed: %v", err)
		}
	}()

	createResourceSlice(t, ctx, client, "gpu-slice-pm", "gpu.nvidia.com", "node-1", "gpu-pool", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
		makeGPUDevice("gpu-1", 1, "pcie-1"),
	})

	classes := waitForDeviceClasses(t, ctx, client, 1)

	// In partitions mode, DeviceClasses should have partitionType labels (old model)
	foundPartitionType := false
	for _, dc := range classes {
		if pt := dc.Labels["nodepartition.dra.k8s.io/partitionType"]; pt != "" {
			foundPartitionType = true
		}
	}
	assert.True(t, foundPartitionType, "partitions mode should produce partition-style DeviceClasses")
}

func TestIntegration_PartitionModeGroupingsNoConfigMaps(t *testing.T) {
	client, teardown := setupEnvtest(t)
	defer teardown()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ctrl := controller.NewController(client, "nodepartition.dra.k8s.io", controller.PartitionModeGroupings)
	go func() {
		if err := ctrl.Run(ctx); err != nil && ctx.Err() == nil {
			t.Errorf("controller failed: %v", err)
		}
	}()

	createResourceSlice(t, ctx, client, "gpu-slice-gm", "gpu.nvidia.com", "node-1", "gpu-pool", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
		makeGPUDevice("gpu-1", 1, "pcie-1"),
	})

	// Give the controller time to reconcile
	time.Sleep(5 * time.Second)

	// With groupings mode and no ConfigMaps, no DeviceClasses should be created
	classes, err := client.ResourceV1().DeviceClasses().List(ctx, metav1.ListOptions{
		LabelSelector: "nodepartition.dra.k8s.io/managed=true",
	})
	require.NoError(t, err)
	assert.Empty(t, classes.Items, "groupings mode with no ConfigMaps should produce zero DeviceClasses")
}
