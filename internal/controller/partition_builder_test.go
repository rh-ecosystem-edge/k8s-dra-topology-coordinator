package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// buildHGXB200Topology populates a topology model with an HGX B200-like node:
// 8 GPUs, 8 NICs, 2 sockets, 4 NUMA nodes, 8 PCIe roots
func buildHGXB200Topology(model *TopologyModel) {
	// GPU slice: 8 GPUs across 4 NUMA nodes
	gpuDevices := make([]resourcev1.Device, 8)
	pcieRoots := []string{"pcie-0", "pcie-1", "pcie-2", "pcie-3", "pcie-4", "pcie-5", "pcie-6", "pcie-7"}
	for i := 0; i < 8; i++ {
		numaNode := int64(i / 2) // 2 GPUs per NUMA
		socket := numaNode / 2   // 2 NUMA per socket
		gpuDevices[i] = resourcev1.Device{
			Name: makeDeviceName("gpu", i),
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				resourcev1.QualifiedName(AttrNUMANode): {IntValue: intPtr(numaNode)},
				resourcev1.QualifiedName(AttrPCIeRoot): {StringValue: strPtr(pcieRoots[i])},
				resourcev1.QualifiedName(AttrSocket):   {IntValue: intPtr(socket)},
			},
		}
	}
	gpuSlice := makeResourceSlice("gpu-slice", "gpu.nvidia.com", "hgx-node-1", "gpu-pool", gpuDevices)
	model.UpdateFromResourceSlice(gpuSlice)

	// NIC slice: 8 NICs across 4 NUMA nodes, same PCIe roots
	nicDevices := make([]resourcev1.Device, 8)
	for i := 0; i < 8; i++ {
		numaNode := int64(i / 2)
		socket := numaNode / 2
		nicDevices[i] = resourcev1.Device{
			Name: makeDeviceName("nic", i),
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				resourcev1.QualifiedName(AttrNUMANode): {IntValue: intPtr(numaNode)},
				resourcev1.QualifiedName(AttrPCIeRoot): {StringValue: strPtr(pcieRoots[i])},
				resourcev1.QualifiedName(AttrSocket):   {IntValue: intPtr(socket)},
			},
		}
	}
	nicSlice := makeResourceSlice("nic-slice", "rdma.mellanox.com", "hgx-node-1", "nic-pool", nicDevices)
	model.UpdateFromResourceSlice(nicSlice)
}

func makeDeviceName(prefix string, index int) string {
	return prefix + "-" + string(rune('0'+index))
}

func TestPartitionBuilder_HGXScenario(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()
	builder := NewPartitionBuilder(model, rules)

	buildHGXB200Topology(model)

	results := builder.BuildPartitions()
	require.Len(t, results, 1, "should have partitions for exactly one node")

	result := results[0]
	assert.Equal(t, "hgx-node-1", result.NodeName)
	assert.NotEmpty(t, result.Profile)

	// Count partition types
	counts := make(map[PartitionType]int)
	for _, p := range result.Partitions {
		counts[p.Type]++
	}

	// pcieRoot/numa/full model: 8 PCIe roots, 4 NUMA nodes, 1 full
	assert.Equal(t, 8, counts[PartitionPCIeRoot], "expected 8 pcieRoot partitions (one per PCIe root)")
	assert.Equal(t, 4, counts[PartitionNUMA], "expected 4 NUMA partitions (one per NUMA node)")
	assert.Equal(t, 1, counts[PartitionFull], "expected 1 full partition")
}

func TestPartitionBuilder_DoesNotCombineDriversAcrossNodes(t *testing.T) {
	model := NewTopologyModel()
	builder := NewPartitionBuilder(model, NewTopologyRuleStore())

	model.UpdateFromResourceSlice(makeResourceSlice("gpu", "gpu.nvidia.com", "gpu-node", "gpu-pool", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
	}))
	model.UpdateFromResourceSlice(makeResourceSlice("cpu", "dra.cpu", "cpu-node", "cpu-pool", []resourcev1.Device{{
		Name: "cpu-0",
		Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
			resourcev1.QualifiedName(AttrNUMANode): {IntValue: intPtr(0)},
		},
		Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
			"dra.cpu/cpu": {Value: resource.MustParse("32")},
		},
	}}))

	results := builder.BuildPartitions()
	require.Len(t, results, 2)
	profiles := map[string]bool{}
	for _, result := range results {
		profiles[result.Profile] = true
	}
	assert.Equal(t, map[string]bool{"gpu.nvidia.com": true, "dra.cpu": true}, profiles)
}

func TestPartitionBuilder_EighthPartitionContents(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()
	builder := NewPartitionBuilder(model, rules)

	buildHGXB200Topology(model)

	results := builder.BuildPartitions()
	require.Len(t, results, 1)

	// Each pcieRoot partition should have 1 GPU + 1 NIC and a single NUMA node
	for _, p := range results[0].Partitions {
		if p.Type == PartitionPCIeRoot {
			totalDevices := 0
			for _, count := range p.DeviceCounts {
				totalDevices += count
			}
			assert.Equal(t, 2, totalDevices,
				"pcieRoot partition %s should have 2 device counts (1 GPU + 1 NIC)", p.Name)
			assert.Len(t, p.NUMANodes, 1, "pcieRoot partition should have exactly 1 NUMA node")
		}
	}
}

func TestPartitionBuilder_NUMAPartitionContents(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()
	builder := NewPartitionBuilder(model, rules)

	buildHGXB200Topology(model)

	results := builder.BuildPartitions()
	require.Len(t, results, 1)

	// NUMA partition = one NUMA node: 2 GPUs + 2 NICs = 4 device counts
	for _, p := range results[0].Partitions {
		if p.Type == PartitionNUMA {
			totalDevices := 0
			for _, count := range p.DeviceCounts {
				totalDevices += count
			}
			assert.Equal(t, 4, totalDevices,
				"NUMA partition %s should have 4 device counts (2 GPU + 2 NIC)", p.Name)
			assert.Len(t, p.NUMANodes, 1, "NUMA partition should have exactly 1 NUMA node")
		}
	}
}

func TestPartitionBuilder_FullPartitionContents(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()
	builder := NewPartitionBuilder(model, rules)

	buildHGXB200Topology(model)

	results := builder.BuildPartitions()
	require.Len(t, results, 1)

	for _, p := range results[0].Partitions {
		if p.Type == PartitionFull {
			totalDevices := 0
			for _, count := range p.DeviceCounts {
				totalDevices += count
			}
			assert.Equal(t, 16, totalDevices,
				"full partition should contain 16 devices (8 GPU + 8 NIC)")
		}
	}
}

func TestPartitionBuilder_WithNVLinkGrouping(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()

	// Add NVLink topology rule
	err := rules.LoadFromConfigMap(makeTopologyRuleConfigMap("nvlink", "default", map[string]string{
		"attribute":    "gpu.nvidia.com/nvlinkDomain",
		"type":         "int",
		"driver":       "gpu.nvidia.com",
		"partitioning": "group",
		"constraint":   "match",
	}))
	require.NoError(t, err)

	// Set rules in the model so extraction works
	model.SetRules(rules.GetRules())

	// Create 4 GPUs: 2 in NVLink domain 0 on NUMA 0, 2 in domain 1 on NUMA 1
	gpuDevices := []resourcev1.Device{
		{
			Name: "gpu-0",
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				resourcev1.QualifiedName(AttrNUMANode):                  {IntValue: intPtr(0)},
				resourcev1.QualifiedName(AttrPCIeRoot):                  {StringValue: strPtr("pcie-0")},
				resourcev1.QualifiedName(AttrSocket):                    {IntValue: intPtr(0)},
				resourcev1.QualifiedName("gpu.nvidia.com/nvlinkDomain"): {IntValue: intPtr(0)},
			},
		},
		{
			Name: "gpu-1",
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				resourcev1.QualifiedName(AttrNUMANode):                  {IntValue: intPtr(0)},
				resourcev1.QualifiedName(AttrPCIeRoot):                  {StringValue: strPtr("pcie-1")},
				resourcev1.QualifiedName(AttrSocket):                    {IntValue: intPtr(0)},
				resourcev1.QualifiedName("gpu.nvidia.com/nvlinkDomain"): {IntValue: intPtr(0)},
			},
		},
		{
			Name: "gpu-2",
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				resourcev1.QualifiedName(AttrNUMANode):                  {IntValue: intPtr(1)},
				resourcev1.QualifiedName(AttrPCIeRoot):                  {StringValue: strPtr("pcie-2")},
				resourcev1.QualifiedName(AttrSocket):                    {IntValue: intPtr(1)},
				resourcev1.QualifiedName("gpu.nvidia.com/nvlinkDomain"): {IntValue: intPtr(1)},
			},
		},
		{
			Name: "gpu-3",
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				resourcev1.QualifiedName(AttrNUMANode):                  {IntValue: intPtr(1)},
				resourcev1.QualifiedName(AttrPCIeRoot):                  {StringValue: strPtr("pcie-3")},
				resourcev1.QualifiedName(AttrSocket):                    {IntValue: intPtr(1)},
				resourcev1.QualifiedName("gpu.nvidia.com/nvlinkDomain"): {IntValue: intPtr(1)},
			},
		},
	}

	gpuSlice := makeResourceSlice("gpu-slice", "gpu.nvidia.com", "node-1", "gpu-pool", gpuDevices)
	model.UpdateFromResourceSlice(gpuSlice)

	builder := NewPartitionBuilder(model, rules)
	results := builder.BuildPartitions()
	require.Len(t, results, 1)

	// Verify that devices in each partition share the same NVLink domain
	for _, p := range results[0].Partitions {
		if p.Type == PartitionPCIeRoot || p.Type == PartitionNUMA {
			// All GPU devices in the partition should have the same nvlinkDomain
			var domain *int64
			for _, d := range p.Devices {
				if val, ok := d.ExtendedAttributes["gpu.nvidia.com/nvlinkDomain"]; ok {
					if domain == nil {
						domain = val.IntValue
					} else if val.IntValue != nil {
						assert.Equal(t, *domain, *val.IntValue,
							"all GPUs in partition %s should share the same NVLink domain", p.Name)
					}
				}
			}
		}
	}
}

func TestPartitionBuilder_EmptyModel(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()
	builder := NewPartitionBuilder(model, rules)

	results := builder.BuildPartitions()
	assert.Empty(t, results)
}

func TestPartitionBuilder_SingleDeviceNode(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()

	// Single GPU on a node
	model.UpdateFromResourceSlice(makeResourceSlice("gpu-slice", "gpu.nvidia.com", "node-1", "gpu-pool", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
	}))

	builder := NewPartitionBuilder(model, rules)
	results := builder.BuildPartitions()
	require.Len(t, results, 1)

	// Should have only a full partition (no subdivisions possible with 1 group per topology level)
	counts := make(map[PartitionType]int)
	for _, p := range results[0].Partitions {
		counts[p.Type]++
	}
	assert.Equal(t, 1, counts[PartitionFull], "should have 1 full partition")
	// Single device on one PCIe root produces 1 pcieRoot partition.
	// NUMA partition is skipped (only 1 NUMA node = would duplicate full).
	assert.Equal(t, 1, counts[PartitionPCIeRoot], "should have 1 pcieRoot partition")
	assert.Equal(t, 0, counts[PartitionNUMA], "should have no NUMA partitions with only 1 NUMA node")
}

func TestPartitionBuilder_MultipleNodes(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()

	// Node 1: 2 GPUs on different NUMA
	model.UpdateFromResourceSlice(makeResourceSlice("gpu-slice-1", "gpu.nvidia.com", "node-1", "gpu-pool-1", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
		makeGPUDevice("gpu-1", 1, "pcie-1"),
	}))

	// Node 2: 2 GPUs on different NUMA
	model.UpdateFromResourceSlice(makeResourceSlice("gpu-slice-2", "gpu.nvidia.com", "node-2", "gpu-pool-2", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
		makeGPUDevice("gpu-1", 1, "pcie-1"),
	}))

	builder := NewPartitionBuilder(model, rules)
	results := builder.BuildPartitions()
	assert.Len(t, results, 2, "should have partition results for 2 nodes")
}

func TestPartitionBuilder_TopologyRecomputation(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()

	// Initial: 2 GPUs
	model.UpdateFromResourceSlice(makeResourceSlice("gpu-slice", "gpu.nvidia.com", "node-1", "gpu-pool", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
		makeGPUDevice("gpu-1", 1, "pcie-1"),
	}))

	builder := NewPartitionBuilder(model, rules)
	results := builder.BuildPartitions()
	require.Len(t, results, 1)

	initialPartitionCount := len(results[0].Partitions)

	// Add a third GPU
	model.UpdateFromResourceSlice(makeResourceSlice("gpu-slice", "gpu.nvidia.com", "node-1", "gpu-pool", []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
		makeGPUDevice("gpu-1", 1, "pcie-1"),
		makeGPUDevice("gpu-2", 2, "pcie-2"),
	}))

	results = builder.BuildPartitions()
	require.Len(t, results, 1)

	// Should have more partitions now due to additional topology groups
	newPartitionCount := len(results[0].Partitions)
	assert.Greater(t, newPartitionCount, initialPartitionCount,
		"adding a device on a new NUMA node should increase partition count")
}

// buildMI355XPartitionableTopology populates a model with 2 AMD MI355X GPUs
// on NUMA 0, each advertised as SPX + 2 DPX = 3 devices (6 total for 2 physical GPUs),
// plus 2 NICs on NUMA 0.
func buildMI355XPartitionableTopology(model *TopologyModel) {
	gpuDevices := []resourcev1.Device{
		// GPU 0: SPX + 2 DPX
		{
			Name: "gpu-0-spx",
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				resourcev1.QualifiedName(AttrNUMANode): {IntValue: intPtr(0)},
				resourcev1.QualifiedName(AttrPCIeRoot): {StringValue: strPtr("pcie-0")},
				resourcev1.QualifiedName(AttrSocket):   {IntValue: intPtr(0)},
			},
			ConsumesCounters: []resourcev1.DeviceCounterConsumption{
				{CounterSet: "gpu-0-counters", Counters: map[string]resourcev1.Counter{
					"xcds": {Value: *resource.NewQuantity(8, resource.DecimalSI)},
				}},
			},
		},
		{
			Name: "gpu-0-dpx-0",
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				resourcev1.QualifiedName(AttrNUMANode): {IntValue: intPtr(0)},
				resourcev1.QualifiedName(AttrPCIeRoot): {StringValue: strPtr("pcie-0")},
				resourcev1.QualifiedName(AttrSocket):   {IntValue: intPtr(0)},
			},
			ConsumesCounters: []resourcev1.DeviceCounterConsumption{
				{CounterSet: "gpu-0-counters", Counters: map[string]resourcev1.Counter{
					"xcds": {Value: *resource.NewQuantity(4, resource.DecimalSI)},
				}},
			},
		},
		{
			Name: "gpu-0-dpx-1",
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				resourcev1.QualifiedName(AttrNUMANode): {IntValue: intPtr(0)},
				resourcev1.QualifiedName(AttrPCIeRoot): {StringValue: strPtr("pcie-0")},
				resourcev1.QualifiedName(AttrSocket):   {IntValue: intPtr(0)},
			},
			ConsumesCounters: []resourcev1.DeviceCounterConsumption{
				{CounterSet: "gpu-0-counters", Counters: map[string]resourcev1.Counter{
					"xcds": {Value: *resource.NewQuantity(4, resource.DecimalSI)},
				}},
			},
		},
		// GPU 1: SPX + 2 DPX
		{
			Name: "gpu-1-spx",
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				resourcev1.QualifiedName(AttrNUMANode): {IntValue: intPtr(0)},
				resourcev1.QualifiedName(AttrPCIeRoot): {StringValue: strPtr("pcie-1")},
				resourcev1.QualifiedName(AttrSocket):   {IntValue: intPtr(0)},
			},
			ConsumesCounters: []resourcev1.DeviceCounterConsumption{
				{CounterSet: "gpu-1-counters", Counters: map[string]resourcev1.Counter{
					"xcds": {Value: *resource.NewQuantity(8, resource.DecimalSI)},
				}},
			},
		},
		{
			Name: "gpu-1-dpx-0",
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				resourcev1.QualifiedName(AttrNUMANode): {IntValue: intPtr(0)},
				resourcev1.QualifiedName(AttrPCIeRoot): {StringValue: strPtr("pcie-1")},
				resourcev1.QualifiedName(AttrSocket):   {IntValue: intPtr(0)},
			},
			ConsumesCounters: []resourcev1.DeviceCounterConsumption{
				{CounterSet: "gpu-1-counters", Counters: map[string]resourcev1.Counter{
					"xcds": {Value: *resource.NewQuantity(4, resource.DecimalSI)},
				}},
			},
		},
		{
			Name: "gpu-1-dpx-1",
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				resourcev1.QualifiedName(AttrNUMANode): {IntValue: intPtr(0)},
				resourcev1.QualifiedName(AttrPCIeRoot): {StringValue: strPtr("pcie-1")},
				resourcev1.QualifiedName(AttrSocket):   {IntValue: intPtr(0)},
			},
			ConsumesCounters: []resourcev1.DeviceCounterConsumption{
				{CounterSet: "gpu-1-counters", Counters: map[string]resourcev1.Counter{
					"xcds": {Value: *resource.NewQuantity(4, resource.DecimalSI)},
				}},
			},
		},
	}
	gpuSlice := makeResourceSlice("gpu-slice", "gpu.amd.com", "node-1", "gpu-pool", gpuDevices)
	model.UpdateFromResourceSlice(gpuSlice)

	// 2 NICs on same NUMA, different PCIe roots
	nicDevices := []resourcev1.Device{
		makeNICDevice("nic-0", 0, "pcie-0"),
		makeNICDevice("nic-1", 0, "pcie-1"),
	}
	nicSlice := makeResourceSlice("nic-slice", "rdma.mellanox.com", "node-1", "nic-pool", nicDevices)
	model.UpdateFromResourceSlice(nicSlice)
}

func TestPartitionBuilder_PartitionableDevices(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()
	builder := NewPartitionBuilder(model, rules)

	buildMI355XPartitionableTopology(model)

	results := builder.BuildPartitions()
	require.Len(t, results, 1)

	result := results[0]

	// The full partition should report 2 effective GPUs, not 6
	for _, p := range result.Partitions {
		if p.Type == PartitionFull {
			gpuCount := p.DeviceCounts["gpu.amd.com"]
			assert.Equal(t, 2, gpuCount,
				"full partition should have 2 effective GPUs (not 6 advertised)")
			nicCount := p.DeviceCounts["rdma.mellanox.com"]
			assert.Equal(t, 2, nicCount,
				"full partition should have 2 NICs")
		}
	}
}

func TestPartitionBuilder_PartitionableDevicesEighths(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()
	builder := NewPartitionBuilder(model, rules)

	buildMI355XPartitionableTopology(model)

	results := builder.BuildPartitions()
	require.Len(t, results, 1)

	// With 2 GPUs on 2 PCIe roots on 1 NUMA, pcieRoot partitions should
	// each have 1 effective GPU + 1 NIC
	for _, p := range results[0].Partitions {
		if p.Type == PartitionPCIeRoot {
			gpuCount := p.DeviceCounts["gpu.amd.com"]
			assert.Equal(t, 1, gpuCount,
				"pcieRoot partition %s should have 1 effective GPU", p.Name)
			nicCount := p.DeviceCounts["rdma.mellanox.com"]
			assert.Equal(t, 1, nicCount,
				"pcieRoot partition %s should have 1 NIC", p.Name)
		}
	}
}

func TestDivideQuantity(t *testing.T) {
	tests := []struct {
		name    string
		qty     string
		divisor int
		want    string
	}{
		{"normal", "64", 4, "16"},
		{"one", "32", 1, "32"},
		{"zero divisor", "64", 0, ""},
		{"unparseable", "notanumber", 4, ""},
		{"remainder truncated", "10", 3, "3"},
		{"result zero", "1", 4, ""},
		{"memory quantity", "8Gi", 4, "2Gi"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := divideQuantity(tt.qty, tt.divisor)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDetectPCIeRootPairings_MultiDriver(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()
	builder := NewPartitionBuilder(model, rules)

	gpuDevices := []resourcev1.Device{makeGPUDevice("gpu-0", 0, "pcie-0")}
	model.UpdateFromResourceSlice(makeResourceSlice("gpu-slice", "gpu.nvidia.com", "node-1", "gpu-pool", gpuDevices))

	nicDevices := []resourcev1.Device{makeNICDevice("nic-0", 0, "pcie-0")}
	model.UpdateFromResourceSlice(makeResourceSlice("nic-slice", "rdma.mellanox.com", "node-1", "nic-pool", nicDevices))

	groupings := builder.DetectPCIeRootPairings()
	assert.Len(t, groupings, 1)
	assert.Equal(t, "pcieRoot", groupings[0].Alignment)
	assert.Equal(t, "numaNode", groupings[0].Fallback)
	assert.Len(t, groupings[0].Devices, 2)
}

func TestDetectPCIeRootPairings_SingleDriver(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()
	builder := NewPartitionBuilder(model, rules)

	gpuDevices := []resourcev1.Device{
		makeGPUDevice("gpu-0", 0, "pcie-0"),
		makeGPUDevice("gpu-1", 0, "pcie-1"),
	}
	model.UpdateFromResourceSlice(makeResourceSlice("gpu-slice", "gpu.nvidia.com", "node-1", "gpu-pool", gpuDevices))

	groupings := builder.DetectPCIeRootPairings()
	assert.Empty(t, groupings)
}

func TestDetectPCIeRootPairings_ExcludesCapacityOnly(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()
	builder := NewPartitionBuilder(model, rules)

	gpuDevices := []resourcev1.Device{makeGPUDevice("gpu-0", 0, "pcie-0")}
	model.UpdateFromResourceSlice(makeResourceSlice("gpu", "gpu.nvidia.com", "node-1", "gpu-pool", gpuDevices))

	cpuDevices := []resourcev1.Device{
		{
			Name: "cpu-0",
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				resourcev1.QualifiedName(AttrNUMANode): {IntValue: intPtr(0)},
			},
			Capacity: map[resourcev1.QualifiedName]resourcev1.DeviceCapacity{
				"dra.cpu/cpu": {Value: resource.MustParse("32")},
			},
		},
	}
	model.UpdateFromResourceSlice(makeResourceSlice("cpu", "dra.cpu", "node-1", "cpu-pool", cpuDevices))

	groupings := builder.DetectPCIeRootPairings()
	assert.Empty(t, groupings, "capacity-only driver without pcieRoot should not produce pairings")
}

func TestDetectPCIeRootPairings_ThreeDrivers(t *testing.T) {
	model := NewTopologyModel()
	rules := NewTopologyRuleStore()
	builder := NewPartitionBuilder(model, rules)

	for _, d := range []struct{ driver, dev string }{
		{"gpu.nvidia.com", "gpu-0"},
		{"rdma.mellanox.com", "nic-0"},
		{"dra.nvme", "nvme-0"},
	} {
		devices := []resourcev1.Device{
			{
				Name: d.dev,
				Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
					resourcev1.QualifiedName(AttrNUMANode): {IntValue: intPtr(0)},
					resourcev1.QualifiedName(AttrPCIeRoot): {StringValue: strPtr("pcie-0")},
				},
			},
		}
		model.UpdateFromResourceSlice(makeResourceSlice(d.driver+"-slice", d.driver, "node-1", d.driver+"-pool", devices))
	}

	groupings := builder.DetectPCIeRootPairings()
	assert.Len(t, groupings, 3, "three drivers should produce 3 pairwise groupings")
}

func TestBaseDriverName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"gpu.nvidia.com", "gpu.nvidia.com"},
		{"gpu.nvidia.com/gpu-pool", "gpu.nvidia.com"},
		{"simple", "simple"},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, baseDriverName(tt.input))
	}
}
