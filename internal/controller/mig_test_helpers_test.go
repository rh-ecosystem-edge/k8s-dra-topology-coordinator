package controller

import (
	"fmt"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	migDriver  = "gpu.nvidia.com"
	migGiB     = int64(1) << 30
	migSMs     = 14 // multiprocessors per memory slice on an A100-class GPU
	migMemPerS = 5  // GiB per memory slice
	migSlices  = 8
)

func strAttr(v string) DeviceAttributeValue { return DeviceAttributeValue{StringValue: &v} }

// migCounters mirrors dra-driver-nvidia-gpu: capacity counters plus one
// memory-slice-N counter of value 1 per memory slice in the placement.
func migCounters(counterSet string, start, size int) []resourcev1.DeviceCounterConsumption {
	counters := map[string]resourcev1.Counter{
		"multiprocessors": {Value: *resource.NewQuantity(int64(size*migSMs), resource.DecimalSI)},
		"memory":          {Value: *resource.NewQuantity(int64(size*migMemPerS)*migGiB, resource.BinarySI)},
	}
	for i := start; i < start+size; i++ {
		counters[fmt.Sprintf("memory-slice-%d", i)] = resourcev1.Counter{Value: *resource.NewQuantity(1, resource.BinarySI)}
	}
	return []resourcev1.DeviceCounterConsumption{{CounterSet: counterSet, Counters: counters}}
}

func migDevice(gpu int, name, devType, profile string, start, size int) TopologyDevice {
	attrs := map[string]DeviceAttributeValue{migDriver + "/type": strAttr(devType)}
	if profile != "" {
		attrs[migDriver+"/profile"] = strAttr(profile)
	}
	return TopologyDevice{
		DriverName:         migDriver,
		DeviceName:         name,
		NodeName:           "node-1",
		PoolName:           "node-1",
		NUMANode:           intPtr(0),
		PCIeRoot:           strPtr(fmt.Sprintf("pci-%d", gpu)),
		ConsumesCounters:   migCounters(fmt.Sprintf("gpu-%d-counter-set", gpu), start, size),
		ExtendedAttributes: attrs,
	}
}

// migGPU returns the full GPU plus all seven 1g.5gb placements.
func migGPU(gpu int) []TopologyDevice {
	devices := []TopologyDevice{migDevice(gpu, fmt.Sprintf("gpu-%d", gpu), "gpu", "", 0, migSlices)}
	for i := 0; i < 7; i++ {
		devices = append(devices, migDevice(gpu, fmt.Sprintf("gpu-%d-mig-1g.5gb-%d", gpu, i), "mig", "1g.5gb", i, 1))
	}
	return devices
}

// amdModeDevices mimics dra-driver AMD AutoPartition: one device per partition
// mode, all consuming the single "partition-mode" mutex counter.
func amdModeDevices(gpu int, modes ...string) []TopologyDevice {
	var devices []TopologyDevice
	for _, m := range modes {
		devices = append(devices, TopologyDevice{
			DriverName: "gpu.amd.com",
			DeviceName: fmt.Sprintf("gpu-%d-%s", gpu, m),
			PoolName:   "node-1",
			ConsumesCounters: []resourcev1.DeviceCounterConsumption{{
				CounterSet: fmt.Sprintf("gpu-%d-mutex", gpu),
				Counters:   map[string]resourcev1.Counter{"partition-mode": makeCounter(1)},
			}},
			ExtendedAttributes: map[string]DeviceAttributeValue{},
		})
	}
	return devices
}
