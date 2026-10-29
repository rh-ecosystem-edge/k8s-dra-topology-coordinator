package controller

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	klog "k8s.io/klog/v2"
)

// PartitionType identifies the size of a partition relative to the whole node.
type PartitionType string

const (
	PartitionPCIeRoot PartitionType = "pcieroot"
	PartitionNUMA     PartitionType = "numa"
	PartitionFull     PartitionType = "full"
)

// PartitionDevice represents a computed partition that the coordinator will publish
// as a device in its own ResourceSlice.
type PartitionDevice struct {
	// Name is the unique device name within the ResourceSlice.
	Name string
	// NodeName is the Kubernetes node this partition belongs to.
	NodeName string
	// Type is the partition size (eighth, quarter, half, full).
	Type PartitionType
	// NUMANode is the NUMA node ID for this partition (may span multiple for larger partitions).
	NUMANodes []int64
	// PCIeRoots lists the PCIe root complexes included in this partition.
	PCIeRoots []string
	// Sockets lists the CPU sockets included in this partition.
	Sockets []int64

	// DeviceCounts maps driver name -> count of devices from that driver in this partition.
	DeviceCounts map[string]int
	// DeviceCapacity maps driver name -> capacity requests for shared devices.
	// Used when a device is shared via DRAConsumableCapacity (count=1 but subdivided).
	DeviceCapacity map[string]map[string]string
	// Devices lists the individual topology devices grouped into this partition.
	Devices []TopologyDevice

	// CPUCount is the number of CPUs in this partition (from kubelet plugin discovery).
	CPUCount int
	// MemoryBytes is the amount of memory in this partition.
	MemoryBytes int64

	// ExtendedAttributes contains additional topology attributes from topology rules.
	ExtendedAttributes map[string]DeviceAttributeValue

	// Profile is a human-readable label for the node hardware profile (e.g., "hgx-b200").
	Profile string
}

// PartitionResult holds all computed partitions for a single node.
type PartitionResult struct {
	NodeName   string
	Profile    string
	Partitions []PartitionDevice
}

// PartitionBuilder computes aligned partition combinations from the topology model.
type PartitionBuilder struct {
	model *TopologyModel
	rules *TopologyRuleStore
}

// NewPartitionBuilder creates a partition builder.
func NewPartitionBuilder(model *TopologyModel, rules *TopologyRuleStore) *PartitionBuilder {
	return &PartitionBuilder{
		model: model,
		rules: rules,
	}
}

// BuildPartitions computes partition devices for all nodes in the topology model.
func (b *PartitionBuilder) BuildPartitions() []PartitionResult {
	nodes := b.model.GetNodeTopologies()
	groupingRules := b.rules.GetGroupingRules()

	var results []PartitionResult
	for nodeName, nodeTopo := range nodes {
		result := b.buildNodePartitions(nodeName, nodeTopo, groupingRules)
		if len(result.Partitions) > 0 {
			results = append(results, result)
		}
	}
	return results
}

// DetectPCIeRootPairings auto-discovers device pairings by finding PCIe roots
// that host devices from 2+ different drivers. Each unique driver combination
// produces a DeviceGrouping with pcieRoot alignment and numaNode fallback.
// Capacity-only drivers (CPU, memory) are excluded.
func (b *PartitionBuilder) DetectPCIeRootPairings() []DeviceGrouping {
	nodes := b.model.GetNodeTopologies()

	type driverPair struct{ a, b string }
	seen := make(map[driverPair]bool)
	var groupings []DeviceGrouping

	for _, nodeTopo := range nodes {
		allDevices := nodeTopo.AllDevices()

		byPCIeRoot := groupDevicesByAttribute(allDevices, func(d TopologyDevice) string {
			if d.PCIeRoot != nil {
				return *d.PCIeRoot
			}
			return ""
		})

		for rootKey, devices := range byPCIeRoot {
			if rootKey == "" {
				continue
			}

			// Collect unique PCI drivers on this root (exclude capacity-only)
			pciDrivers := make(map[string]int)
			for _, d := range devices {
				driver := baseDriverName(d.DriverName)
				if len(d.Capacity) > 0 && d.PCIeRoot == nil {
					continue
				}
				pciDrivers[driver]++
			}

			if len(pciDrivers) < 2 {
				continue
			}

			// Create pairings for each unique driver combination
			drivers := make([]string, 0, len(pciDrivers))
			for d := range pciDrivers {
				drivers = append(drivers, d)
			}
			sort.Strings(drivers)

			for i := 0; i < len(drivers); i++ {
				for j := i + 1; j < len(drivers); j++ {
					pair := driverPair{drivers[i], drivers[j]}
					if seen[pair] {
						continue
					}
					seen[pair] = true

					classA := b.rules.GetDeviceClassForDriver(drivers[i])
					classB := b.rules.GetDeviceClassForDriver(drivers[j])
					name := sanitizeDNSLabel(
						sanitizeForName(classA) + "-" + sanitizeForName(classB) + "-pair")
					groupings = append(groupings, DeviceGrouping{
						Name:         name,
						Alignment:    "pcieRoot",
						Fallback:     "numaNode",
						AutoDetected: true,
						Devices: []GroupingDevice{
							{Class: b.rules.GetDeviceClassForDriver(drivers[i]), Count: 1},
							{Class: b.rules.GetDeviceClassForDriver(drivers[j]), Count: 1},
						},
					})
				}
			}
		}
	}

	return groupings
}

// buildNodePartitions computes partitions for a single node.
func (b *PartitionBuilder) buildNodePartitions(
	nodeName string,
	nodeTopo *NodeTopology,
	groupingRules []TopologyRule,
) PartitionResult {
	result := PartitionResult{
		NodeName: nodeName,
	}

	allDevices := nodeTopo.AllDevices()
	if len(allDevices) == 0 {
		return result
	}

	// Filter overlapping partitionable devices down to one representative
	// per physical resource. This prevents a single GPU advertised as
	// SPX + 2 DPX + 8 CPX (11 devices) from being counted as 11.
	nonOverlapping, groups := GroupOverlappingDevices(allDevices)
	effectiveDevices := make([]TopologyDevice, 0, len(nonOverlapping)+len(groups))
	effectiveDevices = append(effectiveDevices, nonOverlapping...)
	for _, g := range groups {
		effectiveDevices = append(effectiveDevices, selectRepresentative(g.Devices))
	}

	driverDeviceCounts := make(map[string]int)
	for _, d := range nonOverlapping {
		driverDeviceCounts[baseDriverName(d.DriverName)]++
	}
	for _, g := range groups {
		driverDeviceCounts[baseDriverName(g.DriverName)] += g.MaxEffective
	}

	// Group devices by PCIe root for the finest-grained partitioning
	byPCIeRoot := groupDevicesByAttribute(effectiveDevices, func(d TopologyDevice) string {
		if d.PCIeRoot != nil {
			return *d.PCIeRoot
		}
		return ""
	})

	// Group devices by NUMA node
	byNUMA := groupDevicesByAttribute(effectiveDevices, func(d TopologyDevice) string {
		if d.NUMANode != nil {
			return fmt.Sprintf("%d", *d.NUMANode)
		}
		return ""
	})

	// Validate grouping alignment using extended rules
	for _, rule := range groupingRules {
		if !b.validateGroupingAlignment(effectiveDevices, rule) {
			klog.Warningf("Node %s: devices not aligned by rule attribute %s, skipping extended grouping",
				nodeName, rule.Attribute)
		}
	}

	profile := b.inferProfile(driverDeviceCounts)
	result.Profile = profile

	// Build pcieRoot partitions: one per PCIe root with proportional CPU/memory
	pciePartitions := b.buildPCIeRootPartitions(nodeName, profile, byNUMA, byPCIeRoot, effectiveDevices)
	result.Partitions = append(result.Partitions, pciePartitions...)

	// Build NUMA partitions: one per NUMA node with all devices on that NUMA
	numaPartitions := b.buildPartitionsFromGroups(nodeName, profile, PartitionNUMA, byNUMA, groupingRules)
	result.Partitions = append(result.Partitions, numaPartitions...)

	// Build full partition: all devices on the node
	full := b.buildFullPartition(nodeName, profile, effectiveDevices, groupingRules)
	if full != nil {
		result.Partitions = append(result.Partitions, *full)
	}

	tierCounts := make(map[PartitionType]int)
	for _, p := range result.Partitions {
		tierCounts[p.Type]++
	}
	klog.Infof("Node %s (profile=%s): computed %d partitions (%d pcieRoot, %d numa, %d full)",
		nodeName, profile, len(result.Partitions),
		tierCounts[PartitionPCIeRoot], tierCounts[PartitionNUMA],
		tierCounts[PartitionFull])

	return result
}

// buildPartitionsFromGroups creates partition devices from grouped devices.
func (b *PartitionBuilder) buildPartitionsFromGroups(
	nodeName, profile string,
	partType PartitionType,
	groups map[string][]TopologyDevice,
	groupingRules []TopologyRule,
) []PartitionDevice {
	// Don't create partitions if there's only one group (it would duplicate the full partition)
	// or if the grouping key is empty (devices lack the attribute)
	validGroups := make(map[string][]TopologyDevice)
	for key, devices := range groups {
		if key != "" {
			validGroups[key] = devices
		}
	}

	if len(validGroups) <= 1 {
		return nil
	}

	// Validate that extended grouping rules are satisfied.
	// Build a new map to avoid mutating validGroups during iteration.
	for _, rule := range groupingRules {
		splitGroups := make(map[string][]TopologyDevice)
		for key, devices := range validGroups {
			if !devicesShareAttribute(devices, rule.Attribute) {
				klog.V(4).Infof("Partition group %s on node %s: devices don't share %s, splitting further",
					key, nodeName, rule.Attribute)
				subGroups := groupDevicesByExtendedAttribute(devices, rule.Attribute)
				for subKey, subDevices := range subGroups {
					splitGroups[key+"-"+subKey] = subDevices
				}
			} else {
				splitGroups[key] = devices
			}
		}
		validGroups = splitGroups
	}

	// Sort group keys for deterministic output
	keys := make([]string, 0, len(validGroups))
	for k := range validGroups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var partitions []PartitionDevice
	for i, key := range keys {
		devices := validGroups[key]
		p := buildPartitionFromDevices(
			fmt.Sprintf("%s-%s-%d", nodeName, partType, i),
			nodeName, profile, partType, devices,
		)

		// Enrich with capacity from shared devices (CPU, memory). For NUMA
		// partitions the full capacity of each device applies (no division,
		// unlike pcieRoot which splits across roots).
		for _, d := range devices {
			driver := baseDriverName(d.DriverName)
			if len(d.Capacity) > 0 {
				if p.DeviceCapacity == nil {
					p.DeviceCapacity = make(map[string]map[string]string)
				}
				if _, has := p.DeviceCapacity[driver]; !has {
					p.DeviceCapacity[driver] = d.Capacity
				}
			}
		}

		partitions = append(partitions, p)
	}

	return partitions
}

// buildPCIeRootPartitions creates one partition per PCIe root complex.
// Each partition contains all devices on that PCIe root, plus a proportional
// share of the parent NUMA node's CPU and memory (equal split by default).
func (b *PartitionBuilder) buildPCIeRootPartitions(
	nodeName, profile string,
	byNUMA map[string][]TopologyDevice,
	byPCIeRoot map[string][]TopologyDevice,
	allDevices []TopologyDevice,
) []PartitionDevice {
	// Count unique PCIe roots per NUMA node for CPU/memory division
	numaPCIeRoots := make(map[string]map[string]bool)
	for _, d := range allDevices {
		if d.NUMANode == nil || d.PCIeRoot == nil {
			continue
		}
		numaKey := fmt.Sprintf("%d", *d.NUMANode)
		if numaPCIeRoots[numaKey] == nil {
			numaPCIeRoots[numaKey] = make(map[string]bool)
		}
		numaPCIeRoots[numaKey][*d.PCIeRoot] = true
	}

	// Sort PCIe root keys for deterministic output
	pcieKeys := make([]string, 0, len(byPCIeRoot))
	for k := range byPCIeRoot {
		if k != "" {
			pcieKeys = append(pcieKeys, k)
		}
	}
	sort.Strings(pcieKeys)

	var partitions []PartitionDevice
	for idx, pcieKey := range pcieKeys {
		devices := byPCIeRoot[pcieKey]
		p := buildPartitionFromDevices(
			fmt.Sprintf("%s-pcieRoot-%d", nodeName, idx),
			nodeName, profile, PartitionPCIeRoot, devices,
		)

		// Add proportional CPU/memory from the parent NUMA node.
		// Find which NUMA this PCIe root belongs to and how many roots share it.
		var parentNUMA string
		for _, d := range devices {
			if d.NUMANode != nil {
				parentNUMA = fmt.Sprintf("%d", *d.NUMANode)
				break
			}
		}
		if parentNUMA != "" {
			numRoots := len(numaPCIeRoots[parentNUMA])
			if numRoots > 0 {
				numaDevices := byNUMA[parentNUMA]
				for _, d := range numaDevices {
					driver := baseDriverName(d.DriverName)
					if len(d.Capacity) > 0 && p.DeviceCounts[driver] == 0 {
						if p.DeviceCapacity == nil {
							p.DeviceCapacity = make(map[string]map[string]string)
						}
						capPerPartition := make(map[string]string)
						for capName, capVal := range d.Capacity {
							dv := divideQuantity(capVal, numRoots)
							if dv != "" {
								capPerPartition[capName] = dv
							}
						}
						if len(capPerPartition) > 0 {
							p.DeviceCapacity[driver] = capPerPartition
							p.DeviceCounts[driver] = 1
							p.Devices = append(p.Devices, d)
						}
					}
				}
			}
		}

		partitions = append(partitions, p)
	}

	return partitions
}

// buildFullPartition creates a single partition containing all devices on the node.
func (b *PartitionBuilder) buildFullPartition(
	nodeName, profile string,
	devices []TopologyDevice,
	_ []TopologyRule,
) *PartitionDevice {
	if len(devices) == 0 {
		return nil
	}
	p := buildPartitionFromDevices(
		fmt.Sprintf("%s-full-0", nodeName),
		nodeName, profile, PartitionFull, devices,
	)
	return &p
}

// buildPartitionFromDevices constructs a PartitionDevice from a set of TopologyDevices.
func buildPartitionFromDevices(
	name, nodeName, profile string,
	partType PartitionType,
	devices []TopologyDevice,
) PartitionDevice {
	p := PartitionDevice{
		Name:               name,
		NodeName:           nodeName,
		Type:               partType,
		Profile:            profile,
		DeviceCounts:       make(map[string]int),
		Devices:            devices,
		ExtendedAttributes: make(map[string]DeviceAttributeValue),
	}

	numaSet := make(map[int64]bool)
	pcieSet := make(map[string]bool)
	socketSet := make(map[int64]bool)

	// Use effective counting to handle overlapping partitionable devices.
	p.DeviceCounts = EffectiveDeviceCount(devices)

	for _, d := range devices {
		if d.NUMANode != nil {
			numaSet[*d.NUMANode] = true
		}
		if d.PCIeRoot != nil {
			pcieSet[*d.PCIeRoot] = true
		}
		if d.Socket != nil {
			socketSet[*d.Socket] = true
		}

		// Collect extended attributes (use first device's values as representative)
		for k, v := range d.ExtendedAttributes {
			if _, exists := p.ExtendedAttributes[k]; !exists {
				p.ExtendedAttributes[k] = v
			}
		}
	}

	for n := range numaSet {
		p.NUMANodes = append(p.NUMANodes, n)
	}
	sort.Slice(p.NUMANodes, func(i, j int) bool { return p.NUMANodes[i] < p.NUMANodes[j] })

	for r := range pcieSet {
		p.PCIeRoots = append(p.PCIeRoots, r)
	}
	sort.Strings(p.PCIeRoots)

	for s := range socketSet {
		p.Sockets = append(p.Sockets, s)
	}
	sort.Slice(p.Sockets, func(i, j int) bool { return p.Sockets[i] < p.Sockets[j] })

	return p
}

// inferProfile identifies the hardware profile from driver names only.
// Device counts are excluded to keep the profile (and DeviceClass names)
// stable across transient device count changes from driver restarts or
// allocation events.
func (b *PartitionBuilder) inferProfile(driverDeviceCounts map[string]int) string {
	var parts []string
	for driver := range driverDeviceCounts {
		parts = append(parts, driver)
	}
	sort.Strings(parts)
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, "_")
}

// validateGroupingAlignment checks if all devices that share a standard topology group
// also share the extended attribute value.
func (b *PartitionBuilder) validateGroupingAlignment(devices []TopologyDevice, rule TopologyRule) bool {
	// Group devices by their NUMA node and check that within each NUMA group,
	// all devices from the rule's driver share the same extended attribute value.
	byNUMA := make(map[int64][]TopologyDevice)
	for _, d := range devices {
		if d.NUMANode != nil && baseDriverName(d.DriverName) == rule.Driver {
			byNUMA[*d.NUMANode] = append(byNUMA[*d.NUMANode], d)
		}
	}

	for _, group := range byNUMA {
		if !devicesShareAttribute(group, rule.Attribute) {
			return false
		}
	}
	return true
}

// groupDevicesByAttribute groups devices by a string key derived from each device.
func groupDevicesByAttribute(devices []TopologyDevice, keyFn func(TopologyDevice) string) map[string][]TopologyDevice {
	groups := make(map[string][]TopologyDevice)
	for _, d := range devices {
		key := keyFn(d)
		groups[key] = append(groups[key], d)
	}
	return groups
}

// groupDevicesByExtendedAttribute groups devices by an extended attribute value.
func groupDevicesByExtendedAttribute(devices []TopologyDevice, attribute string) map[string][]TopologyDevice {
	groups := make(map[string][]TopologyDevice)
	for _, d := range devices {
		val, ok := d.ExtendedAttributes[attribute]
		if !ok {
			groups["none"] = append(groups["none"], d)
			continue
		}
		groups[val.String()] = append(groups[val.String()], d)
	}
	return groups
}

// devicesShareAttribute checks if all devices share the same value for an extended attribute.
func devicesShareAttribute(devices []TopologyDevice, attribute string) bool {
	if len(devices) == 0 {
		return true
	}

	var firstVal *string
	for _, d := range devices {
		val, ok := d.ExtendedAttributes[attribute]
		if !ok {
			continue
		}
		s := val.String()
		if firstVal == nil {
			firstVal = &s
		} else if s != *firstVal {
			return false
		}
	}
	return true
}

// baseDriverName extracts the base driver name from a driver/pool key.
func baseDriverName(driverName string) string {
	if idx := strings.Index(driverName, "/"); idx >= 0 {
		return driverName[:idx]
	}
	return driverName
}

// divideQuantity divides a Kubernetes quantity string by a divisor.
// Returns the divided quantity as a string, or empty string on failure.
func divideQuantity(qty string, divisor int) string {
	if divisor <= 0 {
		return ""
	}
	q, err := resource.ParseQuantity(qty)
	if err != nil {
		klog.V(4).Infof("Failed to parse quantity %q: %v", qty, err)
		return ""
	}
	val := q.Value()
	divided := val / int64(divisor)
	if divided <= 0 {
		return ""
	}
	result := resource.NewQuantity(divided, q.Format)
	return result.String()
}
