package controller

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func makeGroupingConfigMap(name string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				DeviceGroupingLabel: "true",
			},
		},
		Data: data,
	}
}

func validGroupingData(name string) map[string]string {
	return map[string]string{
		"name":      name,
		"alignment": "pcieRoot",
		"devices":   "- class: gpu.example.com\n  count: 1",
	}
}

func uniqueList(itemLength, count int) string {
	values := make([]string, count)
	for i := range values {
		values[i] = strings.Repeat("x", itemLength-1) + string(rune('a'+i))
	}
	return strings.Join(values, ",")
}

func TestParseDeviceGrouping_IntentMetadata(t *testing.T) {
	for _, level := range []string{"workload", "capability", "hardware-specific"} {
		t.Run(level, func(t *testing.T) {
			data := validGroupingData("test")
			data["displayName"] = "GPU inference"
			data["description"] = "GPU workloads"
			data["intentLevel"] = level
			data["categories"] = " ai,,inference, ,ai "
			data["workloadTypes"] = "training, serving,,"
			data["visibility"] = "public"

			grouping, err := parseDeviceGrouping(makeGroupingConfigMap("test", data))
			require.NoError(t, err)
			intent := grouping.Intent
			assert.Equal(t, "GPU inference", intent.DisplayName)
			assert.Equal(t, "GPU workloads", intent.Description)
			assert.Equal(t, level, intent.IntentLevel)
			assert.Equal(t, []string{"ai", "inference"}, intent.Categories)
			assert.Equal(t, []string{"training", "serving"}, intent.WorkloadTypes)
			assert.Equal(t, "public", intent.Visibility)
			assert.True(t, intent.IsOffering())
		})
	}

	t.Run("internal visibility", func(t *testing.T) {
		data := validGroupingData("test")
		data["visibility"] = "internal"
		grouping, err := parseDeviceGrouping(makeGroupingConfigMap("test", data))
		require.NoError(t, err)
		assert.Equal(t, "internal", grouping.Intent.Visibility)
	})

	t.Run("whitespace-only lists are empty", func(t *testing.T) {
		data := validGroupingData("test")
		data["categories"] = " , \t,  "
		data["workloadTypes"] = "\t, , "
		grouping, err := parseDeviceGrouping(makeGroupingConfigMap("test", data))
		require.NoError(t, err)
		assert.Empty(t, grouping.Intent.Categories)
		assert.Empty(t, grouping.Intent.WorkloadTypes)
		assert.False(t, grouping.Intent.IsOffering())
	})

	t.Run("unique list limit applies after deduplication", func(t *testing.T) {
		data := validGroupingData("test")
		unique := uniqueList(1, 16)
		data["categories"] = unique + "," + unique
		data["workloadTypes"] = unique + "," + unique
		grouping, err := parseDeviceGrouping(makeGroupingConfigMap("test", data))
		require.NoError(t, err)
		want := strings.Split(unique, ",")
		assert.Equal(t, want, grouping.Intent.Categories)
		assert.Equal(t, want, grouping.Intent.WorkloadTypes)
	})

	t.Run("unicode list items count code points after trimming", func(t *testing.T) {
		data := validGroupingData("test")
		category := strings.Repeat("é", 63)
		workloadType := strings.Repeat("界", 63)
		data["categories"] = " \t" + category + "  "
		data["workloadTypes"] = " \t" + workloadType + "  "
		grouping, err := parseDeviceGrouping(makeGroupingConfigMap("test", data))
		require.NoError(t, err)
		assert.Equal(t, []string{category}, grouping.Intent.Categories)
		assert.Equal(t, []string{workloadType}, grouping.Intent.WorkloadTypes)
	})

	t.Run("maximum boundaries", func(t *testing.T) {
		data := validGroupingData("test")
		data["displayName"] = strings.Repeat("é", 63)
		data["description"] = strings.Repeat("界", 1024)
		data["categories"] = uniqueList(63, 16)
		data["workloadTypes"] = uniqueList(63, 16)
		grouping, err := parseDeviceGrouping(makeGroupingConfigMap("test", data))
		require.NoError(t, err)
		assert.Equal(t, 16, len(grouping.Intent.Categories))
		assert.Equal(t, 16, len(grouping.Intent.WorkloadTypes))
	})

	t.Run("legacy config has no offering", func(t *testing.T) {
		grouping, err := parseDeviceGrouping(makeGroupingConfigMap("test", validGroupingData("test")))
		require.NoError(t, err)
		assert.False(t, grouping.Intent.IsOffering())
		assert.Empty(t, grouping.Intent.Visibility)
	})
}

func TestParseDeviceGrouping_PassthroughOffering(t *testing.T) {
	data := validGroupingData("gpu-vfio")
	data["mode"] = "passthrough"
	data["devices"] = "- class: gpu.example.com\n  requestClass: gpu-vfio.example.com\n  count: 2\n  passthrough: true"

	grouping, err := parseDeviceGrouping(makeGroupingConfigMap("gpu-vfio", data))
	require.NoError(t, err)
	assert.Equal(t, OfferingModePassthrough, grouping.Mode)
	require.Len(t, grouping.Devices, 1)
	assert.True(t, grouping.Devices[0].Passthrough)
}

func TestParseDeviceGrouping_PassthroughWithoutContractRejected(t *testing.T) {
	data := validGroupingData("gpu-vfio")
	data["mode"] = "passthrough"
	data["devices"] = "- class: gpu.example.com\n  count: 2\n  passthrough: true"

	_, err := parseDeviceGrouping(makeGroupingConfigMap("gpu-vfio", data))
	require.ErrorContains(t, err, "passthrough requires requestClass, selectors, or opaque config")
}

func TestParseDeviceGrouping_OpaqueParametersMustBeObject(t *testing.T) {
	for name, params := range map[string]string{"null": "null", "string": `"x"`, "array": "[1]"} {
		t.Run(name, func(t *testing.T) {
			data := validGroupingData("gpu-vfio")
			data["mode"] = "passthrough"
			data["devices"] = "- class: gpu.example.com\n  count: 1\n  passthrough: true\n  opaque:\n    driver: d\n    parameters: " + params
			_, err := parseDeviceGrouping(makeGroupingConfigMap("gpu-vfio", data))
			require.ErrorContains(t, err, "parameters must be a JSON object")
		})
	}
}

func TestParseDeviceGrouping_ExplicitRequestContract(t *testing.T) {
	data := validGroupingData("amd-gim-vfio")
	data["mode"] = "passthrough"
	data["devices"] = `
- class: gpu.amd.com
  requestClass: gpu.amd.com
  count: 1
  passthrough: true
  selectors:
    - 'device.attributes["gpu.amd.com"].type == "vfio"'
    - 'device.attributes["gpu.amd.com"].isVF == true'
  opaque:
    driver: gpu.resource.amd.com
    parameters:
      apiVersion: gpu.resource.amd.com/v1alpha1
      kind: VfioDeviceConfig
`

	grouping, err := parseDeviceGrouping(makeGroupingConfigMap("amd-gim-vfio", data))
	require.NoError(t, err)
	require.Len(t, grouping.Devices, 1)
	device := grouping.Devices[0]
	assert.Equal(t, "gpu.amd.com", device.Class)
	assert.Equal(t, "gpu.amd.com", device.RequestClass)
	assert.Equal(t, []string{
		`device.attributes["gpu.amd.com"].type == "vfio"`,
		`device.attributes["gpu.amd.com"].isVF == true`,
	}, device.Selectors)
	require.NotNil(t, device.Opaque)
	assert.Equal(t, "gpu.resource.amd.com", device.Opaque.Driver)
	assert.JSONEq(t, `{"apiVersion":"gpu.resource.amd.com/v1alpha1","kind":"VfioDeviceConfig"}`, string(device.Opaque.Parameters))
}

func TestParseDeviceGrouping_RejectsOpaqueConfigOutsidePassthrough(t *testing.T) {
	data := validGroupingData("container-with-opaque")
	data["devices"] = `
- class: gpu.amd.com
  count: 1
  opaque:
    driver: gpu.resource.amd.com
    parameters:
      apiVersion: gpu.resource.amd.com/v1alpha1
      kind: VfioDeviceConfig
`

	_, err := parseDeviceGrouping(makeGroupingConfigMap("container-with-opaque", data))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires passthrough")
}

func TestParseDeviceGrouping_RejectsDuplicateTopologyClass(t *testing.T) {
	data := validGroupingData("duplicate-class")
	data["devices"] = "- class: gpu.amd.com\n  count: 1\n- class: gpu.amd.com\n  requestClass: vfio.gpu.amd.com\n  count: 1"

	_, err := parseDeviceGrouping(makeGroupingConfigMap("duplicate-class", data))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate class")
}

func TestParseDeviceGrouping_RejectsDuplicateRequestClass(t *testing.T) {
	data := validGroupingData("duplicate-request-class")
	data["devices"] = "- class: gpu.amd.com\n  requestClass: vfio.shared\n  count: 1\n- class: sriov.example.com\n  requestClass: vfio.shared\n  count: 1"

	_, err := parseDeviceGrouping(makeGroupingConfigMap("duplicate-request-class", data))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate requestClass")
}

func TestParseDeviceGrouping_RailOffering(t *testing.T) {
	data := validGroupingData("gpu-nic-pair")
	data["rail"] = "true"
	data["devices"] = "- class: gpu.example.com\n  count: 1\n- class: rdma.example.com\n  count: 1"

	grouping, err := parseDeviceGrouping(makeGroupingConfigMap("gpu-nic-pair", data))
	require.NoError(t, err)
	assert.True(t, grouping.Rail)
}

func TestParseDeviceGrouping_RailValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(map[string]string)
		errText string
	}{
		{
			name:    "invalid boolean",
			mutate:  func(data map[string]string) { data["rail"] = "yes" },
			errText: "rail",
		},
		{
			name: "wrong alignment",
			mutate: func(data map[string]string) {
				data["rail"] = "true"
				data["alignment"] = "numaNode"
			},
			errText: "alignment",
		},
		{
			name: "fallback is not allowed",
			mutate: func(data map[string]string) {
				data["rail"] = "true"
				data["fallback"] = "numaNode"
				data["devices"] = "- class: gpu.example.com\n  count: 1\n- class: rdma.example.com\n  count: 1"
			},
			errText: "fallback",
		},
		{
			name: "wrong device count",
			mutate: func(data map[string]string) {
				data["rail"] = "true"
				data["devices"] = "- class: gpu.example.com\n  count: 1"
			},
			errText: "exactly two",
		},
		{
			name: "device count is not one",
			mutate: func(data map[string]string) {
				data["rail"] = "true"
				data["devices"] = "- class: gpu.example.com\n  count: 2\n- class: rdma.example.com\n  count: 1"
			},
			errText: "count 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := validGroupingData("gpu-nic-pair")
			tt.mutate(data)
			err := NewGroupingStore().LoadFromConfigMap(makeGroupingConfigMap("gpu-nic-pair", data))
			if assert.Error(t, err) {
				assert.ErrorContains(t, err, tt.errText)
			}
		})
	}
}

func TestParseDeviceGrouping_RequestDriver(t *testing.T) {
	const opaque = "\n  opaque:\n    driver: %s\n    parameters:\n      kind: Vfio"
	tests := []struct {
		name    string
		devices string
		wantErr string
	}{
		{"valid", "- class: gpu.example.com\n  count: 1\n  requestClass: vfio.gpu.example.com\n  requestDriver: vfio.example.com", ""},
		{"requires requestClass", "- class: gpu.example.com\n  count: 1\n  requestDriver: vfio.example.com", "requestDriver requires requestClass"},
		{"invalid name", "- class: gpu.example.com\n  count: 1\n  requestClass: v\n  requestDriver: Not_A_Driver", "not a valid driver name"},
		{"too long", "- class: gpu.example.com\n  count: 1\n  requestClass: v\n  requestDriver: " + strings.Repeat("a", 64), "not a valid driver name"},
		{"passthrough without opaque", "- class: gpu.example.com\n  count: 1\n  passthrough: true\n  requestClass: v\n  requestDriver: vfio.example.com", ""},
		{"opaque driver mismatch", "- class: gpu.example.com\n  count: 1\n  passthrough: true\n  requestClass: v\n  requestDriver: vfio.example.com" + fmt.Sprintf(opaque, "other.example.com"), "must match requestDriver"},
		{"opaque driver matches", "- class: gpu.example.com\n  count: 1\n  passthrough: true\n  requestClass: v\n  requestDriver: vfio.example.com" + fmt.Sprintf(opaque, "vfio.example.com"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := validGroupingData("gpu-vfio")
			data["devices"] = tt.devices
			if strings.Contains(tt.devices, "passthrough: true") {
				data["mode"] = "passthrough"
			}
			grouping, err := parseDeviceGrouping(makeGroupingConfigMap("gpu-vfio", data))
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "vfio.example.com", grouping.Devices[0].RequestDriver)
		})
	}
}

func TestGroupingStore_GetGroupingsCopiesRequestDriver(t *testing.T) {
	data := validGroupingData("gpu-vfio")
	data["devices"] = "- class: gpu.example.com\n  count: 1\n  requestClass: v\n  requestDriver: vfio.example.com"
	store := NewGroupingStore()
	require.NoError(t, store.LoadFromConfigMap(makeGroupingConfigMap("gpu-vfio", data)))
	groupings := store.GetGroupings()
	require.Len(t, groupings, 1)
	assert.Equal(t, "vfio.example.com", groupings[0].Devices[0].RequestDriver)
}

func TestParseDeviceGrouping_RejectsUnknownDeviceKeys(t *testing.T) {
	tests := map[string]string{
		"typo":      "- class: gpu.example.com\n  count: 1\n  passthru: true",
		"duplicate": "- class: gpu.example.com\n  count: 1\n  count: 2",
	}
	for name, devices := range tests {
		t.Run(name, func(t *testing.T) {
			data := validGroupingData("gpu-vfio")
			data["devices"] = devices
			_, err := parseDeviceGrouping(makeGroupingConfigMap("gpu-vfio", data))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "failed to parse devices YAML")
		})
	}

	data := validGroupingData("gpu-vfio")
	data["devices"] = "- class: gpu.example.com\n  count: 1\n  passthru: true"
	_, err := parseDeviceGrouping(makeGroupingConfigMap("gpu-vfio", data))
	assert.Contains(t, err.Error(), "passthru")
}

func TestParseDeviceGrouping_UnknownTopLevelKeyIsIgnored(t *testing.T) {
	data := validGroupingData("gpu-vfio")
	data["rial"] = "true"
	grouping, err := parseDeviceGrouping(makeGroupingConfigMap("gpu-vfio", data))
	require.NoError(t, err)
	assert.False(t, grouping.Rail)
}

func TestParseDeviceGrouping_InvalidMode(t *testing.T) {
	data := validGroupingData("test")
	data["mode"] = "vm"

	err := NewGroupingStore().LoadFromConfigMap(makeGroupingConfigMap("test", data))
	assert.ErrorContains(t, err, "invalid mode")
}

func TestParseDeviceGrouping_InvalidIntentMetadata(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value string
	}{
		{name: "intent level", field: "intentLevel", value: "unknown"},
		{name: "visibility", field: "visibility", value: "private"},
		{name: "display name too long", field: "displayName", value: strings.Repeat("x", 64)},
		{name: "description too long", field: "description", value: strings.Repeat("x", 1025)},
		{name: "too many categories", field: "categories", value: uniqueList(1, 17)},
		{name: "category too long", field: "categories", value: strings.Repeat("x", 64)},
		{name: "unicode category too long", field: "categories", value: strings.Repeat("é", 64)},
		{name: "too many workload types", field: "workloadTypes", value: uniqueList(1, 17)},
		{name: "workload type too long", field: "workloadTypes", value: strings.Repeat("x", 64)},
		{name: "unicode workload type too long", field: "workloadTypes", value: strings.Repeat("界", 64)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := validGroupingData("test")
			data[tt.field] = tt.value
			err := NewGroupingStore().LoadFromConfigMap(makeGroupingConfigMap("test", data))
			if assert.Error(t, err) {
				assert.ErrorContains(t, err, "default/test")
				assert.ErrorContains(t, err, tt.field)
			}
		})
	}
}

func TestParseDeviceGrouping_SanitizedNameValidation(t *testing.T) {
	tests := []struct {
		name  string
		valid bool
	}{
		{name: "1-gpu", valid: true},
		{name: "gpu.nvidia.com", valid: true},
		{name: "gpu--nic", valid: true},
		{name: "gpu_inference"},
		{name: "-gpu"},
		{name: "gpu-"},
		{name: strings.Repeat("g", 57)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := validGroupingData(tt.name)
			err := NewGroupingStore().LoadFromConfigMap(makeGroupingConfigMap("test", data))
			if tt.valid {
				assert.NoError(t, err)
			} else if assert.Error(t, err) {
				assert.ErrorContains(t, err, "default/test")
				assert.ErrorContains(t, err, "name")
			}
		})
	}
}

func TestGroupingStore_GetGroupingsDeepCopiesIntent(t *testing.T) {
	store := NewGroupingStore()
	data := validGroupingData("test")
	data["categories"] = "gpu,inference"
	data["workloadTypes"] = "training,serving"
	require.NoError(t, store.LoadFromConfigMap(makeGroupingConfigMap("test", data)))

	first := store.GetGroupings()
	require.Len(t, first, 1)
	require.NotEmpty(t, first[0].Intent.Categories)
	require.NotEmpty(t, first[0].Intent.WorkloadTypes)
	first[0].Intent.Categories[0] = "mutated"
	first[0].Intent.WorkloadTypes[0] = "mutated"

	second := store.GetGroupings()
	require.Len(t, second, 1)
	assert.Equal(t, []string{"gpu", "inference"}, second[0].Intent.Categories)
	assert.Equal(t, []string{"training", "serving"}, second[0].Intent.WorkloadTypes)
}

func TestGroupingStore_GetGroupingsPreservesRail(t *testing.T) {
	store := NewGroupingStore()
	data := validGroupingData("gpu-nic-pair")
	data["rail"] = "true"
	data["devices"] = "- class: gpu.example.com\n  count: 1\n- class: rdma.example.com\n  count: 1"
	require.NoError(t, store.LoadFromConfigMap(makeGroupingConfigMap("gpu-nic-pair", data)))

	groupings := store.GetGroupings()
	require.Len(t, groupings, 1)
	assert.True(t, groupings[0].Rail)
}

func TestGroupingStore_LoadFromConfigMap(t *testing.T) {
	store := NewGroupingStore()

	cm := makeGroupingConfigMap("gpu-dpu-pair", map[string]string{
		"name":      "gpu-dpu-pair",
		"alignment": "pcieRoot",
		"fallback":  "numaNode",
		"devices": `- class: gpu.amd.com
  count: 1
- class: dpu.pensando.com
  count: 1`,
	})

	err := store.LoadFromConfigMap(cm)
	require.NoError(t, err)

	groupings := store.GetGroupings()
	require.Len(t, groupings, 1)

	g := groupings[0]
	assert.Equal(t, "gpu-dpu-pair", g.Name)
	assert.Equal(t, "pcieRoot", g.Alignment)
	assert.Equal(t, "numaNode", g.Fallback)
	require.Len(t, g.Devices, 2)
	assert.Equal(t, "gpu.amd.com", g.Devices[0].Class)
	assert.Equal(t, 1, g.Devices[0].Count)
	assert.Equal(t, "dpu.pensando.com", g.Devices[1].Class)
	assert.Equal(t, 1, g.Devices[1].Count)
}

func TestGroupingStore_LoadFromConfigMap_NoFallback(t *testing.T) {
	store := NewGroupingStore()

	cm := makeGroupingConfigMap("gpu-only", map[string]string{
		"name":      "gpu-only",
		"alignment": "numaNode",
		"devices": `- class: gpu.amd.com
  count: 2`,
	})

	err := store.LoadFromConfigMap(cm)
	require.NoError(t, err)

	groupings := store.GetGroupings()
	require.Len(t, groupings, 1)
	assert.Equal(t, "", groupings[0].Fallback)
}

func TestGroupingStore_LoadFromConfigMap_WithCapacity(t *testing.T) {
	store := NewGroupingStore()

	cm := makeGroupingConfigMap("gpu-cpu", map[string]string{
		"name":      "gpu-cpu",
		"alignment": "numaNode",
		"devices": `- class: gpu.amd.com
  count: 1
- class: dra.cpu
  count: 1
  capacity:
    dra.cpu/cpu: "16"`,
	})

	err := store.LoadFromConfigMap(cm)
	require.NoError(t, err)

	groupings := store.GetGroupings()
	require.Len(t, groupings, 1)
	require.Len(t, groupings[0].Devices, 2)
	assert.Equal(t, map[string]string{"dra.cpu/cpu": "16"}, groupings[0].Devices[1].Capacity)
}

func TestGroupingStore_LoadFromConfigMap_Errors(t *testing.T) {
	tests := []struct {
		name string
		data map[string]string
	}{
		{"missing name", map[string]string{"alignment": "pcieRoot", "devices": "- class: x\n  count: 1"}},
		{"missing alignment", map[string]string{"name": "test", "devices": "- class: x\n  count: 1"}},
		{"invalid alignment", map[string]string{"name": "test", "alignment": "invalid", "devices": "- class: x\n  count: 1"}},
		{"invalid fallback", map[string]string{"name": "test", "alignment": "pcieRoot", "fallback": "invalid", "devices": "- class: x\n  count: 1"}},
		{"missing devices", map[string]string{"name": "test", "alignment": "pcieRoot"}},
		{"empty devices", map[string]string{"name": "test", "alignment": "pcieRoot", "devices": "[]"}},
		{"device missing class", map[string]string{"name": "test", "alignment": "pcieRoot", "devices": "- count: 1"}},
		{"device zero count", map[string]string{"name": "test", "alignment": "pcieRoot", "devices": "- class: x\n  count: 0"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewGroupingStore()
			cm := makeGroupingConfigMap("test", tt.data)
			err := store.LoadFromConfigMap(cm)
			assert.Error(t, err)
		})
	}
}

func TestGroupingStore_RemoveConfigMap(t *testing.T) {
	store := NewGroupingStore()

	cm := makeGroupingConfigMap("test", map[string]string{
		"name":      "test",
		"alignment": "pcieRoot",
		"devices":   "- class: gpu.amd.com\n  count: 1",
	})

	require.NoError(t, store.LoadFromConfigMap(cm))
	require.Len(t, store.GetGroupings(), 1)

	store.RemoveConfigMap("default", "test")
	assert.Len(t, store.GetGroupings(), 0)
}

// --- GroupingBuilder tests ---

func makeTestDevice(driver, name string, numa *int64, pcieRoot *string) TopologyDevice {
	return TopologyDevice{
		DriverName:         driver,
		DeviceName:         name,
		NUMANode:           numa,
		PCIeRoot:           pcieRoot,
		ExtendedAttributes: make(map[string]DeviceAttributeValue),
	}
}

func int64Ptr(v int64) *int64    { return &v }
func stringPtr(v string) *string { return &v }

func TestGroupingBuilder_BasicPCIeRootAlignment(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()

	// Simulate 2 PCIe roots, each with 1 GPU + 1 DPU
	nodeTopo := &NodeTopology{
		NodeName:        "node-1",
		DevicesByDriver: make(map[string][]TopologyDevice),
	}
	nodeTopo.DevicesByDriver["gpu.amd.com"] = []TopologyDevice{
		makeTestDevice("gpu.amd.com", "gpu-0", int64Ptr(0), stringPtr("pcie-0")),
		makeTestDevice("gpu.amd.com", "gpu-1", int64Ptr(0), stringPtr("pcie-1")),
	}
	nodeTopo.DevicesByDriver["dpu.pensando.com"] = []TopologyDevice{
		makeTestDevice("dpu.pensando.com", "dpu-0", int64Ptr(0), stringPtr("pcie-0")),
		makeTestDevice("dpu.pensando.com", "dpu-1", int64Ptr(0), stringPtr("pcie-1")),
	}

	model.mu.Lock()
	model.nodes = map[string]*NodeTopology{"node-1": nodeTopo}
	model.mu.Unlock()

	builder := NewGroupingBuilder(model, rules)

	grouping := DeviceGrouping{
		Name:      "gpu-dpu-pair",
		Alignment: "pcieRoot",
		Devices: []GroupingDevice{
			{Class: "gpu.amd.com", Count: 1},
			{Class: "dpu.pensando.com", Count: 1},
		},
	}

	results := builder.BuildGroupings([]DeviceGrouping{grouping})
	require.Len(t, results, 1)

	instances := results[0].Instances
	assert.Len(t, instances, 2)

	for _, inst := range instances {
		assert.Equal(t, "gpu-dpu-pair", inst.GroupingName)
		assert.Equal(t, "pcieRoot", inst.Alignment)
		assert.Equal(t, 1, inst.DeviceCounts["gpu.amd.com"])
		assert.Equal(t, 1, inst.DeviceCounts["dpu.pensando.com"])
	}
}

func TestGroupingBuilder_BuildGroupingsCarriesRequestContract(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()
	nodeTopo := &NodeTopology{
		NodeName:        "node-1",
		DevicesByDriver: map[string][]TopologyDevice{},
	}
	nodeTopo.DevicesByDriver["gpu.amd.com"] = []TopologyDevice{
		makeTestDevice("gpu.amd.com", "gpu-0", int64Ptr(0), stringPtr("pcie-0")),
	}
	nodeTopo.DevicesByDriver["dpu.pensando.com"] = []TopologyDevice{
		makeTestDevice("dpu.pensando.com", "dpu-0", int64Ptr(0), stringPtr("pcie-0")),
	}
	model.mu.Lock()
	model.nodes = map[string]*NodeTopology{"node-1": nodeTopo}
	model.mu.Unlock()

	opaque := &OpaqueConfig{Driver: "gpu.amd.com", Parameters: []byte(`{"kind":"Vfio"}`)}
	grouping := DeviceGrouping{
		Name:      "gpu-dpu-vfio",
		Alignment: "pcieRoot",
		Mode:      OfferingModePassthrough,
		Devices: []GroupingDevice{
			{
				Class: "gpu.amd.com", Count: 1, Passthrough: true,
				RequestClass: "vfio.gpu.amd.com", RequestDriver: "vfio.amd.com",
				Selectors: []string{`device.attributes["gpu.amd.com"].isVF == true`},
				Opaque:    opaque,
			},
			{Class: "dpu.pensando.com", Count: 1},
		},
	}

	results := NewGroupingBuilder(model, rules).BuildGroupings([]DeviceGrouping{grouping})
	require.Len(t, results, 1)
	require.Len(t, results[0].Instances, 1)
	inst := results[0].Instances[0]

	assert.Equal(t, RequestContract{
		Class:       "vfio.gpu.amd.com",
		Driver:      "vfio.amd.com",
		Selectors:   []string{`device.attributes["gpu.amd.com"].isVF == true`},
		Opaque:      opaque,
		Passthrough: true,
	}, inst.Requests["gpu.amd.com"])
	// A device without an override requests its own class.
	assert.Equal(t, RequestContract{Class: "dpu.pensando.com"}, inst.Requests["dpu.pensando.com"])
	assert.Equal(t, "dpu.pensando.com", inst.requestClass("dpu.pensando.com"))

	// The instance holds copies, not the grouping's own slices.
	assert.NotSame(t, opaque, inst.Requests["gpu.amd.com"].Opaque)
	opaque.Parameters[0] = 'X'
	assert.Equal(t, byte('{'), inst.Requests["gpu.amd.com"].Opaque.Parameters[0])
}

func TestGroupingBuilder_BuildGroupingsCarriesIntentMetadata(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()
	nodeTopo := &NodeTopology{
		NodeName: "node-1",
		DevicesByDriver: map[string][]TopologyDevice{"gpu.amd.com": {
			makeTestDevice("gpu.amd.com", "gpu-0", int64Ptr(0), stringPtr("pcie-0")),
		}},
	}
	model.mu.Lock()
	model.nodes = map[string]*NodeTopology{"node-1": nodeTopo}
	model.mu.Unlock()

	builder := NewGroupingBuilder(model, rules)
	intent := IntentMetadata{
		DisplayName: "GPU for AI",
		Categories:  []string{"ai", "inference"},
		Visibility:  "public",
	}
	grouping := DeviceGrouping{
		Name:      "gpu-inference",
		Alignment: "pcieRoot",
		Rail:      true,
		Devices:   []GroupingDevice{{Class: "gpu.amd.com", Count: 1}},
		Intent:    intent,
	}

	results := builder.BuildGroupings([]DeviceGrouping{grouping})
	require.Len(t, results, 1)
	require.Len(t, results[0].Instances, 1)
	assert.Equal(t, intent, results[0].Instances[0].Intent)
	assert.True(t, results[0].Instances[0].Rail)
}

func TestGroupingBuilder_UnsatisfiableGrouping(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()

	// Node has GPUs but no DPUs
	nodeTopo := &NodeTopology{
		NodeName:        "node-1",
		DevicesByDriver: make(map[string][]TopologyDevice),
	}
	nodeTopo.DevicesByDriver["gpu.amd.com"] = []TopologyDevice{
		makeTestDevice("gpu.amd.com", "gpu-0", int64Ptr(0), stringPtr("pcie-0")),
	}

	model.mu.Lock()
	model.nodes = map[string]*NodeTopology{"node-1": nodeTopo}
	model.mu.Unlock()

	builder := NewGroupingBuilder(model, rules)

	grouping := DeviceGrouping{
		Name:      "gpu-dpu-pair",
		Alignment: "pcieRoot",
		Devices: []GroupingDevice{
			{Class: "gpu.amd.com", Count: 1},
			{Class: "dpu.pensando.com", Count: 1},
		},
	}

	results := builder.BuildGroupings([]DeviceGrouping{grouping})
	// No instances because DPU class doesn't exist on node
	totalInstances := 0
	for _, r := range results {
		totalInstances += len(r.Instances)
	}
	assert.Equal(t, 0, totalInstances)
}

func TestGroupingBuilder_AsymmetricDevices(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()

	// SMC6217-like: 3 PCIe roots, only 2 have NVMe
	nodeTopo := &NodeTopology{
		NodeName:        "node-1",
		DevicesByDriver: make(map[string][]TopologyDevice),
	}
	nodeTopo.DevicesByDriver["gpu.amd.com"] = []TopologyDevice{
		makeTestDevice("gpu.amd.com", "gpu-0", int64Ptr(0), stringPtr("pcie-0")),
		makeTestDevice("gpu.amd.com", "gpu-1", int64Ptr(0), stringPtr("pcie-1")),
		makeTestDevice("gpu.amd.com", "gpu-2", int64Ptr(0), stringPtr("pcie-2")),
	}
	nodeTopo.DevicesByDriver["nvme.kioxia.com"] = []TopologyDevice{
		makeTestDevice("nvme.kioxia.com", "nvme-0", int64Ptr(0), stringPtr("pcie-0")),
		makeTestDevice("nvme.kioxia.com", "nvme-1", int64Ptr(0), stringPtr("pcie-1")),
		// pcie-2 has no NVMe
	}

	model.mu.Lock()
	model.nodes = map[string]*NodeTopology{"node-1": nodeTopo}
	model.mu.Unlock()

	builder := NewGroupingBuilder(model, rules)

	grouping := DeviceGrouping{
		Name:      "gpu-nvme",
		Alignment: "pcieRoot",
		Devices: []GroupingDevice{
			{Class: "gpu.amd.com", Count: 1},
			{Class: "nvme.kioxia.com", Count: 1},
		},
	}

	results := builder.BuildGroupings([]DeviceGrouping{grouping})
	require.Len(t, results, 1)

	// Only 2 of 3 PCIe roots have both GPU + NVMe
	assert.Len(t, results[0].Instances, 2)
}

func TestGroupingBuilder_FallbackAlignment(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()

	// 2 NUMA nodes, 4 PCIe roots
	// NIC only on pcie-0 (NUMA 0). Other GPUs have no co-located NIC.
	// At numaNode level, NUMA 0 has 2 GPUs + 1 NIC → 1 pair possible at numaNode.
	nodeTopo := &NodeTopology{
		NodeName:        "node-1",
		DevicesByDriver: make(map[string][]TopologyDevice),
	}
	nodeTopo.DevicesByDriver["gpu.amd.com"] = []TopologyDevice{
		makeTestDevice("gpu.amd.com", "gpu-0", int64Ptr(0), stringPtr("pcie-0")),
		makeTestDevice("gpu.amd.com", "gpu-1", int64Ptr(0), stringPtr("pcie-1")),
		makeTestDevice("gpu.amd.com", "gpu-2", int64Ptr(1), stringPtr("pcie-2")),
		makeTestDevice("gpu.amd.com", "gpu-3", int64Ptr(1), stringPtr("pcie-3")),
	}
	nodeTopo.DevicesByDriver["nic.mellanox.com"] = []TopologyDevice{
		makeTestDevice("nic.mellanox.com", "nic-0", int64Ptr(0), stringPtr("pcie-0")),
		makeTestDevice("nic.mellanox.com", "nic-1", int64Ptr(1), stringPtr("pcie-4")),
	}

	model.mu.Lock()
	model.nodes = map[string]*NodeTopology{"node-1": nodeTopo}
	model.mu.Unlock()

	builder := NewGroupingBuilder(model, rules)

	grouping := DeviceGrouping{
		Name:      "gpu-nic",
		Alignment: "pcieRoot",
		Fallback:  "numaNode",
		Devices: []GroupingDevice{
			{Class: "gpu.amd.com", Count: 1},
			{Class: "nic.mellanox.com", Count: 1},
		},
	}

	results := builder.BuildGroupings([]DeviceGrouping{grouping})
	require.Len(t, results, 1)

	instances := results[0].Instances

	// Count by alignment level
	pcieRootCount := 0
	numaNodeCount := 0
	for _, inst := range instances {
		switch inst.Alignment {
		case "pcieRoot":
			pcieRootCount++
		case "numaNode":
			numaNodeCount++
		}
	}

	// pcie-0 has GPU+NIC → 1 pcieRoot instance
	assert.Equal(t, 1, pcieRootCount, "should have 1 pcieRoot instance (pcie-0)")
	// Remaining: gpu-1 (NUMA 0) can pair with nic-1? No, nic-0 was used.
	// nic-1 is on NUMA 1, pcie-4 — not consumed at pcieRoot level.
	// At numaNode fallback, NUMA 1 has gpu-2, gpu-3, nic-1 → 1 numaNode pair.
	assert.Equal(t, 1, numaNodeCount, "should have 1 numaNode fallback instance (NUMA 1)")
}

func TestGroupingBuilder_MultipleGroupings(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()

	nodeTopo := &NodeTopology{
		NodeName:        "node-1",
		DevicesByDriver: make(map[string][]TopologyDevice),
	}
	nodeTopo.DevicesByDriver["gpu.amd.com"] = []TopologyDevice{
		makeTestDevice("gpu.amd.com", "gpu-0", int64Ptr(0), stringPtr("pcie-0")),
	}
	nodeTopo.DevicesByDriver["dpu.pensando.com"] = []TopologyDevice{
		makeTestDevice("dpu.pensando.com", "dpu-0", int64Ptr(0), stringPtr("pcie-0")),
	}
	nodeTopo.DevicesByDriver["nvme.kioxia.com"] = []TopologyDevice{
		makeTestDevice("nvme.kioxia.com", "nvme-0", int64Ptr(0), stringPtr("pcie-0")),
	}

	model.mu.Lock()
	model.nodes = map[string]*NodeTopology{"node-1": nodeTopo}
	model.mu.Unlock()

	builder := NewGroupingBuilder(model, rules)

	groupings := []DeviceGrouping{
		{
			Name:      "gpu-dpu",
			Alignment: "pcieRoot",
			Devices: []GroupingDevice{
				{Class: "gpu.amd.com", Count: 1},
				{Class: "dpu.pensando.com", Count: 1},
			},
		},
		{
			Name:      "gpu-dpu-nvme",
			Alignment: "pcieRoot",
			Devices: []GroupingDevice{
				{Class: "gpu.amd.com", Count: 1},
				{Class: "dpu.pensando.com", Count: 1},
				{Class: "nvme.kioxia.com", Count: 1},
			},
		},
	}

	results := builder.BuildGroupings(groupings)
	require.Len(t, results, 1)

	// Both groupings should be satisfiable
	nameCount := make(map[string]int)
	for _, inst := range results[0].Instances {
		nameCount[inst.GroupingName]++
	}
	assert.Equal(t, 1, nameCount["gpu-dpu"])
	assert.Equal(t, 1, nameCount["gpu-dpu-nvme"])
}

func TestGroupingBuilder_MultipleInstancesUniqueDevices(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()

	// 1 NUMA node with 4 GPUs and 4 DPUs — should yield 4 instances,
	// each with unique devices.
	nodeTopo := &NodeTopology{
		NodeName:        "node-1",
		DevicesByDriver: make(map[string][]TopologyDevice),
	}
	nodeTopo.DevicesByDriver["gpu.amd.com"] = []TopologyDevice{
		makeTestDevice("gpu.amd.com", "gpu-0", int64Ptr(0), stringPtr("pcie-0")),
		makeTestDevice("gpu.amd.com", "gpu-1", int64Ptr(0), stringPtr("pcie-1")),
		makeTestDevice("gpu.amd.com", "gpu-2", int64Ptr(0), stringPtr("pcie-2")),
		makeTestDevice("gpu.amd.com", "gpu-3", int64Ptr(0), stringPtr("pcie-3")),
	}
	nodeTopo.DevicesByDriver["dpu.pensando.com"] = []TopologyDevice{
		makeTestDevice("dpu.pensando.com", "dpu-0", int64Ptr(0), stringPtr("pcie-0")),
		makeTestDevice("dpu.pensando.com", "dpu-1", int64Ptr(0), stringPtr("pcie-1")),
		makeTestDevice("dpu.pensando.com", "dpu-2", int64Ptr(0), stringPtr("pcie-2")),
		makeTestDevice("dpu.pensando.com", "dpu-3", int64Ptr(0), stringPtr("pcie-3")),
	}

	model.mu.Lock()
	model.nodes = map[string]*NodeTopology{"node-1": nodeTopo}
	model.mu.Unlock()

	builder := NewGroupingBuilder(model, rules)

	grouping := DeviceGrouping{
		Name:      "gpu-dpu-pair",
		Alignment: "numaNode",
		Devices: []GroupingDevice{
			{Class: "gpu.amd.com", Count: 1},
			{Class: "dpu.pensando.com", Count: 1},
		},
	}

	results := builder.BuildGroupings([]DeviceGrouping{grouping})
	require.Len(t, results, 1)
	instances := results[0].Instances

	// 4 GPUs / 1 per instance = 4 instances
	require.Len(t, instances, 4)

	// Each instance should have exactly 2 devices (1 GPU + 1 DPU)
	allDeviceNames := make(map[string]bool)
	for _, inst := range instances {
		assert.Len(t, inst.Devices, 2, "each instance should have exactly 2 devices")
		assert.Equal(t, 1, inst.DeviceCounts["gpu.amd.com"])
		assert.Equal(t, 1, inst.DeviceCounts["dpu.pensando.com"])

		for _, d := range inst.Devices {
			key := d.DriverName + "/" + d.DeviceName
			assert.False(t, allDeviceNames[key], "device %s should not appear in multiple instances", key)
			allDeviceNames[key] = true
		}
	}
	assert.Len(t, allDeviceNames, 8, "all 8 devices should be allocated")
}

func TestSanitizeForName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"gpu.nvidia.com", "gpu-nvidia-com"},
		{"resource.kubernetes.io/pcieRoot", "resource-kubernetes-io-pcieroot"},
		{"simple", "simple"},
		{"UPPER", "upper"},
		{"host:port", "host-port"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.want, sanitizeForName(tt.input))
		})
	}
}

func TestGroupingBuilder_FallbackFromPCIeRootToNUMA(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()

	// GPU on pcie-0, NIC on pcie-1 — pcieRoot unsatisfiable, but both on NUMA 0
	nodeTopo := &NodeTopology{
		NodeName:        "node-1",
		DevicesByDriver: make(map[string][]TopologyDevice),
	}
	nodeTopo.DevicesByDriver["gpu-slice"] = []TopologyDevice{
		makeTestDevice("gpu.amd.com", "gpu-0", int64Ptr(0), stringPtr("pcie-0")),
	}
	nodeTopo.DevicesByDriver["nic-slice"] = []TopologyDevice{
		makeTestDevice("rdma.mellanox.com", "nic-0", int64Ptr(0), stringPtr("pcie-1")),
	}

	model.mu.Lock()
	model.nodes = map[string]*NodeTopology{"node-1": nodeTopo}
	model.mu.Unlock()

	builder := NewGroupingBuilder(model, rules)

	grouping := DeviceGrouping{
		Name:      "gpu-nic-pair",
		Alignment: "pcieRoot",
		Fallback:  "numaNode",
		Devices: []GroupingDevice{
			{Class: "gpu.amd.com", Count: 1},
			{Class: "rdma.mellanox.com", Count: 1},
		},
	}

	results := builder.BuildGroupings([]DeviceGrouping{grouping})
	require.Len(t, results, 1)

	// pcieRoot should fail (different roots), numaNode fallback should succeed
	hasNUMAInstance := false
	for _, inst := range results[0].Instances {
		if inst.Alignment == "numaNode" {
			hasNUMAInstance = true
		}
	}
	assert.True(t, hasNUMAInstance, "should have a numaNode fallback instance")
}

func TestGroupingBuilder_InstanceIndexDeterministic(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()

	nodeTopo := &NodeTopology{
		NodeName:        "node-1",
		DevicesByDriver: make(map[string][]TopologyDevice),
	}
	nodeTopo.DevicesByDriver["gpu-slice"] = []TopologyDevice{
		makeTestDevice("gpu.amd.com", "gpu-0", int64Ptr(0), stringPtr("pcie-0")),
		makeTestDevice("gpu.amd.com", "gpu-1", int64Ptr(0), stringPtr("pcie-1")),
	}
	nodeTopo.DevicesByDriver["nic-slice"] = []TopologyDevice{
		makeTestDevice("rdma.mellanox.com", "nic-0", int64Ptr(0), stringPtr("pcie-0")),
		makeTestDevice("rdma.mellanox.com", "nic-1", int64Ptr(0), stringPtr("pcie-1")),
	}

	model.mu.Lock()
	model.nodes = map[string]*NodeTopology{"node-1": nodeTopo}
	model.mu.Unlock()

	builder := NewGroupingBuilder(model, rules)

	grouping := DeviceGrouping{
		Name:      "gpu-nic-pair",
		Alignment: "pcieRoot",
		Rail:      true,
		Devices: []GroupingDevice{
			{Class: "gpu.amd.com", Count: 1},
			{Class: "rdma.mellanox.com", Count: 1},
		},
	}

	// Run twice — instance indexes should be identical
	results1 := builder.BuildGroupings([]DeviceGrouping{grouping})
	results2 := builder.BuildGroupings([]DeviceGrouping{grouping})

	require.Len(t, results1, 1)
	require.Len(t, results2, 1)
	require.Len(t, results1[0].Instances, 2)
	require.Len(t, results2[0].Instances, 2)

	for i := range results1[0].Instances {
		assert.Equal(t, results1[0].Instances[i].InstanceIndex, results2[0].Instances[i].InstanceIndex,
			"instance index should be deterministic across calls")
		assert.True(t, results1[0].Instances[i].Rail)
	}
}

func TestParseDeviceGrouping_OfferingFlag(t *testing.T) {
	tests := map[string]struct {
		value   string
		want    bool
		wantErr string
	}{
		"absent": {value: "", want: false},
		"true":   {value: "true", want: true},
		"false":  {value: "false", want: false},
		"bad":    {value: "yes please", wantErr: `invalid offering "yes please"`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			data := validGroupingData("test")
			if tt.value != "" {
				data["offering"] = tt.value
			}
			grouping, err := parseDeviceGrouping(makeGroupingConfigMap("test", data))
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, grouping.Intent.Offering)
		})
	}
}

func TestParseDeviceGrouping_OfferingFlagIsNotInferredFromMetadata(t *testing.T) {
	data := validGroupingData("test")
	data["displayName"] = "GPU inference"
	grouping, err := parseDeviceGrouping(makeGroupingConfigMap("test", data))
	require.NoError(t, err)
	assert.False(t, grouping.Intent.Offering)
}
