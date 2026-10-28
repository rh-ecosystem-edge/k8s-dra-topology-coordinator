package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/fake"
)

// findSpecificDC returns the first DeviceClass with a NUMA or coupling label (non-aggregate).
func findSpecificDC(items []resourcev1.DeviceClass) *resourcev1.DeviceClass {
	for i := range items {
		if items[i].Labels[CoordinatorDriverName+"/numa"] != "" ||
			items[i].Labels[CoordinatorDriverName+"/coupling"] != "" {
			return &items[i]
		}
	}
	if len(items) > 0 {
		return &items[0]
	}
	return nil
}

func TestGroupingDeviceClassName_UsesRailOnlyWhenExplicit(t *testing.T) {
	rail := GroupingInstance{
		GroupingName:  "gpu-nic-pair",
		Alignment:     "pcieRoot",
		Rail:          true,
		InstanceIndex: 1,
	}
	assert.Equal(t, "gpu-nic-pair-rail1", groupingDeviceClassName(rail))

	numa := GroupingInstance{
		GroupingName:  "inference-cpu-memory",
		Alignment:     "numaNode",
		NUMANodes:     []int64{1},
		InstanceIndex: 1,
	}
	assert.Equal(t, "inference-cpu-memory-numa1", groupingDeviceClassName(numa))
}

func TestGroupingDeviceClassName_UsesTopologySpecificSuffixes(t *testing.T) {
	pcie := GroupingInstance{
		GroupingName:  "gpu-nic-pair",
		Alignment:     "pcieRoot",
		PCIeRoots:     []string{"pci0000:00"},
		InstanceIndex: 0,
	}
	socket := GroupingInstance{
		GroupingName:  "cpu-memory",
		Alignment:     "socket",
		Sockets:       []int64{2},
		InstanceIndex: 0,
	}
	assert.Equal(t, "gpu-nic-pair-pcie-pci0000-00", groupingDeviceClassName(pcie))
	assert.Equal(t, "cpu-memory-socket2", groupingDeviceClassName(socket))
}

func TestGroupingDeviceClassName_IsDeterministicAndDNSBounded(t *testing.T) {
	instance := GroupingInstance{
		GroupingName:  strings.Repeat("long-grouping-", 5),
		Alignment:     "pcieRoot",
		PCIeRoots:     []string{strings.Repeat("pci-root-", 10)},
		InstanceIndex: 7,
	}
	first := groupingDeviceClassName(instance)
	second := groupingDeviceClassName(instance)

	assert.Equal(t, first, second)
	assert.LessOrEqual(t, len(first), 63)
	assert.NotEmpty(t, first)
	assert.Empty(t, validation.IsDNS1123Label(first))
}

func TestDeviceClassManager_SyncDeviceClasses(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	results := []PartitionResult{
		{
			NodeName: "node-1",
			Profile:  "gpu-nvidia-com-8_rdma-mellanox-com-8",
			Partitions: []PartitionDevice{
				{
					Name:    "node-1-pcieroot-0",
					Type:    PartitionPCIeRoot,
					Profile: "gpu-nvidia-com-8_rdma-mellanox-com-8",
					DeviceCounts: map[string]int{
						"gpu.nvidia.com":    2,
						"rdma.mellanox.com": 2,
					},
				},
				{
					Name:    "node-1-numa-0",
					Type:    PartitionNUMA,
					Profile: "gpu-nvidia-com-8_rdma-mellanox-com-8",
					DeviceCounts: map[string]int{
						"gpu.nvidia.com":    4,
						"rdma.mellanox.com": 4,
					},
				},
			},
		},
	}

	err := manager.SyncDeviceClasses(context.Background(), results)
	require.NoError(t, err)

	// Verify DeviceClasses were created
	classes, err := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)

	// Should have 4 classes: pcieroot + numa (per-instance) + pcieroot + numa (aggregates)
	assert.Len(t, classes.Items, 4)

	// Verify labels
	for _, dc := range classes.Items {
		assert.Equal(t, "true", dc.Labels[CoordinatorDriverName+"/managed"])
		assert.NotEmpty(t, dc.Labels[CoordinatorDriverName+"/partitionType"])
	}
}

func TestDeviceClassManager_BuildGroupingConfigUsesExplicitRequestContract(t *testing.T) {
	manager := NewDeviceClassManager(fake.NewSimpleClientset(), CoordinatorDriverName, NewTopologyRuleStore())
	instance := GroupingInstance{
		Mode:         OfferingModePassthrough,
		DeviceCounts: map[string]int{"gpu.nvidia.com": 1},
		Requests: map[string]RequestContract{
			"gpu.nvidia.com": {
				Class:     "vfio.gpu.nvidia.com",
				Selectors: []string{`device.attributes["gpu.nvidia.com"].type == "vfio"`},
				Opaque: &OpaqueConfig{
					Driver:     "gpu.nvidia.com",
					Parameters: []byte(`{"apiVersion":"gpu.nvidia.com/v1","kind":"VfioDeviceConfig"}`),
				},
				Passthrough: true,
			},
		},
	}

	config := manager.buildGroupingConfig(instance)
	require.Len(t, config.SubResources, 1)
	subResource := config.SubResources[0]
	assert.Equal(t, "gpu.nvidia.com", subResource.SourceDriver)
	assert.Equal(t, "vfio.gpu.nvidia.com", subResource.DeviceClass)
	assert.Equal(t, []string{`device.attributes["gpu.nvidia.com"].type == "vfio"`}, subResource.Selectors)
	assert.True(t, subResource.Passthrough)
	require.NotNil(t, subResource.Opaque)
	assert.Equal(t, "gpu.nvidia.com", subResource.Opaque.Driver)
	assert.JSONEq(t, `{"apiVersion":"gpu.nvidia.com/v1","kind":"VfioDeviceConfig"}`, string(subResource.Opaque.Parameters))
}

func TestDeviceClassManager_RejectsConflictingAggregateContracts(t *testing.T) {
	client := fake.NewSimpleClientset()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, NewTopologyRuleStore())
	results := []GroupingResult{{
		NodeName: "node-1",
		Instances: []GroupingInstance{
			{
				GroupingName:  "healthy",
				Alignment:     "pcieRoot",
				InstanceIndex: 0,
				PCIeRoots:     []string{"pci-9"},
				DeviceCounts:  map[string]int{"gpu.nvidia.com": 1},
			},
			{
				GroupingName:  "gpu-vfio",
				Alignment:     "pcieRoot",
				InstanceIndex: 0,
				Mode:          OfferingModePassthrough,
				PCIeRoots:     []string{"pci-0"},
				DeviceCounts:  map[string]int{"gpu.nvidia.com": 1},
				Requests: map[string]RequestContract{
					"gpu.nvidia.com": {Class: "vfio.gpu.nvidia.com", Passthrough: true},
				},
			},
			{
				GroupingName:  "gpu-vfio",
				Alignment:     "pcieRoot",
				InstanceIndex: 1,
				Mode:          OfferingModePassthrough,
				PCIeRoots:     []string{"pci-1"},
				DeviceCounts:  map[string]int{"gpu.nvidia.com": 1},
				Requests: map[string]RequestContract{
					"gpu.nvidia.com": {Class: "gpu.nvidia.com", Passthrough: true},
				},
			},
		},
	}}

	err := manager.SyncGroupingDeviceClasses(context.Background(), results)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "conflicting explicit request contract")

	// The conflicting grouping is skipped; the healthy one still publishes.
	_, getErr := client.ResourceV1().DeviceClasses().Get(context.Background(), "healthy", metav1.GetOptions{})
	assert.NoError(t, getErr)
	_, getErr = client.ResourceV1().DeviceClasses().Get(context.Background(), "gpu-vfio", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(getErr))
}

func TestDeviceClassManager_OverrideUsesEffectiveClassesInAlignments(t *testing.T) {
	rules := NewTopologyRuleStore()
	require.NoError(t, rules.LoadFromConfigMap(makeTopologyRuleConfigMap("gpu-vfio", "default", map[string]string{
		"attribute":   "gpu.amd.com/numaNode",
		"type":        "int",
		"driver":      "gpu.amd.com",
		"constraint":  "match",
		"deviceClass": "vfio.gpu.amd.com",
	})))
	manager := NewDeviceClassManager(fake.NewSimpleClientset(), CoordinatorDriverName, rules)
	config, _ := manager.buildPartitionConfig(PartitionPCIeRoot, PartitionDevice{
		DeviceCounts: map[string]int{
			"gpu.amd.com":       1,
			"rdma.mellanox.com": 1,
		},
	})

	var found bool
	for _, alignment := range config.Alignments {
		for _, request := range alignment.Requests {
			if request == "vfio.gpu.amd.com" {
				found = true
			}
			assert.NotEqual(t, "gpu.amd.com", request)
		}
	}
	assert.True(t, found, "alignment should reference the effective overridden DeviceClass")
	require.NoError(t, config.Validate())
}

func TestDeviceClassManager_SyncDeviceClasses_PCIePartitionsAreNotRails(t *testing.T) {
	client := fake.NewSimpleClientset()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, NewTopologyRuleStore())
	results := []PartitionResult{{
		NodeName: "node-1",
		Profile:  "gpu.example.com",
		Partitions: []PartitionDevice{{
			Name:         "node-1-pcie-0",
			Type:         PartitionPCIeRoot,
			Profile:      "gpu.example.com",
			NUMANodes:    []int64{0},
			PCIeRoots:    []string{"pci0000:00"},
			DeviceCounts: map[string]int{"gpu.example.com": 1},
		}},
	}}

	require.NoError(t, manager.SyncDeviceClasses(context.Background(), results))
	classes, err := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	for _, dc := range classes.Items {
		if dc.Labels[CoordinatorDriverName+"/partitionType"] != string(PartitionPCIeRoot) {
			continue
		}
		assert.NotContains(t, dc.Name, "-rail")
		assert.NotContains(t, dc.Labels, CoordinatorDriverName+"/railIndex")
	}
}

func TestDeviceClassManager_SyncDeviceClasses_RequiresLegacyPartitionRailMigration(t *testing.T) {
	legacy := &resourcev1.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gpu-example-com-pcieroot-numa0-pci0000-00-loose-rail0",
			Labels: map[string]string{
				CoordinatorDriverName + "/managed":       "true",
				CoordinatorDriverName + "/profile":       "gpu.example.com",
				CoordinatorDriverName + "/partitionType": string(PartitionPCIeRoot),
				CoordinatorDriverName + "/numa":          "numa0-pci0000-00",
				CoordinatorDriverName + "/coupling":      string(CouplingLoose),
				CoordinatorDriverName + "/railIndex":     "0",
			},
		},
	}
	client := fake.NewSimpleClientset(legacy)
	manager := NewDeviceClassManager(client, CoordinatorDriverName, NewTopologyRuleStore())
	results := []PartitionResult{{
		NodeName: "node-1",
		Profile:  "gpu.example.com",
		Partitions: []PartitionDevice{{
			Name:         "node-1-pcie-0",
			Type:         PartitionPCIeRoot,
			Profile:      "gpu.example.com",
			NUMANodes:    []int64{0},
			PCIeRoots:    []string{"pci0000:00"},
			DeviceCounts: map[string]int{"gpu.example.com": 1},
		}},
	}}

	err := manager.SyncDeviceClasses(context.Background(), results)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "legacy fixed-partition rail DeviceClass")
	_, err = client.ResourceV1().DeviceClasses().Get(context.Background(), legacy.Name, metav1.GetOptions{})
	assert.NoError(t, err)
	require.NoError(t, client.ResourceV1().DeviceClasses().Delete(context.Background(), legacy.Name, metav1.DeleteOptions{}))
	require.NoError(t, manager.SyncDeviceClasses(context.Background(), results))
}

func TestDeviceClassManager_SyncDeviceClasses_RejectsSanitizedPCIeRootCollision(t *testing.T) {
	client := fake.NewSimpleClientset()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, NewTopologyRuleStore())
	results := []PartitionResult{{
		NodeName: "node-1",
		Profile:  "gpu.example.com",
		Partitions: []PartitionDevice{
			{
				Name:         "node-1-pcie-colon",
				Type:         PartitionPCIeRoot,
				Profile:      "gpu.example.com",
				PCIeRoots:    []string{"pci:0000:00"},
				DeviceCounts: map[string]int{"gpu.example.com": 1},
			},
			{
				Name:         "node-1-pcie-dash",
				Type:         PartitionPCIeRoot,
				Profile:      "gpu.example.com",
				PCIeRoots:    []string{"pci-0000:00"},
				DeviceCounts: map[string]int{"gpu.example.com": 1},
			},
		},
	}}

	err := manager.SyncDeviceClasses(context.Background(), results)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PCIe-root DeviceClass name collision")
	classes, listErr := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, listErr)
	assert.Empty(t, classes.Items)
}

func TestDeviceClassManager_SyncDeviceClasses_RemovesStalePCIeRootByName(t *testing.T) {
	client := fake.NewSimpleClientset()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, NewTopologyRuleStore())
	partition := func(root string) PartitionDevice {
		return PartitionDevice{
			Name:         "node-1-" + root,
			Type:         PartitionPCIeRoot,
			Profile:      "gpu.example.com",
			PCIeRoots:    []string{root},
			DeviceCounts: map[string]int{"gpu.example.com": 1},
		}
	}
	first := []PartitionResult{{
		NodeName:   "node-1",
		Profile:    "gpu.example.com",
		Partitions: []PartitionDevice{partition("pci0000:00"), partition("pci0000:01")},
	}}
	require.NoError(t, manager.SyncDeviceClasses(context.Background(), first))
	classes, err := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	var staleName, activeName string
	for _, dc := range classes.Items {
		if strings.Contains(dc.Name, "pci0000-01") {
			staleName = dc.Name
		}
		if strings.Contains(dc.Name, "pci0000-00") {
			activeName = dc.Name
		}
	}
	require.NotEmpty(t, staleName)
	require.NotEmpty(t, activeName)

	second := []PartitionResult{{
		NodeName:   "node-1",
		Profile:    "gpu.example.com",
		Partitions: []PartitionDevice{partition("pci0000:00")},
	}}
	require.NoError(t, manager.SyncDeviceClasses(context.Background(), second))
	_, err = client.ResourceV1().DeviceClasses().Get(context.Background(), staleName, metav1.GetOptions{})
	assert.Error(t, err)
	_, err = client.ResourceV1().DeviceClasses().Get(context.Background(), activeName, metav1.GetOptions{})
	assert.NoError(t, err)
}

func TestDeviceClassManager_DeviceClassContents(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	results := []PartitionResult{
		{
			NodeName: "node-1",
			Profile:  "test",
			Partitions: []PartitionDevice{
				{
					Name:    "node-1-numa-0",
					Type:    PartitionNUMA,
					Profile: "test",
					DeviceCounts: map[string]int{
						"gpu.nvidia.com":    4,
						"rdma.mellanox.com": 4,
					},
					Devices: func() []TopologyDevice {
						pcieroot := "pci0000:00"
						var devs []TopologyDevice
						for i := 0; i < 4; i++ {
							devs = append(devs, TopologyDevice{DriverName: "gpu.nvidia.com", PCIeRoot: &pcieroot})
						}
						for i := 0; i < 4; i++ {
							devs = append(devs, TopologyDevice{DriverName: "rdma.mellanox.com", PCIeRoot: &pcieroot})
						}
						return devs
					}(),
				},
			},
		},
	}

	err := manager.SyncDeviceClasses(context.Background(), results)
	require.NoError(t, err)

	classes, err := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	// 1 per-instance + 1 aggregate = 2
	require.Len(t, classes.Items, 2)

	dc := findSpecificDC(classes.Items)

	// Verify CEL selector
	require.Len(t, dc.Spec.Selectors, 1)
	require.NotNil(t, dc.Spec.Selectors[0].CEL)
	assert.Contains(t, dc.Spec.Selectors[0].CEL.Expression, "partitionType")
	assert.Contains(t, dc.Spec.Selectors[0].CEL.Expression, "numa")

	// Verify opaque config
	require.Len(t, dc.Spec.Config, 1)
	require.NotNil(t, dc.Spec.Config[0].Opaque)
	assert.Equal(t, CoordinatorDriverName, dc.Spec.Config[0].Opaque.Driver)

	// Parse the opaque parameters
	var config PartitionConfig
	err = json.Unmarshal(dc.Spec.Config[0].Opaque.Parameters.Raw, &config)
	require.NoError(t, err)

	assert.Equal(t, "PartitionConfig", config.Kind)
	assert.Len(t, config.SubResources, 2, "should have 2 sub-resources (GPU + NIC)")

	// Verify sub-resources
	subResourceMap := make(map[string]int)
	for _, sr := range config.SubResources {
		subResourceMap[sr.DeviceClass] = sr.Count
	}
	assert.Equal(t, 4, subResourceMap["gpu.nvidia.com"])
	assert.Equal(t, 4, subResourceMap["rdma.mellanox.com"])

	// Default pcieroot alignment: all devices share pci0000:00, so pcieroot
	// alignment should be present (tight coupling). NUMA alignment uses
	// per-driver CEL selectors, not matchAttribute.
	hasPCIeAlignment := false
	for _, a := range config.Alignments {
		if a.Attribute == AttrNUMANode {
			t.Fatal("should NOT have NUMA matchAttribute — use per-driver CEL selectors instead")
		}
		if a.Attribute == AttrPCIeRoot {
			hasPCIeAlignment = true
		}
	}
	assert.True(t, hasPCIeAlignment, "should have default pcieroot alignment")
}

// TestDeviceClassManager_MixedPCIAndNonPCIDrivers verifies that pcieroot alignment
// constraints are only emitted for PCI-based drivers. When a partition contains a
// mix of PCI devices (NICs, GPUs) and non-PCI devices (CPUs, memory), the pcieroot
// constraint must exclude non-PCI drivers — they don't publish
// resource.kubernetes.io/pcieRoot, so including them makes the matchAttribute
// constraint unsatisfiable at scheduling time.
//
// This is the primary regression test for the fix. Without it, a quarter partition
// containing dra.cpu + SR-IOV NICs would produce a claim the scheduler rejects
// with "cannot allocate all claims" because dra.cpu devices lack pcieroot.
//
// NUMA alignment (numaNode) should still include ALL drivers regardless of PCI
// status, since both PCI and non-PCI devices have NUMA affinity.
func TestDeviceClassManager_MixedPCIAndNonPCIDrivers(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	pcieroot := "pci0000:15"
	results := []PartitionResult{
		{
			NodeName: "node-1",
			Profile:  "test",
			Partitions: []PartitionDevice{
				{
					Name:      "node-1-pcieroot-0",
					Type:      PartitionPCIeRoot,
					Profile:   "test",
					NUMANodes: []int64{0},
					// Simulates a real-world partition: SR-IOV NICs (PCI) + CPUs (non-PCI)
					// on the same NUMA node, as seen on multi-NUMA servers like Dell XE9680.
					DeviceCounts: map[string]int{
						"sriovnetwork.k8snetworkplumbingwg.io": 2,
						"dra.cpu":                              1,
					},
					Devices: []TopologyDevice{
						// NIC VFs are PCI devices — they publish pcieroot
						{DriverName: "sriovnetwork.k8snetworkplumbingwg.io", PCIeRoot: &pcieroot},
						{DriverName: "sriovnetwork.k8snetworkplumbingwg.io", PCIeRoot: &pcieroot},
						// CPUs are not PCI devices — PCIeRoot is nil
						{DriverName: "dra.cpu", PCIeRoot: nil},
					},
				},
			},
		},
	}

	err := manager.SyncDeviceClasses(context.Background(), results)
	require.NoError(t, err)

	classes, err := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	// 1 per-instance + 1 aggregate = 2
	require.Len(t, classes.Items, 2)

	var config PartitionConfig
	err = json.Unmarshal(findSpecificDC(classes.Items).Spec.Config[0].Opaque.Parameters.Raw, &config)
	require.NoError(t, err)

	// NUMA alignment is now per-driver CEL selectors, not matchAttribute.
	// No NUMA or PCIe matchAttribute should exist.
	for _, a := range config.Alignments {
		if a.Attribute == AttrNUMANode {
			t.Fatal("should NOT have NUMA matchAttribute — use per-driver CEL selectors instead")
		}
		if a.Attribute == AttrPCIeRoot {
			t.Fatal("should NOT have PCIe alignment")
		}
	}

	// Each sub-resource should have a fallback NUMA CEL selector using resource.kubernetes.io/numaNode
	// (no topology rules configured, so fallback to standard attribute)
	for _, sr := range config.SubResources {
		assert.NotEmpty(t, sr.Selectors,
			"sub-resource %s should have NUMA CEL selector", sr.DeviceClass)
	}
}

func TestDeviceClassManager_WithMatchConstraintRules(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()

	// Add NVLink match constraint rule
	err := rules.LoadFromConfigMap(makeTopologyRuleConfigMap("nvlink", "default", map[string]string{
		"attribute":  "gpu.nvidia.com/nvlinkDomain",
		"type":       "int",
		"driver":     "gpu.nvidia.com",
		"constraint": "match",
	}))
	require.NoError(t, err)

	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	results := []PartitionResult{
		{
			NodeName: "node-1",
			Profile:  "test",
			Partitions: []PartitionDevice{
				{
					Name:    "node-1-numa-0",
					Type:    PartitionNUMA,
					Profile: "test",
					DeviceCounts: map[string]int{
						"gpu.nvidia.com": 4,
					},
				},
			},
		},
	}

	err = manager.SyncDeviceClasses(context.Background(), results)
	require.NoError(t, err)

	classes, err := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	// 1 per-instance + 1 aggregate = 2
	require.Len(t, classes.Items, 2)

	var config PartitionConfig
	err = json.Unmarshal(findSpecificDC(classes.Items).Spec.Config[0].Opaque.Parameters.Raw, &config)
	require.NoError(t, err)

	// Verify NVLink match constraint is present
	hasNVLinkAlignment := false
	for _, a := range config.Alignments {
		if a.Attribute == "gpu.nvidia.com/nvlinkDomain" {
			hasNVLinkAlignment = true
		}
	}
	assert.True(t, hasNVLinkAlignment, "should have NVLink match constraint alignment")
}

func TestDeviceClassManager_EnforcementPropagation(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()

	// Add a preferred enforcement match constraint rule
	err := rules.LoadFromConfigMap(makeTopologyRuleConfigMap("preferred-numa", "default", map[string]string{
		"attribute":   "gpu.nvidia.com/numaNode",
		"type":        "int",
		"driver":      "gpu.nvidia.com",
		"constraint":  "match",
		"enforcement": "preferred",
	}))
	require.NoError(t, err)

	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	results := []PartitionResult{
		{
			NodeName: "node-1",
			Profile:  "test",
			Partitions: []PartitionDevice{
				{
					Name:    "node-1-numa-0",
					Type:    PartitionNUMA,
					Profile: "test",
					DeviceCounts: map[string]int{
						"gpu.nvidia.com": 4,
					},
				},
			},
		},
	}

	err = manager.SyncDeviceClasses(context.Background(), results)
	require.NoError(t, err)

	classes, err := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	// 1 per-instance + 1 aggregate = 2
	require.Len(t, classes.Items, 2)

	var config PartitionConfig
	err = json.Unmarshal(findSpecificDC(classes.Items).Spec.Config[0].Opaque.Parameters.Raw, &config)
	require.NoError(t, err)

	// NUMA is now per-driver CEL, no matchAttribute.
	// But rule-based match constraints (like gpu.nvidia.com/numaNode with constraint=match)
	// should still be emitted as matchAttribute alignments with their enforcement mode.
	hasPreferred := false
	for _, a := range config.Alignments {
		if a.Attribute == AttrNUMANode {
			t.Fatal("should NOT have NUMA matchAttribute")
		}
		if a.Attribute == "gpu.nvidia.com/numaNode" {
			hasPreferred = true
			assert.Equal(t, EnforcementPreferred, a.Enforcement,
				"rule-based alignment should propagate preferred enforcement")
		}
	}
	assert.True(t, hasPreferred, "should have preferred enforcement alignment from rule")
}

func TestDeviceClassManager_PerDriverCELSelectors(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()

	// Configure topology rules for GPU and CPU drivers with different NUMA attribute names
	err := rules.LoadFromConfigMap(makeTopologyRuleConfigMap("gpu-numa", "default", map[string]string{
		"attribute": "gpu.amd.com/numaNode",
		"type":      "int",
		"driver":    "gpu.amd.com",
		"mapsTo":    "numaNode",
	}))
	require.NoError(t, err)

	err = rules.LoadFromConfigMap(makeTopologyRuleConfigMap("cpu-numa", "default", map[string]string{
		"attribute": "dra.cpu/numaNodeID",
		"type":      "int",
		"driver":    "dra.cpu",
		"mapsTo":    "numaNode",
	}))
	require.NoError(t, err)

	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	results := []PartitionResult{
		{
			NodeName: "node-1",
			Profile:  "test",
			Partitions: []PartitionDevice{
				{
					Name:      "node-1-pcieroot-0",
					Type:      PartitionPCIeRoot,
					Profile:   "test",
					NUMANodes: []int64{0},
					DeviceCounts: map[string]int{
						"gpu.amd.com": 1,
						"dra.cpu":     1,
					},
				},
			},
		},
	}

	err = manager.SyncDeviceClasses(context.Background(), results)
	require.NoError(t, err)

	classes, err := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	// 1 per-instance + 1 aggregate = 2
	require.Len(t, classes.Items, 2)

	var config PartitionConfig
	err = json.Unmarshal(findSpecificDC(classes.Items).Spec.Config[0].Opaque.Parameters.Raw, &config)
	require.NoError(t, err)

	// Each sub-resource should have a CEL selector using its driver's own attribute name
	selectorMap := make(map[string][]string)
	for _, sr := range config.SubResources {
		selectorMap[sr.DeviceClass] = sr.Selectors
	}

	// GPU should use gpu.amd.com/numaNode
	gpuSelectors := selectorMap["gpu.amd.com"]
	require.Len(t, gpuSelectors, 1)
	assert.Equal(t, `has(device.attributes["gpu.amd.com"].numaNode) && device.attributes["gpu.amd.com"].numaNode == 0`, gpuSelectors[0])

	// CPU should use dra.cpu/numaNodeID
	cpuSelectors := selectorMap["dra.cpu"]
	require.Len(t, cpuSelectors, 1)
	assert.Equal(t, `has(device.attributes["dra.cpu"].numaNodeID) && device.attributes["dra.cpu"].numaNodeID == 0`, cpuSelectors[0])

	// No NUMA matchAttribute alignment should exist
	for _, a := range config.Alignments {
		if a.Attribute == AttrNUMANode {
			t.Fatal("should NOT have NUMA matchAttribute — per-driver CEL selectors replace it")
		}
	}

	// DeviceClass name should include NUMA suffix
	assert.Contains(t, findSpecificDC(classes.Items).Name, "numa0")
}

func TestDeviceClassManager_DeviceClassName(t *testing.T) {
	manager := &DeviceClassManager{driverName: CoordinatorDriverName}

	tests := []struct {
		profile  string
		partType PartitionType
		want     string
	}{
		{"test-profile", PartitionNUMA, "test-profile-numa"},
		{"test-profile", PartitionPCIeRoot, "test-profile-pcieroot"},
		{"UPPER_CASE", PartitionFull, "upper-case-full"},
		{"has spaces", PartitionPCIeRoot, "has-spaces-pcieroot"},
		{"", PartitionFull, "default-full"},
	}

	for _, tt := range tests {
		t.Run(tt.profile+"-"+string(tt.partType), func(t *testing.T) {
			got := manager.deviceClassName(tt.profile, tt.partType)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDeviceClassManager_Update(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	results := []PartitionResult{
		{
			NodeName: "node-1",
			Profile:  "test",
			Partitions: []PartitionDevice{
				{
					Name:         "node-1-full-0",
					Type:         PartitionFull,
					Profile:      "test",
					DeviceCounts: map[string]int{"gpu.nvidia.com": 8},
				},
			},
		},
	}

	// First sync
	err := manager.SyncDeviceClasses(context.Background(), results)
	require.NoError(t, err)

	// Update device count
	results[0].Partitions[0].DeviceCounts["gpu.nvidia.com"] = 4

	// Second sync (update)
	err = manager.SyncDeviceClasses(context.Background(), results)
	require.NoError(t, err)

	// Should still have 1 class
	classes, err := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, classes.Items, 1)
}

func TestDeviceClassManager_CleansUpStaleClasses(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	// First sync: create classes for pcieroot and numa
	results := []PartitionResult{
		{
			NodeName: "node-1",
			Profile:  "test",
			Partitions: []PartitionDevice{
				{Name: "p-pcieroot", Type: PartitionPCIeRoot, Profile: "test", DeviceCounts: map[string]int{"gpu": 2}},
				{Name: "p-numa", Type: PartitionNUMA, Profile: "test", DeviceCounts: map[string]int{"gpu": 4}},
			},
		},
	}
	err := manager.SyncDeviceClasses(context.Background(), results)
	require.NoError(t, err)

	classes, _ := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	// 2 per-instance + 2 aggregates = 4
	assert.Len(t, classes.Items, 4, "should have 4 DeviceClasses after first sync")

	// Second sync: only pcieroot remains (GPUs removed, no numa partition anymore)
	results = []PartitionResult{
		{
			NodeName: "node-1",
			Profile:  "test",
			Partitions: []PartitionDevice{
				{Name: "p-pcieroot", Type: PartitionPCIeRoot, Profile: "test", DeviceCounts: map[string]int{"gpu": 2}},
			},
		},
	}
	err = manager.SyncDeviceClasses(context.Background(), results)
	require.NoError(t, err)

	classes, _ = client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	// 1 per-instance + 1 aggregate = 2
	assert.Len(t, classes.Items, 2, "stale numa DeviceClasses should be cleaned up")
}

func TestDeviceClassManager_FallbackCouplingTight(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()

	// Add a pcieroot match rule with numaNode fallback
	err := rules.LoadFromConfigMap(makeTopologyRuleConfigMap("pcie-rule", "default", map[string]string{
		"attribute":         "resource.kubernetes.io/pcieRoot",
		"type":              "string",
		"driver":            "gpu.nvidia.com",
		"constraint":        "match",
		"fallbackAttribute": "numaNode",
	}))
	require.NoError(t, err)

	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	// Partition where GPU and NIC share a pcieroot — tight coupling
	pcieroot := "pci0000:15"
	results := []PartitionResult{
		{
			NodeName: "node-1",
			Profile:  "test",
			Partitions: []PartitionDevice{
				{
					Name:      "node-1-pcieroot-0",
					Type:      PartitionPCIeRoot,
					Profile:   "test",
					NUMANodes: []int64{0},
					DeviceCounts: map[string]int{
						"gpu.nvidia.com":    1,
						"rdma.mellanox.com": 1,
					},
					Devices: []TopologyDevice{
						{DriverName: "gpu.nvidia.com", PCIeRoot: &pcieroot},
						{DriverName: "rdma.mellanox.com", PCIeRoot: &pcieroot},
					},
				},
			},
		},
	}

	err = manager.SyncDeviceClasses(context.Background(), results)
	require.NoError(t, err)

	classes, err := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	// 1 per-instance + 1 aggregate = 2
	require.Len(t, classes.Items, 2)

	// Should have tight coupling label
	assert.Equal(t, "tight", findSpecificDC(classes.Items).Labels[CoordinatorDriverName+"/coupling"])

	// Should have pcieroot matchAttribute alignment
	var config PartitionConfig
	err = json.Unmarshal(findSpecificDC(classes.Items).Spec.Config[0].Opaque.Parameters.Raw, &config)
	require.NoError(t, err)

	hasPCIeAlignment := false
	for _, a := range config.Alignments {
		if a.Attribute == "resource.kubernetes.io/pcieRoot" {
			hasPCIeAlignment = true
		}
	}
	assert.True(t, hasPCIeAlignment, "tight partition should have pcieroot alignment")
}

func TestDeviceClassManager_FallbackCouplingLoose(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()

	err := rules.LoadFromConfigMap(makeTopologyRuleConfigMap("pcie-rule", "default", map[string]string{
		"attribute":         "resource.kubernetes.io/pcieRoot",
		"type":              "string",
		"driver":            "gpu.nvidia.com",
		"constraint":        "match",
		"fallbackAttribute": "numaNode",
	}))
	require.NoError(t, err)

	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	// Partition where GPU and NIC have DIFFERENT pcieroots — loose coupling
	gpuRoot := "pci0000:37"
	nicRoot := "pci0000:15"
	results := []PartitionResult{
		{
			NodeName: "node-1",
			Profile:  "test",
			Partitions: []PartitionDevice{
				{
					Name:      "node-1-pcieroot-1",
					Type:      PartitionPCIeRoot,
					Profile:   "test",
					NUMANodes: []int64{0},
					DeviceCounts: map[string]int{
						"gpu.nvidia.com":    1,
						"rdma.mellanox.com": 1,
					},
					Devices: []TopologyDevice{
						{DriverName: "gpu.nvidia.com", PCIeRoot: &gpuRoot},
						{DriverName: "rdma.mellanox.com", PCIeRoot: &nicRoot},
					},
				},
			},
		},
	}

	err = manager.SyncDeviceClasses(context.Background(), results)
	require.NoError(t, err)

	classes, err := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	// 1 per-instance + 1 aggregate = 2
	require.Len(t, classes.Items, 2)

	// Should have loose coupling label
	assert.Equal(t, "loose", findSpecificDC(classes.Items).Labels[CoordinatorDriverName+"/coupling"])

	// Should NOT have pcieroot matchAttribute alignment
	var config PartitionConfig
	err = json.Unmarshal(findSpecificDC(classes.Items).Spec.Config[0].Opaque.Parameters.Raw, &config)
	require.NoError(t, err)

	for _, a := range config.Alignments {
		if a.Attribute == "resource.kubernetes.io/pcieRoot" {
			t.Fatal("loose partition should NOT have pcieroot alignment")
		}
	}
}

func TestDeviceClassManager_FallbackMixedCoupling(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()

	err := rules.LoadFromConfigMap(makeTopologyRuleConfigMap("pcie-rule", "default", map[string]string{
		"attribute":         "resource.kubernetes.io/pcieRoot",
		"type":              "string",
		"driver":            "gpu.nvidia.com",
		"constraint":        "match",
		"fallbackAttribute": "numaNode",
	}))
	require.NoError(t, err)

	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	sharedRoot := "pci0000:15"
	gpuOnlyRoot := "pci0000:37"
	nicRoot := "pci0000:15"
	results := []PartitionResult{
		{
			NodeName: "node-1",
			Profile:  "test",
			Partitions: []PartitionDevice{
				{
					Name:      "node-1-pcieroot-0",
					Type:      PartitionPCIeRoot,
					Profile:   "test",
					NUMANodes: []int64{0},
					DeviceCounts: map[string]int{
						"gpu.nvidia.com":    1,
						"rdma.mellanox.com": 1,
					},
					Devices: []TopologyDevice{
						{DriverName: "gpu.nvidia.com", PCIeRoot: &sharedRoot},
						{DriverName: "rdma.mellanox.com", PCIeRoot: &nicRoot},
					},
				},
				{
					Name:      "node-1-pcieroot-1",
					Type:      PartitionPCIeRoot,
					Profile:   "test",
					NUMANodes: []int64{0},
					DeviceCounts: map[string]int{
						"gpu.nvidia.com":    1,
						"rdma.mellanox.com": 1,
					},
					Devices: []TopologyDevice{
						{DriverName: "gpu.nvidia.com", PCIeRoot: &gpuOnlyRoot},
						{DriverName: "rdma.mellanox.com", PCIeRoot: &nicRoot},
					},
				},
			},
		},
	}

	err = manager.SyncDeviceClasses(context.Background(), results)
	require.NoError(t, err)

	classes, err := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)

	// 2 per-instance (tight+loose) + 1 aggregate + 1 tier ("half" = 1/2) = 4
	assert.Len(t, classes.Items, 4, "mixed coupling should produce 4 DeviceClasses")

	couplings := map[string]bool{}
	var tierNames []string
	for _, dc := range classes.Items {
		if c := dc.Labels[CoordinatorDriverName+"/coupling"]; c != "" {
			couplings[c] = true
		}
		if tn := dc.Labels[CoordinatorDriverName+"/tierName"]; tn != "" {
			tierNames = append(tierNames, tn)
		}
	}
	assert.True(t, couplings["tight"], "should have a tight DeviceClass")
	assert.True(t, couplings["loose"], "should have a loose DeviceClass")
	assert.Contains(t, tierNames, "half", "should have a 'half' tier alias (2 pcieroot partitions → 1/2)")
}

func TestDeviceClassManager_NoFallbackAttribute(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()

	// Rule WITHOUT fallbackAttribute — should behave as before
	err := rules.LoadFromConfigMap(makeTopologyRuleConfigMap("nvlink", "default", map[string]string{
		"attribute":  "gpu.nvidia.com/nvlinkDomain",
		"type":       "int",
		"driver":     "gpu.nvidia.com",
		"constraint": "match",
	}))
	require.NoError(t, err)

	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	results := []PartitionResult{
		{
			NodeName: "node-1",
			Profile:  "test",
			Partitions: []PartitionDevice{
				{
					Name:    "node-1-numa-0",
					Type:    PartitionNUMA,
					Profile: "test",
					DeviceCounts: map[string]int{
						"gpu.nvidia.com": 4,
					},
				},
			},
		},
	}

	err = manager.SyncDeviceClasses(context.Background(), results)
	require.NoError(t, err)

	classes, err := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	// 1 per-instance + 1 aggregate = 2
	require.Len(t, classes.Items, 2)

	// With the default pcieroot rule (which has fallback), coupling label may
	// be set to "loose" when devices don't publish pcieroot. The explicit
	// nvlink rule without fallback always emits its alignment regardless.
	coupling := findSpecificDC(classes.Items).Labels[CoordinatorDriverName+"/coupling"]
	if coupling != "" {
		assert.Equal(t, string(CouplingLoose), coupling, "coupling should be loose when pcieroot is unsatisfiable")
	}

	// Should still have the matchAttribute alignment
	var config PartitionConfig
	err = json.Unmarshal(findSpecificDC(classes.Items).Spec.Config[0].Opaque.Parameters.Raw, &config)
	require.NoError(t, err)

	hasNVLink := false
	for _, a := range config.Alignments {
		if a.Attribute == "gpu.nvidia.com/nvlinkDomain" {
			hasNVLink = true
		}
	}
	assert.True(t, hasNVLink, "should have NVLink alignment without fallback")
}

func TestFractionToTierName(t *testing.T) {
	tests := []struct {
		num, den int
		want     string
	}{
		{1, 8, "eighth"},
		{1, 4, "quarter"},
		{2, 8, "quarter"},
		{1, 2, "half"},
		{4, 8, "half"},
		{1, 1, "full"},
		{8, 8, "full"},
		{1, 3, "third"},
		{1, 6, "sixth"},
		{1, 16, "sixteenth"},
		{1, 12, "twelfth"},
		{3, 8, ""},
		{1, 5, ""},
		{1, 7, ""},
		{0, 8, ""},
		{1, 0, ""},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d/%d", tt.num, tt.den), func(t *testing.T) {
			got := fractionToTierName(tt.num, tt.den)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDeviceClassManager_TierNamedAggregates(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	// Simulate an 8-PCIe-root, 4-NUMA node (typical HGX-style).
	// 8 pcieroot partitions, 4 NUMA partitions (2 PCIe roots per NUMA).
	var partitions []PartitionDevice
	numaForRoot := []int64{0, 0, 1, 1, 2, 2, 3, 3}
	for i := 0; i < 8; i++ {
		partitions = append(partitions, PartitionDevice{
			Name:         fmt.Sprintf("node-1-pcieroot-%d", i),
			Type:         PartitionPCIeRoot,
			Profile:      "hgx-b200",
			NUMANodes:    []int64{numaForRoot[i]},
			PCIeRoots:    []string{fmt.Sprintf("pci0000:%02x", i)},
			DeviceCounts: map[string]int{"gpu.nvidia.com": 1, "rdma.mellanox.com": 1},
		})
	}
	for i := 0; i < 4; i++ {
		partitions = append(partitions, PartitionDevice{
			Name:         fmt.Sprintf("node-1-numa-%d", i),
			Type:         PartitionNUMA,
			Profile:      "hgx-b200",
			NUMANodes:    []int64{int64(i)},
			DeviceCounts: map[string]int{"gpu.nvidia.com": 2, "rdma.mellanox.com": 2},
		})
	}
	partitions = append(partitions, PartitionDevice{
		Name:         "node-1-full",
		Type:         PartitionFull,
		Profile:      "hgx-b200",
		DeviceCounts: map[string]int{"gpu.nvidia.com": 8, "rdma.mellanox.com": 8},
	})

	results := []PartitionResult{{
		NodeName:   "node-1",
		Profile:    "hgx-b200",
		Partitions: partitions,
	}}

	err := manager.SyncDeviceClasses(context.Background(), results)
	require.NoError(t, err)

	classes, err := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)

	// Collect DeviceClass names
	names := make(map[string]bool)
	for _, dc := range classes.Items {
		names[dc.Name] = true
	}

	// Topology-named aggregates
	assert.True(t, names["pcieroot"], "should have aggregate 'pcieroot' DeviceClass")
	assert.True(t, names["numa"], "should have aggregate 'numa' DeviceClass")
	assert.True(t, names["full"], "should have 'full' DeviceClass")

	// Tier-named aggregates
	assert.True(t, names["eighth"], "pcieroot (1/8) should produce 'eighth' tier alias")
	assert.True(t, names["quarter"], "numa (2/8=1/4) should produce 'quarter' tier alias")

	// Verify tier DeviceClasses have correct labels
	for _, dc := range classes.Items {
		if dc.Name == "eighth" {
			assert.Equal(t, "eighth", dc.Labels[CoordinatorDriverName+"/tierName"])
			assert.Equal(t, "pcieroot", dc.Labels[CoordinatorDriverName+"/partitionType"])
		}
		if dc.Name == "quarter" {
			assert.Equal(t, "quarter", dc.Labels[CoordinatorDriverName+"/tierName"])
			assert.Equal(t, "numa", dc.Labels[CoordinatorDriverName+"/partitionType"])
		}
	}
}

func TestDeviceClassManager_SLITReachability(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	// 4 NUMA nodes, 1 GPU each (scalar NUMA), NICs only on NUMA 0 with SLIT [0,1,2,3]
	var partitions []PartitionDevice
	for i := 0; i < 4; i++ {
		p := PartitionDevice{
			Name:         fmt.Sprintf("node-1-numa-%d", i),
			Type:         PartitionNUMA,
			Profile:      "test-slit",
			NUMANodes:    []int64{int64(i)},
			DeviceCounts: map[string]int{"gpu.amd.com": 1},
			Devices: []TopologyDevice{
				{DriverName: "gpu.amd.com", NUMANode: intPtr(int64(i)), NUMANodes: []int64{int64(i)}},
			},
		}
		// Add NICs only on NUMA 0, but reachable to 0-3 via SLIT
		if i == 0 {
			p.DeviceCounts["sriov"] = 4
			for j := 0; j < 4; j++ {
				p.Devices = append(p.Devices, TopologyDevice{
					DriverName: "sriov",
					NUMANode:   intPtr(0),
					NUMANodes:  []int64{0, 1, 2, 3},
				})
			}
		}
		partitions = append(partitions, p)
	}
	partitions = append(partitions, PartitionDevice{
		Name:         "node-1-full",
		Type:         PartitionFull,
		Profile:      "test-slit",
		DeviceCounts: map[string]int{"gpu.amd.com": 4, "sriov": 4},
	})

	results := []PartitionResult{{
		NodeName:   "node-1",
		Profile:    "test-slit",
		Partitions: partitions,
	}}

	err := manager.SyncDeviceClasses(context.Background(), results)
	require.NoError(t, err)

	classes, err := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)

	// Find the numa aggregate
	var numaAgg *resourcev1.DeviceClass
	for i := range classes.Items {
		if classes.Items[i].Name == "numa" {
			numaAgg = &classes.Items[i]
			break
		}
	}
	require.NotNil(t, numaAgg, "should have a 'numa' aggregate DeviceClass")

	var config PartitionConfig
	err = json.Unmarshal(numaAgg.Spec.Config[0].Opaque.Parameters.Raw, &config)
	require.NoError(t, err)

	// NUMA aggregate should include NIC via SLIT reachability
	subResources := make(map[string]int)
	for _, sr := range config.SubResources {
		subResources[sr.DeviceClass] = sr.Count
	}
	assert.Equal(t, 1, subResources["gpu.amd.com"], "GPU count should be 1 (not inflated)")
	assert.Equal(t, 1, subResources["sriov"], "NIC count should be 1 per NUMA (4 VFs / 4 sharing NUMAs)")
}

func TestIsPartitionConstraintSatisfiable_SharedRoot(t *testing.T) {
	pcieRoot := "pci0000:15"
	devices := []TopologyDevice{
		{DriverName: "gpu.nvidia.com", PCIeRoot: &pcieRoot},
		{DriverName: "rdma.mellanox.com", PCIeRoot: &pcieRoot},
	}
	driverCounts := map[string]int{
		"gpu.nvidia.com":    1,
		"rdma.mellanox.com": 1,
	}

	assert.True(t, isPartitionConstraintSatisfiable(devices, AttrPCIeRoot, driverCounts))
}

func TestIsPartitionConstraintSatisfiable_DifferentRoots(t *testing.T) {
	gpuRoot := "pci0000:15"
	nicRoot := "pci0000:37"
	devices := []TopologyDevice{
		{DriverName: "gpu.nvidia.com", PCIeRoot: &gpuRoot},
		{DriverName: "rdma.mellanox.com", PCIeRoot: &nicRoot},
	}
	driverCounts := map[string]int{
		"gpu.nvidia.com":    1,
		"rdma.mellanox.com": 1,
	}

	assert.False(t, isPartitionConstraintSatisfiable(devices, AttrPCIeRoot, driverCounts))
}

func TestIsPartitionConstraintSatisfiable_NonPCIDriverSkipped(t *testing.T) {
	pcieRoot := "pci0000:15"
	devices := []TopologyDevice{
		{DriverName: "gpu.nvidia.com", PCIeRoot: &pcieRoot},
		{DriverName: "rdma.mellanox.com", PCIeRoot: &pcieRoot},
		{DriverName: "dra.cpu", PCIeRoot: nil},
	}
	driverCounts := map[string]int{
		"gpu.nvidia.com":    1,
		"rdma.mellanox.com": 1,
		"dra.cpu":           1,
	}

	assert.True(t, isPartitionConstraintSatisfiable(devices, AttrPCIeRoot, driverCounts),
		"CPU driver without pcieRoot should be skipped, not cause failure")
}

func TestIsPartitionConstraintSatisfiable_SingleDriver(t *testing.T) {
	pcieRoot := "pci0000:15"
	devices := []TopologyDevice{
		{DriverName: "gpu.nvidia.com", PCIeRoot: &pcieRoot},
	}
	driverCounts := map[string]int{"gpu.nvidia.com": 1}

	assert.False(t, isPartitionConstraintSatisfiable(devices, AttrPCIeRoot, driverCounts),
		"single driver should return false (need 2+ for meaningful constraint)")
}

func TestSanitizeDNSLabel(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"valid passthrough", "my-label"},
		{"uppercase lowered", "My-Label"},
		{"dots to dashes", "gpu.nvidia.com"},
		{"truncates long", "this-is-a-very-long-label-name-that-exceeds-the-sixty-three-character-limit-for-dns-labels"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizeDNSLabel(tt.input)
			assert.LessOrEqual(t, len(result), 63, "DNS label must be <= 63 chars")
			assert.NotEmpty(t, result)
		})
	}
}

// multiDeviceGroupingInstance returns a grouping instance with three device
// classes, so buildGroupingConfig's map-ranging over DeviceCounts has enough
// entries to expose ordering nondeterminism if sortPartitionConfig is absent
// or broken.
func multiDeviceGroupingInstance(railIndex int) GroupingInstance {
	return GroupingInstance{
		Name:         fmt.Sprintf("node-1-gpu-nic-pair-pcieroot-%d", railIndex),
		NodeName:     "node-1",
		GroupingName: "gpu-nic-pair",
		Rail:         true,
		Alignment:    "pcieRoot",
		PCIeRoots:    []string{fmt.Sprintf("pci0000:%02d", railIndex)},
		DeviceCounts: map[string]int{
			"gpu.nvidia.com":    1,
			"rdma.mellanox.com": 1,
			"nvme.example.com":  1,
		},
		InstanceIndex: railIndex,
	}
}

func TestDeviceClassManager_GroupingConfigIsDeterministic(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	inst := multiDeviceGroupingInstance(0)

	var firstRaw []byte
	for i := 0; i < 20; i++ {
		config := manager.buildGroupingConfig(inst)
		raw, err := json.Marshal(config)
		require.NoError(t, err)
		if i == 0 {
			firstRaw = raw
			continue
		}
		assert.Equal(t, string(firstRaw), string(raw),
			"buildGroupingConfig must marshal identically across calls, iteration %d", i)
	}
}

// passthroughRequests marks each topology class as a passthrough request that
// uses the topology class itself.
func passthroughRequests(classes ...string) map[string]RequestContract {
	requests := make(map[string]RequestContract, len(classes))
	for _, class := range classes {
		requests[class] = RequestContract{Passthrough: true}
	}
	return requests
}

// buildGroupingConfig appends a NUMA selector to the generated request; it must
// not write through the instance's Selectors slice, which may share backing
// storage with the DeviceGrouping.
func TestDeviceClassManager_BuildGroupingConfigDoesNotMutateSelectors(t *testing.T) {
	manager := NewDeviceClassManager(fake.NewSimpleClientset(), CoordinatorDriverName, NewTopologyRuleStore())
	selectors := make([]string, 1, 4)
	selectors[0] = `device.attributes["gpu.nvidia.com"].type == "vfio"`
	spare := selectors[:2:4]
	instance := GroupingInstance{
		NUMANodes:    []int64{0},
		DeviceCounts: map[string]int{"gpu.nvidia.com": 1},
		Requests:     map[string]RequestContract{"gpu.nvidia.com": {Selectors: selectors}},
	}

	for i := 0; i < 2; i++ {
		config := manager.buildGroupingConfig(instance)
		require.Len(t, config.SubResources, 1)
		assert.Len(t, config.SubResources[0].Selectors, 2)
	}
	assert.Equal(t, []string{`device.attributes["gpu.nvidia.com"].type == "vfio"`}, instance.Requests["gpu.nvidia.com"].Selectors)
	assert.Empty(t, spare[1], "spare capacity must stay untouched")
}

func TestDeviceClassManager_GroupingConfigCarriesPassthroughMode(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	inst := multiDeviceGroupingInstance(0)
	inst.Mode = OfferingModePassthrough
	inst.Requests = passthroughRequests("gpu.nvidia.com", "rdma.mellanox.com")

	config := manager.buildGroupingConfig(inst)
	assert.Equal(t, OfferingModePassthrough, config.Mode)
	for _, sr := range config.SubResources {
		assert.Equal(t, inst.Requests[sr.DeviceClass].Passthrough, sr.Passthrough, sr.DeviceClass)
	}
}

func TestDeviceClassManager_NUMAGroupingUsesRequestedAlignment(t *testing.T) {
	manager := NewDeviceClassManager(fake.NewSimpleClientset(), CoordinatorDriverName, NewTopologyRuleStore())
	instance := GroupingInstance{
		Alignment:    "numaNode",
		DeviceCounts: map[string]int{"gpu.nvidia.com": 1, "rdma.mellanox.com": 1},
		Devices: []TopologyDevice{
			makeTestDevice("gpu.nvidia.com", "gpu-0", int64Ptr(0), stringPtr("pci0000:00")),
			makeTestDevice("rdma.mellanox.com", "rdma-0", int64Ptr(0), stringPtr("pci0000:01")),
		},
	}

	config := manager.buildGroupingConfig(instance)

	for _, alignment := range config.Alignments {
		assert.NotEqual(t, AttrPCIeRoot, alignment.Attribute,
			"NUMA grouping must not inherit the built-in PCIe-root constraint")
	}
}

func TestDeviceClassManager_ExplicitPCIeRuleDoesNotOverrideNUMAAlignment(t *testing.T) {
	rules := NewTopologyRuleStore()
	require.NoError(t, rules.LoadFromConfigMap(makeTopologyRuleConfigMap("pcie-root", "default", map[string]string{
		"attribute":  AttrPCIeRoot,
		"type":       "string",
		"driver":     "gpu.nvidia.com",
		"constraint": "match",
	})))
	manager := NewDeviceClassManager(fake.NewSimpleClientset(), CoordinatorDriverName, rules)
	instance := GroupingInstance{
		Alignment:    "numaNode",
		DeviceCounts: map[string]int{"gpu.nvidia.com": 1, "rdma.mellanox.com": 1},
		Devices: []TopologyDevice{
			makeTestDevice("gpu.nvidia.com", "gpu-0", int64Ptr(0), stringPtr("pci0000:00")),
			makeTestDevice("rdma.mellanox.com", "rdma-0", int64Ptr(0), stringPtr("pci0000:01")),
		},
	}

	config := manager.buildGroupingConfig(instance)

	for _, alignment := range config.Alignments {
		assert.NotEqual(t, AttrPCIeRoot, alignment.Attribute,
			"explicit PCIe-root rules must not override a NUMA grouping alignment")
	}
}

func TestDeviceClassManager_ContainerGroupingDoesNotCarryPassthrough(t *testing.T) {
	client := fake.NewSimpleClientset()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, NewTopologyRuleStore())
	instance := multiDeviceGroupingInstance(0)
	instance.Mode = OfferingModeContainer
	instance.Requests = passthroughRequests("gpu.nvidia.com")

	config := manager.buildGroupingConfig(instance)
	assert.Equal(t, OfferingModeContainer, config.Mode)
	for _, sr := range config.SubResources {
		assert.False(t, sr.Passthrough, sr.DeviceClass)
	}
}

func TestDeviceClassManager_GroupingAggregateCarriesPassthroughMode(t *testing.T) {
	client := fake.NewSimpleClientset()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, NewTopologyRuleStore())
	instance := multiDeviceGroupingInstance(0)
	instance.Mode = OfferingModePassthrough
	instance.Requests = passthroughRequests("gpu.nvidia.com", "rdma.mellanox.com")

	require.NoError(t, manager.SyncGroupingDeviceClasses(context.Background(), []GroupingResult{{
		NodeName:  "node-1",
		Instances: []GroupingInstance{instance},
	}}))

	dc, err := client.ResourceV1().DeviceClasses().Get(context.Background(), "gpu-nic-pair", metav1.GetOptions{})
	require.NoError(t, err)
	var config PartitionConfig
	require.NoError(t, json.Unmarshal(dc.Spec.Config[0].Opaque.Parameters.Raw, &config))
	assert.Equal(t, OfferingModePassthrough, config.Mode)
	for _, sr := range config.SubResources {
		assert.Equal(t, instance.Requests[sr.DeviceClass].Passthrough, sr.Passthrough, sr.DeviceClass)
	}
}

func TestDeviceClassManager_SyncGroupingDeviceClasses_AggregateIsDeterministic(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	// Two rails for the same grouping name, so the aggregate's representative
	// is picked from a map with more than one entry.
	results := []GroupingResult{
		{NodeName: "node-1", Instances: []GroupingInstance{
			multiDeviceGroupingInstance(0),
			multiDeviceGroupingInstance(1),
		}},
	}

	for i := 0; i < 10; i++ {
		err := manager.SyncGroupingDeviceClasses(context.Background(), results)
		require.NoError(t, err)
	}

	dc, err := client.ResourceV1().DeviceClasses().Get(context.Background(), "gpu-nic-pair", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "pcieRoot", dc.Labels[CoordinatorDriverName+"/alignment"])
}

func TestDeviceClassManager_SyncGroupingDeviceClasses_IntentMetadataOnlyOnAggregate(t *testing.T) {
	client := fake.NewSimpleClientset()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, NewTopologyRuleStore())
	instance := multiDeviceGroupingInstance(0)
	instance.Intent = IntentMetadata{
		DisplayName:   "GPU inference",
		Description:   "GPU and NIC resources for inference",
		IntentLevel:   "workload",
		Categories:    []string{"ai", "inference"},
		WorkloadTypes: []string{"training", "serving"},
		Visibility:    "public",
	}
	results := []GroupingResult{{NodeName: "node-1", Instances: []GroupingInstance{instance}}}
	require.NoError(t, manager.SyncGroupingDeviceClasses(context.Background(), results))

	aggregate, err := client.ResourceV1().DeviceClasses().Get(context.Background(), "gpu-nic-pair", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "workload", aggregate.Labels[CoordinatorDriverName+"/intent-level"])
	assert.Equal(t, "public", aggregate.Labels[CoordinatorDriverName+"/visibility"])
	assert.Equal(t, "GPU inference", aggregate.Annotations[CoordinatorDriverName+"/display-name"])
	assert.Equal(t, "GPU and NIC resources for inference", aggregate.Annotations[CoordinatorDriverName+"/description"])
	assert.Equal(t, "ai,inference", aggregate.Annotations[CoordinatorDriverName+"/categories"])
	assert.Equal(t, "training,serving", aggregate.Annotations[CoordinatorDriverName+"/workload-types"])
	assert.NotContains(t, aggregate.Labels, CoordinatorDriverName+"/railIndex")

	rail, err := client.ResourceV1().DeviceClasses().Get(context.Background(), "gpu-nic-pair-rail0", metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, rail.Labels, CoordinatorDriverName+"/intent-level")
	assert.NotContains(t, rail.Labels, CoordinatorDriverName+"/visibility")
	assert.NotContains(t, rail.Annotations, CoordinatorDriverName+"/display-name")
	assert.NotContains(t, rail.Annotations, CoordinatorDriverName+"/description")
	assert.NotContains(t, rail.Annotations, CoordinatorDriverName+"/categories")
	assert.NotContains(t, rail.Annotations, CoordinatorDriverName+"/workload-types")
}

func TestDeviceClassManager_SyncGroupingDeviceClasses_NoIntentMetadata(t *testing.T) {
	client := fake.NewSimpleClientset()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, NewTopologyRuleStore())
	results := []GroupingResult{{NodeName: "node-1", Instances: []GroupingInstance{multiDeviceGroupingInstance(0)}}}
	require.NoError(t, manager.SyncGroupingDeviceClasses(context.Background(), results))

	classes, err := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	for _, dc := range classes.Items {
		assert.NotContains(t, dc.Labels, CoordinatorDriverName+"/intent-level")
		assert.NotContains(t, dc.Labels, CoordinatorDriverName+"/visibility")
		assert.NotContains(t, dc.Annotations, CoordinatorDriverName+"/display-name")
		assert.NotContains(t, dc.Annotations, CoordinatorDriverName+"/description")
		assert.NotContains(t, dc.Annotations, CoordinatorDriverName+"/categories")
		assert.NotContains(t, dc.Annotations, CoordinatorDriverName+"/workload-types")
	}
}

func TestDeviceClassManager_SyncGroupingDeviceClasses_RemovesLegacyRailNameForNonRail(t *testing.T) {
	legacy := &resourcev1.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gpu-nic-pair-rail0",
			Labels: map[string]string{
				CoordinatorDriverName + "/managed":   "true",
				CoordinatorDriverName + "/grouping":  "gpu-nic-pair",
				CoordinatorDriverName + "/railIndex": "0",
			},
		},
	}
	client := fake.NewSimpleClientset(legacy)
	manager := NewDeviceClassManager(client, CoordinatorDriverName, NewTopologyRuleStore())
	nonRail := GroupingInstance{
		GroupingName:  "gpu-nic-pair",
		Alignment:     "numaNode",
		NUMANodes:     []int64{0},
		InstanceIndex: 0,
		DeviceCounts:  map[string]int{"gpu.example.com": 1},
	}

	results := []GroupingResult{{
		NodeName:  "node-1",
		Instances: []GroupingInstance{nonRail},
	}}

	err := manager.SyncGroupingDeviceClasses(context.Background(), results)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "legacy non-rail grouping DeviceClass")

	_, err = client.ResourceV1().DeviceClasses().Get(context.Background(), legacy.Name, metav1.GetOptions{})
	assert.NoError(t, err)
	require.NoError(t, client.ResourceV1().DeviceClasses().Delete(context.Background(), legacy.Name, metav1.DeleteOptions{}))
	require.NoError(t, manager.SyncGroupingDeviceClasses(context.Background(), results))
	_, err = client.ResourceV1().DeviceClasses().Get(context.Background(), "gpu-nic-pair-numa0", metav1.GetOptions{})
	assert.NoError(t, err)
}

func TestDeviceClassManager_SyncGroupingDeviceClasses_AutoDetectedHasNoIntentMetadata(t *testing.T) {
	model := NewTopologyModel()
	nodeTopo := &NodeTopology{
		NodeName: "node-1",
		DevicesByDriver: map[string][]TopologyDevice{
			"gpu.amd.com":     {makeTestDevice("gpu.amd.com", "gpu-0", int64Ptr(0), stringPtr("pcie-0"))},
			"nic.example.com": {makeTestDevice("nic.example.com", "nic-0", int64Ptr(0), stringPtr("pcie-0"))},
		},
	}
	model.mu.Lock()
	model.nodes = map[string]*NodeTopology{"node-1": nodeTopo}
	model.mu.Unlock()
	rules := NewTopologyRuleStore()
	autoGroupings := NewPartitionBuilder(model, rules).DetectPCIeRootPairings()
	require.NotEmpty(t, autoGroupings)
	results := NewGroupingBuilder(model, rules).BuildGroupings(autoGroupings)
	client := fake.NewSimpleClientset()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)
	require.NoError(t, manager.SyncGroupingDeviceClasses(context.Background(), results))

	classes, err := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, classes.Items)
	for _, dc := range classes.Items {
		assert.NotContains(t, dc.Name, "-rail")
		assert.NotContains(t, dc.Labels, CoordinatorDriverName+"/railIndex")
		assert.NotContains(t, dc.Labels, CoordinatorDriverName+"/intent-level")
		assert.NotContains(t, dc.Labels, CoordinatorDriverName+"/visibility")
		assert.NotContains(t, dc.Annotations, CoordinatorDriverName+"/display-name")
		assert.NotContains(t, dc.Annotations, CoordinatorDriverName+"/description")
		assert.NotContains(t, dc.Annotations, CoordinatorDriverName+"/categories")
		assert.NotContains(t, dc.Annotations, CoordinatorDriverName+"/workload-types")
	}
}

func TestDeviceClassManager_SyncGroupingDeviceClasses_AggregateOmitsRailIndex(t *testing.T) {
	client := fake.NewSimpleClientset()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, NewTopologyRuleStore())
	results := []GroupingResult{{NodeName: "node-1", Instances: []GroupingInstance{multiDeviceGroupingInstance(0)}}}
	require.NoError(t, manager.SyncGroupingDeviceClasses(context.Background(), results))

	aggregate, err := client.ResourceV1().DeviceClasses().Get(context.Background(), "gpu-nic-pair", metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, aggregate.Labels, CoordinatorDriverName+"/railIndex")
	rail, err := client.ResourceV1().DeviceClasses().Get(context.Background(), "gpu-nic-pair-rail0", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "0", rail.Labels[CoordinatorDriverName+"/railIndex"])
}

func TestDeviceClassManager_PublishDeviceClass_PreservesForeignMetadata(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	results := []GroupingResult{
		{NodeName: "node-1", Instances: []GroupingInstance{multiDeviceGroupingInstance(0)}},
	}
	require.NoError(t, manager.SyncGroupingDeviceClasses(context.Background(), results))

	// Simulate a foreign actor (kubectl, Helm, ArgoCD) annotating/labeling the
	// managed DeviceClass.
	dc, err := client.ResourceV1().DeviceClasses().Get(context.Background(), "gpu-nic-pair", metav1.GetOptions{})
	require.NoError(t, err)
	if dc.Annotations == nil {
		dc.Annotations = map[string]string{}
	}
	dc.Annotations["kubectl.kubernetes.io/last-applied-configuration"] = "{}"
	if dc.Labels == nil {
		dc.Labels = map[string]string{}
	}
	dc.Labels["app.kubernetes.io/managed-by"] = "Helm"
	_, err = client.ResourceV1().DeviceClasses().Update(context.Background(), dc, metav1.UpdateOptions{})
	require.NoError(t, err)

	// Reconcile again with no config change.
	require.NoError(t, manager.SyncGroupingDeviceClasses(context.Background(), results))

	dc, err = client.ResourceV1().DeviceClasses().Get(context.Background(), "gpu-nic-pair", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "{}", dc.Annotations["kubectl.kubernetes.io/last-applied-configuration"],
		"foreign annotation must survive reconciliation")
	assert.Equal(t, "Helm", dc.Labels["app.kubernetes.io/managed-by"],
		"foreign label must survive reconciliation")
	assert.Equal(t, "true", dc.Labels[CoordinatorDriverName+"/managed"], "TC-owned label must still be set")
}

func TestDeviceClassManager_PublishDeviceClass_NoOpWhenUnchanged(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	results := []GroupingResult{
		{NodeName: "node-1", Instances: []GroupingInstance{multiDeviceGroupingInstance(0)}},
	}
	require.NoError(t, manager.SyncGroupingDeviceClasses(context.Background(), results))

	before, err := client.ResourceV1().DeviceClasses().Get(context.Background(), "gpu-nic-pair", metav1.GetOptions{})
	require.NoError(t, err)

	require.NoError(t, manager.SyncGroupingDeviceClasses(context.Background(), results))

	after, err := client.ResourceV1().DeviceClasses().Get(context.Background(), "gpu-nic-pair", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion,
		"an unchanged reconcile must not issue an Update (ResourceVersion must not advance)")
}

func TestDeviceClassManager_SyncGroupingDeviceClasses_DeletionCleansUpOrphans(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	results := []GroupingResult{
		{NodeName: "node-1", Instances: []GroupingInstance{multiDeviceGroupingInstance(0)}},
	}
	require.NoError(t, manager.SyncGroupingDeviceClasses(context.Background(), results))

	classes, err := client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, classes.Items, "grouping DeviceClasses should exist after first sync")

	// Simulates the last grouping ConfigMap being deleted: no results at all.
	require.NoError(t, manager.SyncGroupingDeviceClasses(context.Background(), nil))

	classes, err = client.ResourceV1().DeviceClasses().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, classes.Items, "orphaned grouping DeviceClasses must be cleaned up when groupings disappear")
}

func TestDeviceClassManager_SyncDeviceClasses_PreservesForeignMetadata(t *testing.T) {
	// Covers the partition path (not just grouping), since publishDeviceClass
	// is shared by both SyncDeviceClasses and SyncGroupingDeviceClasses.
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	results := []PartitionResult{
		{
			NodeName: "node-1",
			Profile:  "test",
			Partitions: []PartitionDevice{
				{Name: "p-full", Type: PartitionFull, Profile: "test", DeviceCounts: map[string]int{"gpu.nvidia.com": 8}},
			},
		},
	}
	require.NoError(t, manager.SyncDeviceClasses(context.Background(), results))

	dc, err := client.ResourceV1().DeviceClasses().Get(context.Background(), "full", metav1.GetOptions{})
	require.NoError(t, err)
	if dc.Annotations == nil {
		dc.Annotations = map[string]string{}
	}
	dc.Annotations["example.com/owner"] = "team-infra"
	_, err = client.ResourceV1().DeviceClasses().Update(context.Background(), dc, metav1.UpdateOptions{})
	require.NoError(t, err)

	require.NoError(t, manager.SyncDeviceClasses(context.Background(), results))

	dc, err = client.ResourceV1().DeviceClasses().Get(context.Background(), "full", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "team-infra", dc.Annotations["example.com/owner"],
		"foreign annotation on a partition-path DeviceClass must survive reconciliation")
}

// TestDeviceClassManager_BuildPartitionConfig_AlignmentsOrderIsDeterministic
// covers config.Alignments ordering specifically. GetMatchConstraintRules
// ranges over TopologyRuleStore's internal map, so with 2+ ConstraintMatch
// rules configured, the order buildPartitionConfig appends Alignments in is
// random across calls. sortPartitionConfig must sort the Alignments slice
// itself (not just each Alignment's Requests) or the marshaled config keeps
// changing every reconcile, defeating publishDeviceClass's no-op check.
func TestDeviceClassManager_BuildPartitionConfig_AlignmentsOrderIsDeterministic(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	require.NoError(t, rules.LoadFromConfigMap(makeTopologyRuleConfigMap("pcie-root-rule", "default", map[string]string{
		"attribute":  AttrPCIeRoot,
		"type":       "string",
		"driver":     "gpu.nvidia.com",
		"constraint": "match",
	})))
	require.NoError(t, rules.LoadFromConfigMap(makeTopologyRuleConfigMap("socket-rule", "default", map[string]string{
		"attribute":  AttrSocket,
		"type":       "int",
		"driver":     "gpu.nvidia.com",
		"constraint": "match",
	})))

	pcieRoot := "pci0000:00"
	var socket int64
	representative := PartitionDevice{
		DeviceCounts: map[string]int{
			"gpu.nvidia.com":    1,
			"rdma.mellanox.com": 1,
		},
		Devices: []TopologyDevice{
			{DriverName: "gpu.nvidia.com", PCIeRoot: &pcieRoot, Socket: &socket},
			{DriverName: "rdma.mellanox.com", PCIeRoot: &pcieRoot, Socket: &socket},
		},
	}

	var firstRaw []byte
	for i := 0; i < 30; i++ {
		config, _ := manager.buildPartitionConfig(PartitionFull, representative)
		require.Len(t, config.Alignments, 2, "both match-constraint rules should produce an alignment")
		raw, err := json.Marshal(config)
		require.NoError(t, err)
		if i == 0 {
			firstRaw = raw
			continue
		}
		assert.Equal(t, string(firstRaw), string(raw),
			"buildPartitionConfig must marshal identically across calls with multiple match-constraint rules, iteration %d", i)
	}
}

func TestDeviceClassManager_SyncGroupingDeviceClasses_AggregateIntersectsAsymmetricRails(t *testing.T) {
	client := fake.NewSimpleClientset()
	rules := NewTopologyRuleStore()
	manager := NewDeviceClassManager(client, CoordinatorDriverName, rules)

	// Rail 0 has gpu+nic+nvme; rail 1 has only gpu, and with a lower count.
	// The aggregate must publish only what's guaranteed on every rail: gpu
	// at the minimum count (1), and nic/nvme excluded entirely since they're
	// not present on rail 1. Without intersection, the aggregate would
	// arbitrarily publish whichever rail's full config happened to "win" by
	// lowest InstanceIndex, misrepresenting rail 1's actual (lesser) capability.
	rail0 := multiDeviceGroupingInstance(0)
	rail1 := GroupingInstance{
		Name:         "node-1-gpu-nic-pair-pcieroot-1",
		NodeName:     "node-1",
		GroupingName: "gpu-nic-pair",
		Alignment:    "pcieRoot",
		PCIeRoots:    []string{"pci0000:01"},
		DeviceCounts: map[string]int{
			"gpu.nvidia.com": 2, // higher count than rail0's 1 -> min must win
		},
		Rail:          true,
		InstanceIndex: 1,
	}

	results := []GroupingResult{
		{NodeName: "node-1", Instances: []GroupingInstance{rail0, rail1}},
	}
	require.NoError(t, manager.SyncGroupingDeviceClasses(context.Background(), results))

	dc, err := client.ResourceV1().DeviceClasses().Get(context.Background(), "gpu-nic-pair", metav1.GetOptions{})
	require.NoError(t, err)

	var config PartitionConfig
	require.NoError(t, json.Unmarshal(dc.Spec.Config[0].Opaque.Parameters.Raw, &config))

	classes := make(map[string]SubResourceConfig, len(config.SubResources))
	for _, sr := range config.SubResources {
		classes[sr.DeviceClass] = sr
	}

	require.Contains(t, classes, "gpu.nvidia.com", "gpu is present on every rail, must survive intersection")
	assert.Equal(t, 1, classes["gpu.nvidia.com"].Count, "aggregate count must be the minimum across rails, not rail1's higher count")

	assert.NotContains(t, classes, "rdma.mellanox.com", "nic is absent from rail1, must not appear in the aggregate")
	assert.NotContains(t, classes, "nvme.example.com", "nvme is absent from rail1, must not appear in the aggregate")
}

func TestDeviceClassManager_AggregateCapacityIsMinimumAndDeterministic(t *testing.T) {
	// Two PCIe-root partitions under one aggregate key, same driver, different
	// capacity - exactly what partition_builder.go's
	// divideQuantity(capVal, numRoots) produces when a node's NUMA nodes have
	// different PCIe-root counts.
	//
	// Before the fix, the aggregate took whichever partition came last in map
	// iteration order, so the published capacity (and therefore Spec.Config,
	// and therefore publishDeviceClass's no-op check) varied between
	// reconciles, and could advertise 16 when one partition supplies only 8.
	results := func() []PartitionResult {
		return []PartitionResult{{
			NodeName: "node-1",
			Profile:  "test",
			Partitions: []PartitionDevice{
				{
					Name: "p-root0", Type: PartitionPCIeRoot, Profile: "test",
					DeviceCounts:   map[string]int{"dra.cpu": 1},
					DeviceCapacity: map[string]map[string]string{"dra.cpu": {"dra.cpu/cpu": "16"}},
					NUMANodes:      []int64{0},
					PCIeRoots:      []string{"pci0000:00"},
				},
				{
					Name: "p-root1", Type: PartitionPCIeRoot, Profile: "test",
					DeviceCounts:   map[string]int{"dra.cpu": 1},
					DeviceCapacity: map[string]map[string]string{"dra.cpu": {"dra.cpu/cpu": "8"}},
					NUMANodes:      []int64{1},
					PCIeRoots:      []string{"pci0000:80"},
				},
			},
		}}
	}

	observed := make(map[string]int)
	for i := 0; i < 40; i++ {
		client := fake.NewSimpleClientset()
		manager := NewDeviceClassManager(client, CoordinatorDriverName, NewTopologyRuleStore())
		require.NoError(t, manager.SyncDeviceClasses(context.Background(), results()))

		dc, err := client.ResourceV1().DeviceClasses().Get(context.Background(), "pcieroot", metav1.GetOptions{})
		require.NoError(t, err)

		var config PartitionConfig
		require.NoError(t, json.Unmarshal(dc.Spec.Config[0].Opaque.Parameters.Raw, &config))
		for _, sr := range config.SubResources {
			if sr.DeviceClass == "dra.cpu" {
				observed[sr.Capacity["dra.cpu/cpu"]]++
			}
		}
	}

	require.Len(t, observed, 1, "aggregate capacity must not vary with map iteration order, saw %v", observed)
	assert.Equal(t, 40, observed["8"],
		"aggregate must advertise the minimum capacity satisfiable on every partition, saw %v", observed)
}

func TestMergeMinCapacity(t *testing.T) {
	tests := []struct {
		name                     string
		existing, incoming, want map[string]string
	}{
		{
			name:     "per-key minimum",
			existing: map[string]string{"dra.cpu/cpu": "16"},
			incoming: map[string]string{"dra.cpu/cpu": "8"},
			want:     map[string]string{"dra.cpu/cpu": "8"},
		},
		{
			name:     "minimum regardless of argument order",
			existing: map[string]string{"dra.cpu/cpu": "8"},
			incoming: map[string]string{"dra.cpu/cpu": "16"},
			want:     map[string]string{"dra.cpu/cpu": "8"},
		},
		{
			name:     "compares by quantity value, not string",
			existing: map[string]string{"memory/mem": "1Gi"},
			incoming: map[string]string{"memory/mem": "900Mi"},
			want:     map[string]string{"memory/mem": "900Mi"},
		},
		{
			name:     "key missing on one side is dropped as unguaranteed",
			existing: map[string]string{"dra.cpu/cpu": "16", "dra.cpu/extra": "1"},
			incoming: map[string]string{"dra.cpu/cpu": "8"},
			want:     map[string]string{"dra.cpu/cpu": "8"},
		},
		{
			name:     "nil incoming means capacity is not guaranteed",
			existing: map[string]string{"dra.cpu/cpu": "16"},
			incoming: nil,
			want:     nil,
		},
		{
			name:     "nil existing means capacity is not guaranteed",
			existing: nil,
			incoming: map[string]string{"dra.cpu/cpu": "16"},
			want:     nil,
		},
		{
			name:     "no shared keys yields nil",
			existing: map[string]string{"a/x": "1"},
			incoming: map[string]string{"b/y": "2"},
			want:     nil,
		},
		{
			name:     "unparseable value on either side is dropped, not guessed",
			existing: map[string]string{"vendor/thing": "zzz"},
			incoming: map[string]string{"vendor/thing": "aaa"},
			want:     nil,
		},
		{
			name:     "unparseable value would otherwise sort before the larger valid-looking one",
			existing: map[string]string{"dra.cpu/cpu": "16"},
			incoming: map[string]string{"dra.cpu/cpu": "not-a-quantity"},
			want:     nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, mergeMinCapacity(tt.existing, tt.incoming))
		})
	}
}

func TestDeviceIntersectionState_DroppedCapacityKeyStaysDropped(t *testing.T) {
	// A capacity key correctly removed because one alternative lacked it
	// must not reappear just because a later alternative happens to declare
	// it again. Order matters here precisely because it shouldn't: the
	// result must be the same regardless of which alternative is "later".
	s := newDeviceIntersectionState()

	// Instance A: driver X with capacity {k: "10"}.
	s.add(map[string]int{"X": 1}, map[string]map[string]string{"X": {"k": "10"}}, nil)

	// Instance B: driver X present, but no capacity declared for it at all.
	s.add(map[string]int{"X": 1}, map[string]map[string]string{}, nil)

	_, cap := s.intersect(nil)
	require.NotContains(t, cap, "X", "capacity for X must already be dropped after instance B lacks it")

	// Instance C: driver X present again, with capacity {k: "5"}.
	s.add(map[string]int{"X": 1}, map[string]map[string]string{"X": {"k": "5"}}, nil)

	counts, cap := s.intersect(nil)
	assert.Equal(t, 1, counts["X"], "driver count is unaffected by the capacity gap")
	assert.NotContains(t, cap, "X",
		"capacity for X must stay dropped: instance B had none, so it is not guaranteed across every alternative")
}

func TestDeviceIntersectionState_DroppedCapacityKeyOrderIndependent(t *testing.T) {
	// Same scenario as above, but the gap is in the middle, confirming the
	// result doesn't depend on which position lacks the capacity.
	orders := [][]map[string]string{
		{{"k": "10"}, {}, {"k": "5"}}, // gap in the middle
		{{}, {"k": "10"}, {"k": "5"}}, // gap first
		{{"k": "10"}, {"k": "5"}, {}}, // gap last
	}
	for i, order := range orders {
		s := newDeviceIntersectionState()
		for _, cap := range order {
			capacity := map[string]map[string]string{}
			if len(cap) > 0 {
				capacity["X"] = cap
			}
			s.add(map[string]int{"X": 1}, capacity, nil)
		}
		_, cap := s.intersect(nil)
		assert.NotContains(t, cap, "X", "order %d: a gap anywhere in the fold must drop the key", i)
	}
}

func TestDeviceIntersectionState_UnparseableCapacityDropped(t *testing.T) {
	// An unparseable capacity value must drop the key, not fall back to a
	// lexical "minimum" that has no relationship to the actual quantity.
	s := newDeviceIntersectionState()
	s.add(map[string]int{"X": 1}, map[string]map[string]string{"X": {"k": "16"}}, nil)
	s.add(map[string]int{"X": 1}, map[string]map[string]string{"X": {"k": "not-a-quantity"}}, nil)

	counts, cap := s.intersect(nil)
	assert.Equal(t, 1, counts["X"], "driver count is unaffected by an unparseable capacity value")
	assert.NotContains(t, cap, "X", "an unparseable capacity value must not produce a fabricated minimum")
}

func requestDriverRules(t *testing.T, data ...map[string]string) *TopologyRuleStore {
	t.Helper()
	rules := NewTopologyRuleStore()
	for i, d := range data {
		require.NoError(t, rules.LoadFromConfigMap(makeTopologyRuleConfigMap(fmt.Sprintf("rule-%d", i), "default", d)))
	}
	return rules
}

func TestDeviceClassManager_NUMASelectorUsesRequestDriver(t *testing.T) {
	sourceRule := map[string]string{"attribute": "gpu.example.com/numaNode", "type": "int", "driver": "gpu.example.com", "mapsTo": "numaNode"}
	tests := []struct {
		name   string
		rules  []map[string]string
		driver string
		want   string
	}{
		{"request driver rule", []map[string]string{sourceRule, {"attribute": "vfio.example.com/numa", "type": "int", "driver": "vfio.example.com", "mapsTo": "numaNode"}}, "vfio.example.com", `device.attributes["vfio.example.com"].numa`},
		{"unqualified attribute gets request driver prefix", []map[string]string{sourceRule, {"attribute": "numaID", "type": "int", "driver": "vfio.example.com", "mapsTo": "numaNode"}}, "vfio.example.com", `device.attributes["vfio.example.com"].numaID`},
		{"no request driver rule falls back to standard attribute", []map[string]string{sourceRule}, "vfio.example.com", `device.attributes["resource.kubernetes.io"].numaNode`},
		{"empty request driver keeps source driver", []map[string]string{sourceRule}, "", `device.attributes["gpu.example.com"].numaNode`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := NewDeviceClassManager(fake.NewSimpleClientset(), CoordinatorDriverName, requestDriverRules(t, tt.rules...))
			config := manager.buildGroupingConfig(GroupingInstance{
				NUMANodes:    []int64{0},
				DeviceCounts: map[string]int{"gpu.example.com": 1},
				Requests: map[string]RequestContract{
					"gpu.example.com": {Class: "vfio.gpu.example.com", Driver: tt.driver},
				},
			})
			require.Len(t, config.SubResources, 1)
			require.Len(t, config.SubResources[0].Selectors, 1)
			assert.Contains(t, config.SubResources[0].Selectors[0], tt.want)
		})
	}
}

func TestDeviceClassManager_AlignmentSkipsRequestDriverWithoutAttribute(t *testing.T) {
	nvlink := int64(1)
	devices := func() []TopologyDevice {
		root := "pci0000:00"
		attrs := map[string]DeviceAttributeValue{"gpu.example.com/nvlink": {IntValue: &nvlink}}
		return []TopologyDevice{
			{DriverName: "gpu.example.com", DeviceName: "gpu-0", PCIeRoot: &root, ExtendedAttributes: attrs},
			{DriverName: "nic.example.com", DeviceName: "nic-0", PCIeRoot: &root, ExtendedAttributes: attrs},
		}
	}
	rules := requestDriverRules(t,
		map[string]string{"attribute": AttrPCIeRoot, "type": "string", "driver": "nic.example.com", "constraint": "match"},
		map[string]string{"attribute": "gpu.example.com/nvlink", "type": "int", "driver": "nic.example.com", "constraint": "match"},
	)
	manager := NewDeviceClassManager(fake.NewSimpleClientset(), CoordinatorDriverName, rules)
	build := func(driver string) PartitionConfig {
		return manager.buildGroupingConfig(GroupingInstance{
			Alignment:    "pcieRoot",
			Devices:      devices(),
			DeviceCounts: map[string]int{"gpu.example.com": 1, "nic.example.com": 1},
			Requests: map[string]RequestContract{
				"gpu.example.com": {Class: "vfio.gpu.example.com", Driver: driver},
			},
		})
	}
	attributes := func(c PartitionConfig) map[string][]string {
		out := map[string][]string{}
		for _, a := range c.Alignments {
			out[a.Attribute] = a.Requests
		}
		return out
	}

	same := attributes(build(""))
	assert.Len(t, same[AttrPCIeRoot], 2)
	assert.Len(t, same["gpu.example.com/nvlink"], 2)

	other := attributes(build("vfio.example.com"))
	assert.Len(t, other[AttrPCIeRoot], 2, "standard attributes carry over to the request driver")
	assert.NotContains(t, other, "gpu.example.com/nvlink", "driver-specific attribute is not assumed on the request driver")

	owned := attributes(manager.buildGroupingConfig(GroupingInstance{
		Alignment:    "pcieRoot",
		Devices:      devices(),
		DeviceCounts: map[string]int{"gpu.example.com": 1, "nic.example.com": 1},
		Requests: map[string]RequestContract{
			"gpu.example.com": {Class: "vfio.gpu.example.com", Driver: "gpu.example.com"},
		},
	}))
	assert.Len(t, owned["gpu.example.com/nvlink"], 2, "source driver keeps its own alignment")
}

func TestRequestDriverPublishes(t *testing.T) {
	tests := []struct {
		name                  string
		requestDriver, source string
		attribute             string
		want                  bool
	}{
		{"no request driver", "", "gpu.example.com", "gpu.example.com/nvlink", true},
		{"same driver", "gpu.example.com", "gpu.example.com", "gpu.example.com/nvlink", true},
		{"standard attribute carries over", "vfio.example.com", "gpu.example.com", AttrPCIeRoot, true},
		{"request driver owns the domain", "vfio.example.com", "gpu.example.com", "vfio.example.com/group", true},
		{"source-only domain", "vfio.example.com", "gpu.example.com", "gpu.example.com/nvlink", false},
		{"unqualified attribute", "vfio.example.com", "gpu.example.com", "nvlink", false},
		{"suffixed request driver owns the domain", "vfio.example.com/v2", "gpu.example.com", "vfio.example.com/group", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, requestDriverPublishes(tt.requestDriver, tt.source, tt.attribute))
		})
	}
}

func TestGroupingRequestContractKey_IncludesRequestDriver(t *testing.T) {
	a := GroupingInstance{Requests: map[string]RequestContract{"gpu.example.com": {Class: "v", Driver: "one.example.com"}}}
	b := GroupingInstance{Requests: map[string]RequestContract{"gpu.example.com": {Class: "v", Driver: "two.example.com"}}}
	assert.NotEqual(t, groupingRequestContractKey(a), groupingRequestContractKey(b))
}

func TestPartitionConfigValidate_AlignmentAcceptsLegacySourceDriver(t *testing.T) {
	config := PartitionConfig{
		Kind: "PartitionConfig",
		SubResources: []SubResourceConfig{
			{SourceDriver: "gpu.example.com", DeviceClass: "gpu-class", Count: 1},
			{SourceDriver: "nic.example.com", DeviceClass: "nic-class", Count: 1},
		},
		Alignments: []AlignmentConfig{{Attribute: "pcieRoot", Requests: []string{"gpu.example.com", "nic-class"}}},
	}
	require.NoError(t, config.Validate())

	config.Alignments[0].Requests = []string{"unknown"}
	require.ErrorContains(t, config.Validate(), "unknown deviceClass")
}

func TestDeviceClassManager_PublishedPartitionConfigsCarryAPIVersion(t *testing.T) {
	manager := NewDeviceClassManager(fake.NewSimpleClientset(), CoordinatorDriverName, NewTopologyRuleStore())
	inst := multiDeviceGroupingInstance(0)
	assert.Equal(t, PartitionConfigAPIVersion, manager.buildGroupingConfig(inst).APIVersion)
	assert.Equal(t, PartitionConfigAPIVersion, manager.buildGroupingAggregateConfig(inst).APIVersion)
	partition, _ := manager.buildPartitionConfig(PartitionPCIeRoot, PartitionDevice{DeviceCounts: map[string]int{"gpu.amd.com": 1}})
	assert.Equal(t, PartitionConfigAPIVersion, partition.APIVersion)
}
