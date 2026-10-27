package controller

import (
	"bytes"
	"context"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	klog "k8s.io/klog/v2"
)

func captureKlog(t *testing.T, call func()) string {
	t.Helper()
	var output bytes.Buffer
	klog.LogToStderr(false)
	klog.SetOutput(&output)
	t.Cleanup(func() {
		klog.SetOutput(os.Stderr)
		klog.LogToStderr(true)
	})
	call()
	return output.String()
}

func addGPUAndNICTopology(ctrl *Controller) {
	ctrl.Model().UpdateFromResourceSlice(makeResourceSlice("gpu-slice", "gpu.nvidia.com", "node-1", "gpu-pool", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
	}))
	ctrl.Model().UpdateFromResourceSlice(makeResourceSlice("nic-slice", "rdma.mellanox.com", "node-1", "nic-pool", []resourcev1.Device{
		makeNICDevice("nic-0", 0, "pcie-0"),
	}))
}

func TestController_Reconcile_DuplicateUserNamesLogsActionableError(t *testing.T) {
	ctrl := NewController(k8sfake.NewSimpleClientset(), CoordinatorDriverName, PartitionModeGroupings)
	for _, cmName := range []string{"first", "second"} {
		require.NoError(t, ctrl.groupingStore.LoadFromConfigMap(
			makeGroupingConfigMap(cmName, validGroupingData("shared-offering")),
		))
	}

	logs := captureKlog(t, func() {
		require.NoError(t, ctrl.reconcile(context.Background()))
	})

	assert.Contains(t, logs, "data.name")
	assert.Contains(t, logs, "shared-offering")
	assert.Contains(t, logs, "default/first")
	assert.Contains(t, logs, "default/second")
}

func TestController_NonLeaderDoesNotPublishUntilEnabled(t *testing.T) {
	client := k8sfake.NewSimpleClientset()
	ctrl := NewController(client, CoordinatorDriverName, PartitionModePartitions)
	addGPUAndNICTopology(ctrl)
	ctrl.synced.Store(true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ctrl.RunWorker(ctx) }()

	classes, err := client.ResourceV1().DeviceClasses().List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, classes.Items)

	ctrl.SetPublishing(true)
	ctrl.EnqueueReconcile()
	require.Eventually(t, func() bool {
		classes, err := client.ResourceV1().DeviceClasses().List(ctx, metav1.ListOptions{})
		return err == nil && len(classes.Items) > 0
	}, 2*time.Second, 20*time.Millisecond)
}

func TestController_TopologyRuleEventsRefreshModelWithoutReconcile(t *testing.T) {
	ctrl := NewController(k8sfake.NewSimpleClientset(), CoordinatorDriverName, PartitionModePartitions)
	ctrl.synced.Store(true)
	customNUMA := int64(3)
	ctrl.model.UpdateFromResourceSlice(makeResourceSlice("custom-slice", "mock-device.example.com", "node-1", "pool", []resourcev1.Device{{
		Name: "device-0",
		Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
			"mock-device.example.com/numa": {IntValue: &customNUMA},
		},
	}}))

	rule := makeTopologyRuleConfigMap("custom-rule", "default", map[string]string{
		"attribute": "mock-device.example.com/numa",
		"type":      "int",
		"driver":    "mock-device.example.com",
		"mapsTo":    "numaNode",
	})
	ctrl.onConfigMapAdd(rule)

	require.Len(t, ctrl.model.GetRules(), 1)
	topology := ctrl.model.GetNodeTopology("node-1")
	require.NotNil(t, topology)
	require.Len(t, topology.AllDevices(), 1)
	require.NotNil(t, topology.AllDevices()[0].NUMANode)
	assert.Equal(t, customNUMA, *topology.AllDevices()[0].NUMANode)

	updated := rule.DeepCopy()
	updated.Data["mapsTo"] = "pcieRoot"
	updated.Data["type"] = "string"
	updated.Data["attribute"] = "mock-device.example.com/pcie"
	ctrl.onConfigMapUpdate(rule, updated)

	require.Len(t, ctrl.model.GetRules(), 1)
	assert.Equal(t, "mock-device.example.com/pcie", ctrl.model.GetRules()[0].Attribute)
	topology = ctrl.model.GetNodeTopology("node-1")
	require.NotNil(t, topology)
	require.Len(t, topology.AllDevices(), 1)
	assert.Nil(t, topology.AllDevices()[0].NUMANode)

	ctrl.onConfigMapDelete(updated)
	assert.Empty(t, ctrl.model.GetRules())
}

func TestController_TopologyRuleLabelRemovalClearsModel(t *testing.T) {
	ctrl := NewController(k8sfake.NewSimpleClientset(), CoordinatorDriverName, PartitionModePartitions)
	rule := makeTopologyRuleConfigMap("custom-rule", "default", map[string]string{
		"attribute": "mock-device.example.com/numa",
		"type":      "int",
		"driver":    "mock-device.example.com",
		"mapsTo":    "numaNode",
	})
	ctrl.onConfigMapAdd(rule)
	require.Len(t, ctrl.model.GetRules(), 1)

	withoutRuleLabel := rule.DeepCopy()
	withoutRuleLabel.Labels = nil
	ctrl.onConfigMapUpdate(rule, withoutRuleLabel)
	assert.Empty(t, ctrl.model.GetRules())
}

func TestController_Reconcile_AutoGroupingOverrideIsNotDiagnosedAsDuplicate(t *testing.T) {
	ctrl := NewController(k8sfake.NewSimpleClientset(), CoordinatorDriverName, PartitionModeGroupings)
	addGPUAndNICTopology(ctrl)
	autoGroupings := ctrl.partitionBuilder.DetectPCIeRootPairings()
	require.Len(t, autoGroupings, 1)
	groupingName := autoGroupings[0].Name
	data := validGroupingData(groupingName)
	data["devices"] = "- class: gpu.nvidia.com\n  count: 1\n- class: rdma.mellanox.com\n  count: 1"
	data["displayName"] = "User-defined override"
	require.NoError(t, ctrl.groupingStore.LoadFromConfigMap(makeGroupingConfigMap("override", data)))

	logs := captureKlog(t, func() {
		require.NoError(t, ctrl.reconcile(context.Background()))
	})
	assert.NotContains(t, logs, "Duplicate user-defined device grouping")
	deviceClass, err := ctrl.client.ResourceV1().DeviceClasses().Get(context.Background(), sanitizeForName(groupingName), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "User-defined override", deviceClass.Annotations[CoordinatorDriverName+"/display-name"])
}

func TestController_Reconcile_AutoAndUserSanitizedCollisionPreservesExistingDeviceClass(t *testing.T) {
	ctrl := NewController(k8sfake.NewSimpleClientset(), CoordinatorDriverName, PartitionModeGroupings)
	addGPUAndNICTopology(ctrl)
	autoGroupings := ctrl.partitionBuilder.DetectPCIeRootPairings()
	require.Len(t, autoGroupings, 1)
	autoName := autoGroupings[0].Name
	userName := strings.ReplaceAll(autoName, "-", ".")
	require.Equal(t, sanitizeForName(autoName), sanitizeForName(userName))

	existing := &resourcev1.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: sanitizeForName(autoName),
			Labels: map[string]string{
				CoordinatorDriverName + "/managed":  "true",
				CoordinatorDriverName + "/grouping": "previous-offering",
			},
		},
	}
	_, err := ctrl.client.ResourceV1().DeviceClasses().Create(context.Background(), existing, metav1.CreateOptions{})
	require.NoError(t, err)
	data := validGroupingData(userName)
	data["devices"] = "- class: gpu.nvidia.com\n  count: 1\n- class: rdma.mellanox.com\n  count: 1"
	require.NoError(t, ctrl.groupingStore.LoadFromConfigMap(makeGroupingConfigMap("user-conflict", data)))

	err = ctrl.reconcile(context.Background())
	assert.Error(t, err)
	if err != nil {
		for _, want := range []string{"data.name", autoName, userName, "auto-detected", "default/user-conflict"} {
			assert.Contains(t, err.Error(), want)
		}
	}
	after, getErr := ctrl.client.ResourceV1().DeviceClasses().Get(context.Background(), existing.Name, metav1.GetOptions{})
	require.NoError(t, getErr)
	assert.Equal(t, existing.Labels, after.Labels)
}

func TestController_Reconcile_SanitizedCollisionPreservesExistingDeviceClass(t *testing.T) {
	existing := &resourcev1.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gpu-inference",
			Labels: map[string]string{
				CoordinatorDriverName + "/managed":  "true",
				CoordinatorDriverName + "/grouping": "gpu-inference",
			},
		},
	}
	client := k8sfake.NewSimpleClientset(existing)
	ctrl := NewController(client, CoordinatorDriverName, PartitionModeGroupings)
	for _, groupingName := range []string{"gpu.inference", "gpu-inference"} {
		cmName := strings.ReplaceAll(groupingName, ".", "-dot-")
		data := validGroupingData(groupingName)
		require.NoError(t, ctrl.groupingStore.LoadFromConfigMap(makeGroupingConfigMap(cmName, data)))
	}

	err := ctrl.reconcile(context.Background())
	assert.Error(t, err)
	if err != nil {
		for _, want := range []string{"data.name", "gpu.inference", "gpu-inference", "default/gpu-dot-inference", "default/gpu-inference"} {
			assert.Contains(t, err.Error(), want)
		}
	}

	after, getErr := client.ResourceV1().DeviceClasses().Get(context.Background(), existing.Name, metav1.GetOptions{})
	require.NoError(t, getErr, "a sanitized-name collision must not run stale-class cleanup")
	assert.Equal(t, existing.Labels, after.Labels)
}

func TestController_Reconcile_GeneratedNameCollisionPreservesExistingDeviceClass(t *testing.T) {
	existing := &resourcev1.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gpu-rail0",
			Labels: map[string]string{
				CoordinatorDriverName + "/managed":  "true",
				CoordinatorDriverName + "/grouping": "previous-offering",
			},
			Annotations: map[string]string{"foreign.example.com/owner": "preserve"},
		},
	}
	client := k8sfake.NewSimpleClientset(existing)
	ctrl := NewController(client, CoordinatorDriverName, PartitionModeGroupings)
	ctrl.Model().UpdateFromResourceSlice(makeResourceSlice("gpu-slice", "gpu.nvidia.com", "node-1", "gpu-pool", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
	}))

	for _, groupingName := range []string{"gpu", "gpu-rail0"} {
		data := validGroupingData(groupingName)
		data["devices"] = "- class: gpu.nvidia.com\n  count: 1"
		if groupingName == "gpu-rail0" {
			data["intentLevel"] = "workload"
		}
		require.NoError(t, ctrl.groupingStore.LoadFromConfigMap(makeGroupingConfigMap(groupingName, data)))
	}

	err := ctrl.reconcile(context.Background())
	assert.Error(t, err)
	if err != nil {
		for _, want := range []string{"gpu-rail0", "default/gpu", "default/gpu-rail0"} {
			assert.Contains(t, err.Error(), want)
		}
	}

	after, getErr := client.ResourceV1().DeviceClasses().Get(context.Background(), existing.Name, metav1.GetOptions{})
	require.NoError(t, getErr)
	assert.Equal(t, existing.Labels, after.Labels)
	assert.Equal(t, existing.Annotations, after.Annotations)
}

func TestController_Reconcile_GeneratedNameCollisionUsesPublishedRails(t *testing.T) {
	ctrl := NewController(k8sfake.NewSimpleClientset(), CoordinatorDriverName, PartitionModeGroupings)
	ctrl.Model().UpdateFromResourceSlice(makeResourceSlice("gpu-slice", "gpu.nvidia.com", "node-1", "gpu-pool", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
		makeGPUDevice("gpu-1", 0, "pcie-0"),
	}))
	for _, groupingName := range []string{"gpu", "gpu-rail1"} {
		data := validGroupingData(groupingName)
		data["devices"] = "- class: gpu.nvidia.com\n  count: 1"
		require.NoError(t, ctrl.groupingStore.LoadFromConfigMap(makeGroupingConfigMap(groupingName, data)))
	}

	require.NoError(t, ctrl.reconcile(context.Background()))
	deviceClass, err := ctrl.client.ResourceV1().DeviceClasses().Get(context.Background(), "gpu-rail1", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "gpu-rail1", deviceClass.Labels[CoordinatorDriverName+"/grouping"])
	assert.NotContains(t, deviceClass.Labels, CoordinatorDriverName+"/railIndex")
}

func TestGroupingConfigMapUpdate_InvalidUpdateKeepsCachedGrouping(t *testing.T) {
	ctrl := NewController(k8sfake.NewSimpleClientset(), CoordinatorDriverName, PartitionModeGroupings)
	original := makeGroupingConfigMap("test", validGroupingData("existing-offering"))
	require.NoError(t, ctrl.groupingStore.LoadFromConfigMap(original))
	updated := original.DeepCopy()
	updated.Data["intentLevel"] = "unknown"

	logs := captureKlog(t, func() {
		ctrl.onGroupingConfigMapUpdate(original, updated)
	})

	assert.Contains(t, logs, "default/test")
	assert.Contains(t, logs, "stale")
	groupings := ctrl.groupingStore.GetGroupings()
	require.Len(t, groupings, 1)
	assert.Equal(t, "existing-offering", groupings[0].Name)
}

func TestMergeGroupings_UserOverridesAuto(t *testing.T) {
	auto := []DeviceGrouping{
		{Name: "gpu-nic-pair", Alignment: "pcieRoot", Devices: []GroupingDevice{{Class: "gpu.auto", Count: 1}}},
	}
	user := []DeviceGrouping{
		{Name: "gpu-nic-pair", Alignment: "numaNode", Devices: []GroupingDevice{{Class: "gpu.user", Count: 2}}},
	}

	merged := mergeGroupings(auto, user)
	assert.Len(t, merged, 1)
	assert.Equal(t, "numaNode", merged[0].Alignment, "user-defined should override auto-detected")
	assert.Equal(t, "gpu.user", merged[0].Devices[0].Class)
}

func TestMergeGroupings_BothEmpty(t *testing.T) {
	merged := mergeGroupings(nil, nil)
	assert.Empty(t, merged)
}

func TestMergeGroupings_NoOverlap(t *testing.T) {
	auto := []DeviceGrouping{
		{Name: "auto-pair", Alignment: "pcieRoot"},
	}
	user := []DeviceGrouping{
		{Name: "user-pair", Alignment: "numaNode"},
	}

	merged := mergeGroupings(auto, user)
	assert.Len(t, merged, 2)

	names := make([]string, len(merged))
	for i, g := range merged {
		names[i] = g.Name
	}
	sort.Strings(names)
	assert.Equal(t, []string{"auto-pair", "user-pair"}, names)
}

func TestMergeGroupings_AutoOnly(t *testing.T) {
	auto := []DeviceGrouping{
		{Name: "pair-a"},
		{Name: "pair-b"},
	}
	merged := mergeGroupings(auto, nil)
	assert.Len(t, merged, 2)
}

func TestMergeGroupings_UserOnly(t *testing.T) {
	user := []DeviceGrouping{
		{Name: "pair-a"},
	}
	merged := mergeGroupings(nil, user)
	assert.Len(t, merged, 1)
}
