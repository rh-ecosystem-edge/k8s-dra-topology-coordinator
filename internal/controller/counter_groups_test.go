package controller

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func makeCounter(value int64) resourcev1.Counter {
	return resourcev1.Counter{
		Value: *resource.NewQuantity(value, resource.DecimalSI),
	}
}

func makePartitionableGPUDevice(name string, numaNode int64, pcieRoot string, counterSet string, counters map[string]int64) TopologyDevice {
	consumptions := []resourcev1.DeviceCounterConsumption{
		{
			CounterSet: counterSet,
			Counters:   make(map[string]resourcev1.Counter),
		},
	}
	for k, v := range counters {
		consumptions[0].Counters[k] = makeCounter(v)
	}

	return TopologyDevice{
		DriverName:         "gpu.amd.com",
		DeviceName:         name,
		NodeName:           "node-1",
		PoolName:           "gpu-pool",
		NUMANode:           intPtr(numaNode),
		PCIeRoot:           strPtr(pcieRoot),
		ConsumesCounters:   consumptions,
		ExtendedAttributes: map[string]DeviceAttributeValue{},
	}
}

func TestGroupOverlappingDevices_NoCounters(t *testing.T) {
	devices := []TopologyDevice{
		{DriverName: "gpu.nvidia.com", DeviceName: "gpu-0", ExtendedAttributes: map[string]DeviceAttributeValue{}},
		{DriverName: "gpu.nvidia.com", DeviceName: "gpu-1", ExtendedAttributes: map[string]DeviceAttributeValue{}},
	}

	nonOverlapping, groups := GroupOverlappingDevices(devices)
	assert.Len(t, nonOverlapping, 2)
	assert.Empty(t, groups)
}

func TestGroupOverlappingDevices_SingleGPUWithPartitions(t *testing.T) {
	// Simulate one AMD MI355X GPU: 1 SPX + 2 DPX + 8 CPX = 11 advertised devices
	devices := []TopologyDevice{
		makePartitionableGPUDevice("gpu-0-spx", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 8, "memory": 288}),
		makePartitionableGPUDevice("gpu-0-dpx-0", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 4, "memory": 144}),
		makePartitionableGPUDevice("gpu-0-dpx-1", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 4, "memory": 144}),
		makePartitionableGPUDevice("gpu-0-cpx-0", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 1, "memory": 36}),
		makePartitionableGPUDevice("gpu-0-cpx-1", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 1, "memory": 36}),
		makePartitionableGPUDevice("gpu-0-cpx-2", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 1, "memory": 36}),
		makePartitionableGPUDevice("gpu-0-cpx-3", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 1, "memory": 36}),
		makePartitionableGPUDevice("gpu-0-cpx-4", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 1, "memory": 36}),
		makePartitionableGPUDevice("gpu-0-cpx-5", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 1, "memory": 36}),
		makePartitionableGPUDevice("gpu-0-cpx-6", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 1, "memory": 36}),
		makePartitionableGPUDevice("gpu-0-cpx-7", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 1, "memory": 36}),
	}

	nonOverlapping, groups := GroupOverlappingDevices(devices)

	assert.Empty(t, nonOverlapping, "all devices have ConsumesCounters")
	require.Len(t, groups, 1, "all 11 devices share one counter set → one group")
	assert.Equal(t, 1, groups[0].MaxEffective, "one physical GPU = 1 effective device")
	assert.Len(t, groups[0].Devices, 11)
}

func TestGroupOverlappingDevices_MultipleGPUs(t *testing.T) {
	// 2 physical GPUs, each with SPX + 2 DPX
	devices := []TopologyDevice{
		makePartitionableGPUDevice("gpu-0-spx", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 8}),
		makePartitionableGPUDevice("gpu-0-dpx-0", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 4}),
		makePartitionableGPUDevice("gpu-0-dpx-1", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 4}),

		makePartitionableGPUDevice("gpu-1-spx", 1, "pcie-1", "gpu-1-counters", map[string]int64{"xcds": 8}),
		makePartitionableGPUDevice("gpu-1-dpx-0", 1, "pcie-1", "gpu-1-counters", map[string]int64{"xcds": 4}),
		makePartitionableGPUDevice("gpu-1-dpx-1", 1, "pcie-1", "gpu-1-counters", map[string]int64{"xcds": 4}),
	}

	nonOverlapping, groups := GroupOverlappingDevices(devices)

	assert.Empty(t, nonOverlapping)
	require.Len(t, groups, 2, "2 distinct counter sets → 2 groups")
	assert.Equal(t, 1, groups[0].MaxEffective)
	assert.Equal(t, 1, groups[1].MaxEffective)
}

func TestGroupOverlappingDevices_MixedDrivers(t *testing.T) {
	// One partitionable GPU + one non-partitionable NIC
	devices := []TopologyDevice{
		makePartitionableGPUDevice("gpu-0-spx", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 8}),
		makePartitionableGPUDevice("gpu-0-dpx-0", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 4}),
		{
			DriverName:         "rdma.mellanox.com",
			DeviceName:         "nic-0",
			NodeName:           "node-1",
			PoolName:           "nic-pool",
			NUMANode:           intPtr(0),
			PCIeRoot:           strPtr("pcie-0"),
			ExtendedAttributes: map[string]DeviceAttributeValue{},
		},
	}

	nonOverlapping, groups := GroupOverlappingDevices(devices)

	assert.Len(t, nonOverlapping, 1, "NIC has no ConsumesCounters")
	assert.Equal(t, "rdma.mellanox.com", nonOverlapping[0].DriverName)
	require.Len(t, groups, 1, "GPUs share one counter set")
	assert.Equal(t, 1, groups[0].MaxEffective)
}

func TestEffectiveDeviceCount_NoCounters(t *testing.T) {
	devices := []TopologyDevice{
		{DriverName: "gpu.nvidia.com", DeviceName: "gpu-0", ExtendedAttributes: map[string]DeviceAttributeValue{}},
		{DriverName: "gpu.nvidia.com", DeviceName: "gpu-1", ExtendedAttributes: map[string]DeviceAttributeValue{}},
		{DriverName: "rdma.mellanox.com", DeviceName: "nic-0", ExtendedAttributes: map[string]DeviceAttributeValue{}},
	}

	counts := EffectiveDeviceCount(devices)
	assert.Equal(t, 2, counts["gpu.nvidia.com"])
	assert.Equal(t, 1, counts["rdma.mellanox.com"])
}

func TestEffectiveDeviceCount_WithPartitions(t *testing.T) {
	// 2 physical GPUs (each SPX + 2 DPX = 3 advertised) + 2 NICs
	devices := []TopologyDevice{
		makePartitionableGPUDevice("gpu-0-spx", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 8}),
		makePartitionableGPUDevice("gpu-0-dpx-0", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 4}),
		makePartitionableGPUDevice("gpu-0-dpx-1", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 4}),

		makePartitionableGPUDevice("gpu-1-spx", 1, "pcie-1", "gpu-1-counters", map[string]int64{"xcds": 8}),
		makePartitionableGPUDevice("gpu-1-dpx-0", 1, "pcie-1", "gpu-1-counters", map[string]int64{"xcds": 4}),
		makePartitionableGPUDevice("gpu-1-dpx-1", 1, "pcie-1", "gpu-1-counters", map[string]int64{"xcds": 4}),

		{DriverName: "rdma.mellanox.com", DeviceName: "nic-0", ExtendedAttributes: map[string]DeviceAttributeValue{}},
		{DriverName: "rdma.mellanox.com", DeviceName: "nic-1", ExtendedAttributes: map[string]DeviceAttributeValue{}},
	}

	counts := EffectiveDeviceCount(devices)
	assert.Equal(t, 2, counts["gpu.amd.com"], "6 advertised GPU devices → 2 effective (2 physical GPUs)")
	assert.Equal(t, 2, counts["rdma.mellanox.com"], "2 NICs unchanged")
}

func TestFilterToEffectiveDevices(t *testing.T) {
	devices := []TopologyDevice{
		makePartitionableGPUDevice("gpu-0-spx", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 8, "memory": 288}),
		makePartitionableGPUDevice("gpu-0-dpx-0", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 4, "memory": 144}),
		makePartitionableGPUDevice("gpu-0-dpx-1", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 4, "memory": 144}),
		{DriverName: "rdma.mellanox.com", DeviceName: "nic-0", NUMANode: intPtr(0), ExtendedAttributes: map[string]DeviceAttributeValue{}},
	}

	effective := FilterToEffectiveDevices(devices)
	assert.Len(t, effective, 2, "1 representative GPU + 1 NIC")

	// The representative should be the SPX (highest counter consumption)
	var gpuDevice TopologyDevice
	for _, d := range effective {
		if d.DriverName == "gpu.amd.com" {
			gpuDevice = d
		}
	}
	assert.Equal(t, "gpu-0-spx", gpuDevice.DeviceName, "SPX should be selected as representative")
}

func TestSmallestPartitionDevices(t *testing.T) {
	devices := []TopologyDevice{
		makePartitionableGPUDevice("gpu-0-spx", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 8}),
		makePartitionableGPUDevice("gpu-0-dpx-0", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 4}),
		makePartitionableGPUDevice("gpu-0-dpx-1", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 4}),
		makePartitionableGPUDevice("gpu-0-cpx-0", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 1}),
		makePartitionableGPUDevice("gpu-0-cpx-1", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 1}),
		{DriverName: "rdma.mellanox.com", DeviceName: "nic-0", ExtendedAttributes: map[string]DeviceAttributeValue{}},
	}

	smallest := SmallestPartitionDevices(devices)

	// Should get all CPX devices (smallest) + NIC
	assert.Len(t, smallest, 3, "2 CPX devices + 1 NIC")

	var gpuNames []string
	for _, d := range smallest {
		if d.DriverName == "gpu.amd.com" {
			gpuNames = append(gpuNames, d.DeviceName)
		}
	}
	assert.Contains(t, gpuNames, "gpu-0-cpx-0")
	assert.Contains(t, gpuNames, "gpu-0-cpx-1")
}

func TestTotalCounterConsumption(t *testing.T) {
	consumptions := []resourcev1.DeviceCounterConsumption{
		{
			CounterSet: "gpu-0-counters",
			Counters: map[string]resourcev1.Counter{
				"xcds":   makeCounter(4),
				"memory": makeCounter(144),
			},
		},
	}

	total := totalCounterConsumption(consumptions)
	assert.Equal(t, int64(148), total)
}

func TestSelectRepresentative_SingleDevice(t *testing.T) {
	devices := []TopologyDevice{
		makePartitionableGPUDevice("gpu-0-spx", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 8}),
	}

	rep := selectRepresentative(devices)
	assert.Equal(t, "gpu-0-spx", rep.DeviceName)
}

func TestSelectRepresentative_PicksLargest(t *testing.T) {
	devices := []TopologyDevice{
		makePartitionableGPUDevice("gpu-0-cpx-0", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 1}),
		makePartitionableGPUDevice("gpu-0-spx", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 8}),
		makePartitionableGPUDevice("gpu-0-dpx-0", 0, "pcie-0", "gpu-0-counters", map[string]int64{"xcds": 4}),
	}

	rep := selectRepresentative(devices)
	assert.Equal(t, "gpu-0-spx", rep.DeviceName, "should pick device with highest counter consumption")
}

func TestGroupOverlappingDevices_EmptyInput(t *testing.T) {
	nonOverlapping, groups := GroupOverlappingDevices(nil)
	assert.Empty(t, nonOverlapping)
	assert.Empty(t, groups)
}

func TestPackDevices_AMDModeMutexPacksToOne(t *testing.T) {
	packed := PackDevices(amdModeDevices(0, "spx", "dpx", "cpx"), nil)
	assert.Len(t, packed, 1, "modes share one mutex counter of value 1")
}

func TestPackDevices_MIGSlicesPackDisjointly(t *testing.T) {
	var slices []TopologyDevice
	for _, d := range migGPU(0) {
		if d.ExtendedAttributes[migDriver+"/type"].String() == "mig" {
			slices = append(slices, d)
		}
	}
	assert.Len(t, PackDevices(slices, migGPU(0)), 7)
}

func TestPackDevices_MixedProfilesRespectOverlap(t *testing.T) {
	// 3g.20gb at memory slices 0-3 overlaps four 1g slices; the other four 1g fit beside it.
	devices := []TopologyDevice{migDevice(0, "mig-3g-0", "mig", "3g.20gb", 0, 4)}
	for i := 0; i < 7; i++ {
		devices = append(devices, migDevice(0, fmt.Sprintf("mig-1g-%d", i), "mig", "1g.5gb", i, 1))
	}
	// Smallest-first packing takes all seven 1g slices and cannot also fit the 3g.
	assert.Len(t, PackDevices(devices, append(devices, migGPU(0)[0])), 7)
}

func TestDynamicMIG_GPUStaysOneEffectiveDevice(t *testing.T) {
	devices := append(migGPU(0), migGPU(1)...)

	_, groups := GroupOverlappingDevices(devices)
	require.Len(t, groups, 2, "one counter set per physical GPU")
	assert.Equal(t, 2, EffectiveDeviceCount(devices)[migDriver], "tier math counts GPUs, not slices")
}

func TestDynamicMIG_RepresentativeIsFullGPU(t *testing.T) {
	devices := migGPU(0)
	// A full-size 7g slice consumes as much as the full GPU, so it must not win on list order.
	full := migDevice(0, "gpu-0-mig-7g.40gb-0", "mig", "7g.40gb", 0, migSlices)
	for _, order := range [][]TopologyDevice{append([]TopologyDevice{full}, devices...), append(devices, full)} {
		assert.Equal(t, "gpu-0", selectRepresentative(order).DeviceName)
	}
}

func TestDynamicMIG_AmpereMIGOnHasNoFullGPU(t *testing.T) {
	// Documents a quirk: when the driver does not announce the full GPU, the
	// representative is the largest slice.
	var slices []TopologyDevice
	for _, d := range migGPU(0) {
		if d.DeviceName != "gpu-0" {
			slices = append(slices, d)
		}
	}
	slices = append(slices, migDevice(0, "gpu-0-mig-7g.40gb-0", "mig", "7g.40gb", 0, migSlices))
	assert.Equal(t, "gpu-0-mig-7g.40gb-0", selectRepresentative(slices).DeviceName)
}
