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
	// Mode is the intended consumer mode for this offering.
	Mode OfferingMode
	// Rail marks an explicit GPU+NIC PCIe-root rail offering.
	Rail bool
	// Intent contains optional discovery metadata from the source grouping.
	Intent IntentMetadata
	// AutoDetected marks instances of coordinator-derived groupings.
	AutoDetected bool
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
	// DeviceCapacity maps driver name -> capacity for shared devices.
	DeviceCapacity map[string]map[string]string
	// Requests maps each topology class to the explicit driver contract the
	// grouping ConfigMap declares for it.
	Requests map[string]RequestContract
	// Devices lists the topology devices in this instance.
	Devices []TopologyDevice

	// ExtendedAttributes from topology rules.
	ExtendedAttributes map[string]DeviceAttributeValue

	// InstanceIndex is the generic deterministic index of this instance.
	InstanceIndex int
}

// RequestContract is the driver-owned contract for one topology class in a
// grouping: which DeviceClass expanded claims request, plus the selectors and
// opaque configuration copied onto the generated request.
type RequestContract struct {
	// Class is the requested DeviceClass; empty means the topology class itself.
	Class string `json:"class,omitempty"`
	// Driver is the driver whose devices Class allocates; empty means the
	// driver that publishes the topology class.
	Driver    string        `json:"driver,omitempty"`
	Selectors []string      `json:"selectors,omitempty"`
	Opaque    *OpaqueConfig `json:"opaque,omitempty"`
	// Passthrough marks the class as explicitly selected for VM passthrough.
	Passthrough bool `json:"passthrough,omitempty"`
}

// requestClass returns the DeviceClass expanded claims use for a topology class.
func (inst GroupingInstance) requestClass(class string) string {
	if c := inst.Requests[class].Class; c != "" {
		return c
	}
	return class
}

func (r RequestContract) clone() RequestContract {
	r.Selectors = append([]string(nil), r.Selectors...)
	if r.Opaque != nil {
		r.Opaque = &OpaqueConfig{
			Driver:     r.Opaque.Driver,
			Parameters: append([]byte(nil), r.Opaque.Parameters...),
		}
	}
	return r
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

// nodeHasRequiredClasses checks if the node has at least one device
// of each class required by the grouping, using effective counts
// to handle overlapping partitionable devices.
func (b *GroupingBuilder) nodeHasRequiredClasses(
	devices []TopologyDevice,
	grouping DeviceGrouping,
) bool {
	// Use effective counting to deduplicate overlapping partitions.
	effectiveCounts := EffectiveDeviceCount(devices)

	classCounts := make(map[string]int)
	for driver, count := range effectiveCounts {
		driverClass := b.rules.GetDeviceClassForDriver(driver)
		classCounts[driverClass] += count
	}

	for _, gd := range grouping.Devices {
		if classCounts[gd.Class] < gd.Count {
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

		// Filter to effective devices (deduplicate overlapping partitions)
		// so that one GPU with SPX/DPX/CPX is counted once, not 11 times.
		effectiveGroupDevices := FilterToEffectiveDevices(groupDevices)

		// Count from the filtered device list so classCounts stays consistent
		// with devicesByClass (both derived from effectiveGroupDevices).
		classCounts := make(map[string]int)
		for _, d := range effectiveGroupDevices {
			driverClass := b.rules.GetDeviceClassForDriver(baseDriverName(d.DriverName))
			classCounts[driverClass]++
		}

		// Check how many instances this group can satisfy
		// (limited by the scarcest required device class)
		maxInstances := -1
		for _, gd := range grouping.Devices {
			available := classCounts[gd.Class]
			possible := available / gd.Count
			if maxInstances < 0 || possible < maxInstances {
				maxInstances = possible
			}
		}

		if maxInstances <= 0 {
			continue
		}

		// Index effective devices by class for per-instance allocation
		devicesByClass := make(map[string][]TopologyDevice)
		for _, d := range effectiveGroupDevices {
			driverClass := b.rules.GetDeviceClassForDriver(baseDriverName(d.DriverName))
			devicesByClass[driverClass] = append(devicesByClass[driverClass], d)
		}

		consumed := make(map[string]int) // class -> count already allocated

		for i := 0; i < maxInstances; i++ {
			var instanceDevices []TopologyDevice
			for _, gd := range grouping.Devices {
				start := consumed[gd.Class]
				end := start + gd.Count
				instanceDevices = append(instanceDevices, devicesByClass[gd.Class][start:end]...)
				consumed[gd.Class] = end
			}

			inst := b.buildInstance(
				fmt.Sprintf("%s-%s-%s-%d", nodeName, grouping.Name, sanitizeForName(alignment), idx),
				nodeName, grouping, alignment, instanceDevices,
			)
			inst.InstanceIndex = idx
			instances = append(instances, inst)
			idx++
		}
	}

	return instances
}

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
		Mode:               grouping.Mode,
		Rail:               grouping.Rail,
		Intent:             grouping.Intent,
		AutoDetected:       grouping.AutoDetected,
		Alignment:          alignment,
		DeviceCounts:       make(map[string]int),
		DeviceCapacity:     make(map[string]map[string]string),
		Requests:           make(map[string]RequestContract),
		Devices:            devices,
		ExtendedAttributes: make(map[string]DeviceAttributeValue),
	}

	numaSet := make(map[int64]bool)
	pcieSet := make(map[string]bool)
	socketSet := make(map[int64]bool)

	// Count actual devices by class
	for _, d := range devices {
		driverClass := b.rules.GetDeviceClassForDriver(baseDriverName(d.DriverName))
		inst.DeviceCounts[driverClass]++
	}

	// Apply capacity from grouping definition for shared devices
	for _, gd := range grouping.Devices {
		requestClass := gd.RequestClass
		if requestClass == "" {
			requestClass = gd.Class
		}
		inst.Requests[gd.Class] = RequestContract{
			Class:       requestClass,
			Driver:      gd.RequestDriver,
			Selectors:   gd.Selectors,
			Opaque:      gd.Opaque,
			Passthrough: gd.Passthrough,
		}.clone()
		if len(gd.Capacity) > 0 {
			inst.DeviceCapacity[gd.Class] = gd.Capacity
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
// It is not a general DNS-label sanitizer (see naming.Sanitize): it only replaces "/", "." and ":".
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
