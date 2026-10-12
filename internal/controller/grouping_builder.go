package controller

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	klog "k8s.io/klog/v2"
)

// GroupingInstance represents a single satisfiable instance of a device grouping
// on a specific node at a specific alignment level.
type GroupingInstance struct {
	// Name is the unique device name within the ResourceSlice.
	Name string
	// NodeName is the Kubernetes node this instance belongs to.
	NodeName string
	// GroupingName is the admin-defined grouping name.
	GroupingName string
	// Alignment is the topology attribute this instance matched on
	// (e.g., "pcieRoot", "numaNode").
	Alignment string

	// NUMANodes lists the NUMA nodes spanned by this instance.
	NUMANodes []int64
	// PCIeRoots lists the PCIe root complexes in this instance.
	PCIeRoots []string
	// Sockets lists the CPU sockets in this instance.
	Sockets []int64

	// DeviceCounts maps driver name -> count of devices in this instance.
	DeviceCounts map[string]int
	// DeviceProfiles maps class -> required device profile, when set.
	DeviceProfiles map[string]string
	// DeviceCapacity maps driver name -> capacity for shared devices.
	DeviceCapacity map[string]map[string]string
	// Devices lists the topology devices in this instance.
	Devices []TopologyDevice

	// ExtendedAttributes from topology rules.
	ExtendedAttributes map[string]DeviceAttributeValue

	// RailIndex is the deterministic index of this instance within its
	// grouping on a node. Used for rail networking to map GPU-to-NIC
	// pairings consistently across nodes.
	RailIndex int
}

// GroupingResult holds all computed grouping instances for a single node.
type GroupingResult struct {
	NodeName  string
	Instances []GroupingInstance
}

// GroupingBuilder validates admin-defined device groupings against real topology.
type GroupingBuilder struct {
	model *TopologyModel
	rules *TopologyRuleStore
}

// NewGroupingBuilder creates a grouping builder.
func NewGroupingBuilder(model *TopologyModel, rules *TopologyRuleStore) *GroupingBuilder {
	return &GroupingBuilder{
		model: model,
		rules: rules,
	}
}

// BuildGroupings validates all groupings against all nodes and returns
// satisfiable instances with their alignment level.
func (b *GroupingBuilder) BuildGroupings(groupings []DeviceGrouping) []GroupingResult {
	nodes := b.model.GetNodeTopologies()

	var results []GroupingResult
	for nodeName, nodeTopo := range nodes {
		result := b.buildNodeGroupings(nodeName, nodeTopo, groupings)
		if len(result.Instances) > 0 {
			results = append(results, result)
		}
	}
	return results
}

// buildNodeGroupings evaluates all grouping definitions against a single node.
func (b *GroupingBuilder) buildNodeGroupings(
	nodeName string,
	nodeTopo *NodeTopology,
	groupings []DeviceGrouping,
) GroupingResult {
	result := GroupingResult{NodeName: nodeName}

	allDevices := nodeTopo.AllDevices()
	if len(allDevices) == 0 {
		return result
	}

	for _, grouping := range groupings {
		instances := b.evaluateGrouping(nodeName, allDevices, grouping)
		result.Instances = append(result.Instances, instances...)
	}

	if len(result.Instances) > 0 {
		counts := make(map[string]int)
		for _, inst := range result.Instances {
			counts[inst.GroupingName+"-"+inst.Alignment]++
		}
		for key, count := range counts {
			klog.Infof("Node %s: %d instances of %s", nodeName, count, key)
		}
	}

	return result
}

// evaluateGrouping checks if a grouping definition is satisfiable on a node,
// first at the preferred alignment level, then at the fallback level.
func (b *GroupingBuilder) evaluateGrouping(
	nodeName string,
	allDevices []TopologyDevice,
	grouping DeviceGrouping,
) []GroupingInstance {
	// Check if the node has all required device classes at all
	if !b.nodeHasRequiredClasses(allDevices, grouping) {
		return nil
	}

	// Try preferred alignment
	instances := b.findInstances(nodeName, allDevices, grouping, grouping.Alignment)

	// If fallback is defined and some devices weren't captured, try fallback
	if grouping.Fallback != "" {
		usedDevices := b.collectUsedDevices(instances)
		remainingDevices := b.filterOutDevices(allDevices, usedDevices)

		if len(remainingDevices) > 0 {
			fallbackInstances := b.findInstances(nodeName, remainingDevices, grouping, grouping.Fallback)
			instances = append(instances, fallbackInstances...)
		}
	}

	return instances
}

// candidate is a device that can fill a grouping entry, tagged with the
// counter group it draws from. Two candidates of one group cannot both be
// allocated unless they are co-allocatable slices.
type candidate struct {
	dev   TopologyDevice
	group string
}

func counterGroupID(d TopologyDevice) string {
	if len(d.ConsumesCounters) == 0 {
		return ""
	}
	return d.DriverName + "/" + d.PoolName + "/" + d.ConsumesCounters[0].CounterSet
}

// representative picks the device that stands for a whole counter group. On a
// tie in counter consumption (a full-size slice next to the full device) the
// device in the driver's default class wins, so a slice never replaces the
// full device.
func (b *GroupingBuilder) representative(devices []TopologyDevice) TopologyDevice {
	max := int64(-1)
	for _, d := range devices {
		if t := totalCounterConsumption(d.ConsumesCounters); t > max {
			max = t
		}
	}
	var top, preferred []TopologyDevice
	for _, d := range devices {
		if totalCounterConsumption(d.ConsumesCounters) != max {
			continue
		}
		top = append(top, d)
		if b.rules.GetDeviceClassForDevice(d) == b.rules.GetDeviceClassForDriver(baseDriverName(d.DriverName)) {
			preferred = append(preferred, d)
		}
	}
	if len(preferred) > 0 {
		top = preferred
	}
	return selectRepresentative(top)
}

// classCandidates splits devices into per-class candidate lists.
// Representatives of overlapping counter groups, and devices without counters,
// land in their own class. Devices that share a counter set with a
// representative of a different class are co-allocatable slices (for example
// MIG slices next to a full GPU) and are kept separately so they can be packed.
func (b *GroupingBuilder) classCandidates(devices []TopologyDevice) (reps map[string][]candidate, slices map[string][]sliceSet) {
	reps = make(map[string][]candidate)
	slices = make(map[string][]sliceSet)

	nonOverlapping, groups := GroupOverlappingDevices(devices)
	for _, d := range nonOverlapping {
		class := b.rules.GetDeviceClassForDevice(d)
		reps[class] = append(reps[class], candidate{dev: d})
	}
	for _, g := range groups {
		rep := b.representative(g.Devices)
		repClass := b.rules.GetDeviceClassForDevice(rep)
		id := counterGroupID(rep)
		reps[repClass] = append(reps[repClass], candidate{dev: rep, group: id})
		bySliceClass := make(map[string][]TopologyDevice)
		for _, d := range g.Devices {
			if class := b.rules.GetDeviceClassForDevice(d); class != repClass {
				bySliceClass[class] = append(bySliceClass[class], d)
			}
		}
		classes := make([]string, 0, len(bySliceClass))
		for class := range bySliceClass {
			classes = append(classes, class)
		}
		sort.Strings(classes)
		for _, class := range classes {
			slices[class] = append(slices[class], sliceSet{devices: bySliceClass[class], group: g.Devices, id: id})
		}
	}
	return reps, slices
}

// sliceSet holds the co-allocatable slices of one counter group together with
// every device of that group, which supplies the counter capacity for packing.
type sliceSet struct {
	devices []TopologyDevice
	group   []TopologyDevice
	id      string
}

func filterByProfile(devices []TopologyDevice, profile string) []TopologyDevice {
	if profile == "" {
		return devices
	}
	var matched []TopologyDevice
	for _, d := range devices {
		if deviceProfile(d) == profile {
			matched = append(matched, d)
		}
	}
	return matched
}

// available returns the candidates that can satisfy one grouping device entry:
// packed slices when the class is a slice class, otherwise representatives.
func available(gd GroupingDevice, reps map[string][]candidate, slices map[string][]sliceSet) []candidate {
	sets, isSlice := slices[gd.Class]
	if !isSlice {
		var result []candidate
		for _, c := range reps[gd.Class] {
			if gd.Profile == "" || deviceProfile(c.dev) == gd.Profile {
				result = append(result, c)
			}
		}
		return result
	}
	var result []candidate
	for _, set := range sets {
		for _, d := range PackDevices(filterByProfile(set.devices, gd.Profile), set.group) {
			result = append(result, candidate{dev: d, group: set.id})
		}
	}
	return result
}

// deviceProfile returns the device's profile attribute in its own driver's
// namespace, matching the CEL selector the DeviceClass manager emits.
func deviceProfile(d TopologyDevice) string {
	if v, ok := d.ExtendedAttributes[baseDriverName(d.DriverName)+"/profile"]; ok && v.StringValue != nil {
		return *v.StringValue
	}
	return ""
}

// nodeHasRequiredClasses checks if the node has enough devices for every
// entry of the grouping, using effective counts for overlapping devices and
// packed counts for co-allocatable slices.
func (b *GroupingBuilder) nodeHasRequiredClasses(
	devices []TopologyDevice,
	grouping DeviceGrouping,
) bool {
	reps, slices := b.classCandidates(devices)
	for _, gd := range grouping.Devices {
		if len(available(gd, reps, slices)) < gd.Count {
			klog.V(1).Infof("Grouping %q: class %q profile %q has fewer than %d usable devices on this node",
				grouping.Name, gd.Class, gd.Profile, gd.Count)
			return false
		}
	}
	return true
}

// findInstances groups devices by the alignment attribute and checks which
// groups satisfy the grouping's device requirements.
func (b *GroupingBuilder) findInstances(
	nodeName string,
	devices []TopologyDevice,
	grouping DeviceGrouping,
	alignment string,
) []GroupingInstance {
	groups := b.groupByAlignment(devices, alignment)

	// Sort group keys for deterministic output
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var instances []GroupingInstance
	idx := 0

	for _, key := range keys {
		groupDevices := groups[key]

		// Overlapping partitions count once (one GPU with SPX/DPX/CPX is one
		// device); co-allocatable slices are packed per class.
		reps, slices := b.classCandidates(groupDevices)

		pool := make(map[string][]candidate)
		for _, gd := range grouping.Devices {
			if k := poolKey(gd); pool[k] == nil {
				pool[k] = available(gd, reps, slices)
			}
		}

		taken := make(map[string]bool)            // devices already placed in an earlier instance
		committedClass := make(map[string]string) // counter group -> class it serves in earlier instances

		for {
			// A counter group serves one class per instance: a full GPU and
			// its own slices exclude each other.
			groupClass := make(map[string]string)
			picked := make(map[string]bool)
			var instanceDevices []TopologyDevice
			complete := true

			for _, gd := range grouping.Devices {
				got := 0
				for _, c := range pool[poolKey(gd)] {
					if got == gd.Count {
						break
					}
					dk := c.dev.DriverName + "/" + c.dev.DeviceName
					if taken[dk] || picked[dk] {
						continue
					}
					if c.group != "" {
						if cls, used := groupClass[c.group]; used && cls != gd.Class {
							continue
						}
						if cls, used := committedClass[c.group]; used && cls != gd.Class {
							continue
						}
					}
					picked[dk] = true
					if c.group != "" {
						groupClass[c.group] = gd.Class
					}
					instanceDevices = append(instanceDevices, c.dev)
					got++
				}
				if got < gd.Count {
					complete = false
					break
				}
			}
			if !complete {
				break
			}
			for dk := range picked {
				taken[dk] = true
			}
			for id, cls := range groupClass {
				committedClass[id] = cls
			}

			inst := b.buildInstance(
				fmt.Sprintf("%s-%s-%s-%d", nodeName, grouping.Name, sanitizeForName(alignment), idx),
				nodeName, grouping, alignment, instanceDevices,
			)
			inst.RailIndex = idx
			instances = append(instances, inst)
			idx++
		}
	}

	return instances
}

func poolKey(gd GroupingDevice) string { return gd.Class + "|" + gd.Profile }

// groupByAlignment groups devices by the specified topology attribute.
func (b *GroupingBuilder) groupByAlignment(
	devices []TopologyDevice,
	alignment string,
) map[string][]TopologyDevice {
	return groupDevicesByAttribute(devices, func(d TopologyDevice) string {
		switch alignment {
		case "pcieRoot":
			if d.PCIeRoot != nil {
				return *d.PCIeRoot
			}
		case "numaNode":
			if d.NUMANode != nil {
				return fmt.Sprintf("%d", *d.NUMANode)
			}
		case "socket":
			if d.Socket != nil {
				return fmt.Sprintf("%d", *d.Socket)
			}
		}
		return ""
	})
}

// buildInstance constructs a GroupingInstance from the devices in a topology group.
func (b *GroupingBuilder) buildInstance(
	name, nodeName string,
	grouping DeviceGrouping,
	alignment string,
	devices []TopologyDevice,
) GroupingInstance {
	inst := GroupingInstance{
		Name:               name,
		NodeName:           nodeName,
		GroupingName:       grouping.Name,
		Alignment:          alignment,
		DeviceCounts:       make(map[string]int),
		DeviceCapacity:     make(map[string]map[string]string),
		DeviceProfiles:     make(map[string]string),
		Devices:            devices,
		ExtendedAttributes: make(map[string]DeviceAttributeValue),
	}

	numaSet := make(map[int64]bool)
	pcieSet := make(map[string]bool)
	socketSet := make(map[int64]bool)

	// Count actual devices by class
	for _, d := range devices {
		inst.DeviceCounts[b.rules.GetDeviceClassForDevice(d)]++
	}

	// Apply capacity from grouping definition for shared devices
	for _, gd := range grouping.Devices {
		if len(gd.Capacity) > 0 {
			inst.DeviceCapacity[gd.Class] = gd.Capacity
		}
		if gd.Profile != "" {
			inst.DeviceProfiles[gd.Class] = gd.Profile
		}
	}

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
		for k, v := range d.ExtendedAttributes {
			if _, exists := inst.ExtendedAttributes[k]; !exists {
				inst.ExtendedAttributes[k] = v
			}
		}
	}

	for n := range numaSet {
		inst.NUMANodes = append(inst.NUMANodes, n)
	}
	sort.Slice(inst.NUMANodes, func(i, j int) bool { return inst.NUMANodes[i] < inst.NUMANodes[j] })

	for r := range pcieSet {
		inst.PCIeRoots = append(inst.PCIeRoots, r)
	}
	sort.Strings(inst.PCIeRoots)

	for s := range socketSet {
		inst.Sockets = append(inst.Sockets, s)
	}
	sort.Slice(inst.Sockets, func(i, j int) bool { return inst.Sockets[i] < inst.Sockets[j] })

	return inst
}

// collectUsedDevices returns a set of device keys from instances for deduplication.
func (b *GroupingBuilder) collectUsedDevices(instances []GroupingInstance) map[string]bool {
	used := make(map[string]bool)
	for _, inst := range instances {
		for _, d := range inst.Devices {
			key := d.DriverName + "/" + d.DeviceName
			used[key] = true
		}
	}
	return used
}

// filterOutDevices returns devices not in the used set.
func (b *GroupingBuilder) filterOutDevices(
	devices []TopologyDevice,
	used map[string]bool,
) []TopologyDevice {
	var remaining []TopologyDevice
	for _, d := range devices {
		key := d.DriverName + "/" + d.DeviceName
		if !used[key] {
			remaining = append(remaining, d)
		}
	}
	return remaining
}

// sanitizeForName converts a topology attribute name to a DNS-label-safe suffix.
func sanitizeForName(s string) string {
	return strings.ToLower(strings.NewReplacer(
		"/", "-",
		".", "-",
		":", "-",
	).Replace(s))
}

// numaKey returns a stable string key for NUMA node values, used in DeviceClass naming.
func numaKey(numaNodes []int64) string {
	if len(numaNodes) == 0 {
		return ""
	}
	parts := make([]string, len(numaNodes))
	for i, n := range numaNodes {
		parts[i] = strconv.FormatInt(n, 10)
	}
	return strings.Join(parts, "-")
}
