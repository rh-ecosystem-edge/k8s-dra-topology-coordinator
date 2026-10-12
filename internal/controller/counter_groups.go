package controller

import (
	"sort"

	resourcev1 "k8s.io/api/resource/v1"
	klog "k8s.io/klog/v2"
)

// PhysicalResourceGroup represents a set of overlapping device partitions
// that all draw from the same counter set, hence represent the same
// physical resource in different partition modes.
type PhysicalResourceGroup struct {
	CounterSetName string
	DriverName     string
	PoolName       string
	Devices        []TopologyDevice
	// MaxEffective is the maximum number of non-overlapping devices
	// that can be allocated simultaneously from this group.
	// For a single physical GPU with SPX/DPX/CPX variants, this is 1.
	MaxEffective int
}

// GroupOverlappingDevices partitions a device list into non-overlapping devices
// (those without ConsumesCounters) and groups of overlapping partitions
// (those sharing a counter set). This is the primary entry point for
// counter-aware device deduplication.
func GroupOverlappingDevices(devices []TopologyDevice) (nonOverlapping []TopologyDevice, groups []PhysicalResourceGroup) {
	type groupKey struct {
		driverName     string
		poolName       string
		counterSetName string
	}

	groupMap := make(map[groupKey]*PhysicalResourceGroup)

	for _, d := range devices {
		if len(d.ConsumesCounters) == 0 {
			nonOverlapping = append(nonOverlapping, d)
			continue
		}

		// Group by the first counter set reference (primary resource identity).
		// The API allows up to 2 counter set refs per device; the first is
		// the defining physical resource.
		csName := d.ConsumesCounters[0].CounterSet
		key := groupKey{
			driverName:     d.DriverName,
			poolName:       d.PoolName,
			counterSetName: csName,
		}

		if g, ok := groupMap[key]; ok {
			g.Devices = append(g.Devices, d)
		} else {
			groupMap[key] = &PhysicalResourceGroup{
				CounterSetName: csName,
				DriverName:     d.DriverName,
				PoolName:       d.PoolName,
				Devices:        []TopologyDevice{d},
			}
		}
	}

	// Compute MaxEffective for each group and sort for deterministic output.
	keys := make([]groupKey, 0, len(groupMap))
	for k := range groupMap {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].driverName != keys[j].driverName {
			return keys[i].driverName < keys[j].driverName
		}
		if keys[i].poolName != keys[j].poolName {
			return keys[i].poolName < keys[j].poolName
		}
		return keys[i].counterSetName < keys[j].counterSetName
	})

	for _, k := range keys {
		g := groupMap[k]
		g.MaxEffective = computeMaxEffective(g.Devices)
		groups = append(groups, *g)
		klog.V(4).Infof("Counter group %s/%s/%s: %d devices, %d effective",
			g.DriverName, g.PoolName, g.CounterSetName, len(g.Devices), g.MaxEffective)
	}

	return nonOverlapping, groups
}

// computeMaxEffective determines how many physical resources a group represents.
// It finds the device consuming the most of any single counter (the "full" device)
// and computes how many non-overlapping instances exist. Each unique counter set
// name represents one physical resource, so MaxEffective is 1 per counter set.
func computeMaxEffective(devices []TopologyDevice) int {
	if len(devices) == 0 {
		return 0
	}
	// Each counter set name represents one physical resource.
	// All devices in this group share the same counter set name,
	// so MaxEffective is 1.
	return 1
}

// EffectiveDeviceCount returns the effective device count per base driver name,
// accounting for overlapping partitionable devices. For drivers without counter
// consumption, the raw count is returned. For drivers with it, the sum of
// MaxEffective across their PhysicalResourceGroups is returned.
func EffectiveDeviceCount(devices []TopologyDevice) map[string]int {
	nonOverlapping, groups := GroupOverlappingDevices(devices)

	counts := make(map[string]int)
	for _, d := range nonOverlapping {
		counts[baseDriverName(d.DriverName)]++
	}
	for _, g := range groups {
		counts[baseDriverName(g.DriverName)] += g.MaxEffective
	}

	return counts
}

// FilterToEffectiveDevices returns a device list with overlapping partitions
// reduced to one representative per physical resource. For each
// PhysicalResourceGroup, the device consuming the most counters (the "full"
// variant) is selected as the representative.
func FilterToEffectiveDevices(devices []TopologyDevice) []TopologyDevice {
	nonOverlapping, groups := GroupOverlappingDevices(devices)

	result := make([]TopologyDevice, 0, len(nonOverlapping)+len(groups))
	result = append(result, nonOverlapping...)

	for _, g := range groups {
		rep := selectRepresentative(g.Devices)
		result = append(result, rep)
	}
	return result
}

// selectRepresentative picks the device that consumes the most total counter
// value from a group of overlapping devices. This is typically the "full"
// device (SPX for AMD GPUs, full GPU for NVIDIA MIG).
func selectRepresentative(devices []TopologyDevice) TopologyDevice {
	if len(devices) == 1 {
		return devices[0]
	}

	best := devices[0]
	bestTotal := totalCounterConsumption(best.ConsumesCounters)

	for _, d := range devices[1:] {
		total := totalCounterConsumption(d.ConsumesCounters)
		// Ties resolve to the lexically smaller name so list order never decides.
		// GroupingBuilder.representative prefers the default-class device first.
		if total > bestTotal || (total == bestTotal && d.DeviceName < best.DeviceName) {
			best = d
			bestTotal = total
		}
	}
	return best
}

// totalCounterConsumption sums all counter values across all counter set
// references for a device. Used to rank devices by "size" within a group.
func totalCounterConsumption(consumptions []resourcev1.DeviceCounterConsumption) int64 {
	var total int64
	for _, cc := range consumptions {
		for _, counter := range cc.Counters {
			total += counter.Value.Value()
		}
	}
	return total
}

// SmallestPartitionDevices returns the devices from each PhysicalResourceGroup
// that consume the least counters (finest-grained partitions, e.g., CPX eighths).
// This is useful for building the finest partition tier where you want the
// maximum number of individually allocatable units.
func SmallestPartitionDevices(devices []TopologyDevice) []TopologyDevice {
	nonOverlapping, groups := GroupOverlappingDevices(devices)

	result := make([]TopologyDevice, 0, len(nonOverlapping))
	result = append(result, nonOverlapping...)

	for _, g := range groups {
		smallest := selectSmallestPartitions(g.Devices)
		result = append(result, smallest...)
	}
	return result
}

// selectSmallestPartitions returns all devices from a group that have the
// minimum total counter consumption (i.e., the smallest partition size).
func selectSmallestPartitions(devices []TopologyDevice) []TopologyDevice {
	if len(devices) <= 1 {
		return devices
	}

	minTotal := totalCounterConsumption(devices[0].ConsumesCounters)
	for _, d := range devices[1:] {
		total := totalCounterConsumption(d.ConsumesCounters)
		if total < minTotal {
			minTotal = total
		}
	}

	var smallest []TopologyDevice
	for _, d := range devices {
		if totalCounterConsumption(d.ConsumesCounters) == minTotal {
			smallest = append(smallest, d)
		}
	}
	return smallest
}

// PackDevices returns the largest subset of devices that can be allocated at
// the same time from one counter set. A counter set's capacity is not visible
// in the topology model, so it is approximated by the highest consumption seen
// per counter across capacitySource, normally every device of the counter
// group (the full device on NVIDIA MIG; the single mutex counter on AMD
// partition-mode sets, where it packs to one device). A nil capacitySource
// uses devices themselves.
//
// shortcut: greedy smallest-first packing, exact for aligned MIG placements but
// only approximate for arbitrary counter shapes, upgrade if a driver needs it.
// Capacity is underestimated when the group lacks a device that consumes the
// physical total, which rejects groupings rather than over-offering them.
func PackDevices(devices, capacitySource []TopologyDevice) []TopologyDevice {
	if len(devices) == 0 {
		return nil
	}
	if capacitySource == nil {
		capacitySource = devices
	}

	type counterKey struct{ set, counter string }
	consumption := func(d TopologyDevice) map[counterKey]int64 {
		m := make(map[counterKey]int64)
		for _, cc := range d.ConsumesCounters {
			for name, c := range cc.Counters {
				m[counterKey{cc.CounterSet, name}] += c.Value.Value()
			}
		}
		return m
	}

	capacity := make(map[counterKey]int64)
	for _, d := range capacitySource {
		for k, v := range consumption(d) {
			if v > capacity[k] {
				capacity[k] = v
			}
		}
	}

	sorted := append([]TopologyDevice(nil), devices...)
	sort.SliceStable(sorted, func(i, j int) bool {
		ti, tj := totalCounterConsumption(sorted[i].ConsumesCounters), totalCounterConsumption(sorted[j].ConsumesCounters)
		if ti != tj {
			return ti < tj
		}
		return sorted[i].DeviceName < sorted[j].DeviceName
	})

	used := make(map[counterKey]int64)
	var packed []TopologyDevice
	for _, d := range sorted {
		c := consumption(d)
		fits := true
		for k, v := range c {
			if used[k]+v > capacity[k] {
				fits = false
				break
			}
		}
		if !fits {
			continue
		}
		for k, v := range c {
			used[k] += v
		}
		packed = append(packed, d)
	}
	return packed
}
