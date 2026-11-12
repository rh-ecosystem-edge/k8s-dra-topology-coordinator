// Package controller implements the topology coordinator controller that watches
// ResourceSlices from all DRA drivers and computes aligned partition combinations.
package controller

import (
	"fmt"
	"sync"

	resourcev1 "k8s.io/api/resource/v1"
	klog "k8s.io/klog/v2"
)

// Standard topology attribute qualified names.
const (
	AttrNUMANode = "resource.kubernetes.io/numaNode"
	AttrPCIeRoot = "resource.kubernetes.io/pcieRoot"
	AttrPCIBusID = "resource.kubernetes.io/pciBusID"
	AttrSocket   = "nodepartition.dra.k8s.io/socket"
)

// numaNodeAliases lists all known NUMA attribute names published by DRA
// drivers. The coordinator checks each of these when extracting NUMA
// topology from device attributes. The first match wins (standard name
// is checked first via AttrNUMANode above).
var socketAliases = []string{
	"resource.kubernetes.io/cpuSocketID",
	"dra.cpu/cpuSocketID",
	"dra.memory/cpuSocketID",
	"cpuSocketID",
	"socket",
}

var numaNodeAliases = []string{
	"nodepartition.dra.k8s.io/numaNode",
	"dra.net/numaNode",
	"dra.cpu/numaNodeID",
	"dra.memory/numaNode",
	"dra.nvme/numaNode",
	"numaNode",
	"numa",
}

// TopologyDevice represents a single device from a DRA driver's ResourceSlice,
// enriched with its topology attributes.
type TopologyDevice struct {
	// DriverName is the DRA driver that published this device.
	DriverName string
	// DeviceName is the device name within its ResourceSlice.
	DeviceName string
	// NodeName is the Kubernetes node where this device exists.
	NodeName string
	// PoolName is the ResourceSlice pool name.
	PoolName string

	// Standard topology attributes
	NUMANode  *int64
	NUMANodes []int64
	PCIeRoot  *string
	PCIBusID  *string
	PCIeRoots []string
	Socket    *int64

	// Extended attributes from topology rules (attribute qualified name -> value).
	ExtendedAttributes map[string]DeviceAttributeValue

	// Capacity holds consumable capacity from the ResourceSlice device.
	// Key is the capacity name, value is the total quantity string.
	Capacity map[string]string

	// ConsumesCounters records which shared counter sets this device draws from.
	// Devices referencing the same CounterSet are overlapping partitions of the
	// same physical resource (KEP-4815 partitionable devices).
	ConsumesCounters []resourcev1.DeviceCounterConsumption
	CounterCapacity  map[string]map[string]int64
}

// DeviceAttributeValue holds a typed attribute value.
type DeviceAttributeValue struct {
	IntValue    *int64
	StringValue *string
	BoolValue   *bool
}

// String returns a string representation for logging.
func (v DeviceAttributeValue) String() string {
	if v.IntValue != nil {
		return fmt.Sprintf("%d", *v.IntValue)
	}
	if v.StringValue != nil {
		return *v.StringValue
	}
	if v.BoolValue != nil {
		return fmt.Sprintf("%t", *v.BoolValue)
	}
	return "<nil>"
}

// NodeTopology holds all topology devices discovered on a single node across all drivers.
type NodeTopology struct {
	NodeName string
	// Devices grouped by driver name.
	DevicesByDriver map[string][]TopologyDevice
}

// AllDevices returns a flat list of all devices on this node.
func (nt *NodeTopology) AllDevices() []TopologyDevice {
	var all []TopologyDevice
	for _, devices := range nt.DevicesByDriver {
		all = append(all, devices...)
	}
	return all
}

// DevicesForDriver returns devices published by a specific driver on this node.
func (nt *NodeTopology) DevicesForDriver(driverName string) []TopologyDevice {
	var result []TopologyDevice
	for _, devices := range nt.DevicesByDriver {
		for _, d := range devices {
			if d.DriverName == driverName {
				result = append(result, d)
			}
		}
	}
	return result
}

// rawSliceData holds the raw information from a ResourceSlice needed
// for re-extraction when topology rules change.
type rawSliceData struct {
	SliceName      string
	DriverName     string
	NodeName       string
	PoolName       string
	Generation     int64
	SliceCount     int64
	SharedCounters []resourcev1.CounterSet
	Devices        []resourcev1.Device
}

// TopologyModel is the cross-driver topology model built from ResourceSlices.
// It is thread-safe for concurrent read/write access.
type TopologyModel struct {
	mu sync.RWMutex
	// nodes maps node name -> NodeTopology.
	nodes map[string]*NodeTopology
	// rules are the active topology rules from ConfigMaps.
	rules []TopologyRule
	// rawSlices stores raw slice data for re-extraction when rules change.
	// Keyed by "nodeName/driverName/poolName".
	rawSlices map[string]rawSliceData
}

// NewTopologyModel creates an empty topology model.
func NewTopologyModel() *TopologyModel {
	return &TopologyModel{
		nodes:     make(map[string]*NodeTopology),
		rawSlices: make(map[string]rawSliceData),
	}
}

// SetRules updates the topology rules and re-extracts all devices
// with the new rules to ensure attribute mappings are up to date.
func (m *TopologyModel) SetRules(rules []TopologyRule) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rules = rules
	m.reextractAllLocked()
}

// reextractAllLocked re-extracts all devices from stored raw slice data
// using the current rules. Must be called with m.mu held.
func (m *TopologyModel) reextractAllLocked() {
	// Clear existing topology
	m.nodes = make(map[string]*NodeTopology)

	// Re-extract all devices with current rules
	type poolKey struct{ driver, pool string }
	groups := make(map[poolKey][]rawSliceData)
	for _, raw := range m.rawSlices {
		key := poolKey{raw.DriverName, raw.PoolName}
		groups[key] = append(groups[key], raw)
	}
	for _, slices := range groups {
		hasSharedCounters := false
		for _, raw := range slices {
			if len(raw.SharedCounters) > 0 {
				hasSharedCounters = true
				break
			}
		}
		if !hasSharedCounters {
			for _, raw := range slices {
				m.applySliceDataLocked(raw, nil)
			}
			continue
		}
		var generation int64
		for _, raw := range slices {
			if raw.Generation > generation {
				generation = raw.Generation
			}
		}
		var current []rawSliceData
		var expected int64
		capacities := make(map[string]map[string]int64)
		valid := true
		for _, raw := range slices {
			if raw.Generation != generation {
				continue
			}
			if expected != 0 && expected != raw.SliceCount {
				valid = false
			}
			expected = raw.SliceCount
			current = append(current, raw)
			for _, set := range raw.SharedCounters {
				values := make(map[string]int64, len(set.Counters))
				for name, counter := range set.Counters {
					values[name] = counter.Value.Value()
				}
				if previous, exists := capacities[set.Name]; exists {
					if !equalCounterCapacity(previous, values) {
						valid = false
					}
				} else {
					capacities[set.Name] = values
				}
			}
		}
		if !valid || expected <= 0 || int64(len(current)) != expected {
			continue
		}
		for _, raw := range current {
			m.applySliceDataLocked(raw, capacities)
		}
	}
}

func equalCounterCapacity(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

// GetRules returns the current topology rules.
func (m *TopologyModel) GetRules() []TopologyRule {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]TopologyRule, len(m.rules))
	copy(result, m.rules)
	return result
}

// UpdateFromResourceSlice processes a ResourceSlice and updates the topology model.
// It extracts devices with their topology attributes and stores them by node.
func (m *TopologyModel) UpdateFromResourceSlice(slice *resourcev1.ResourceSlice) {
	if slice == nil {
		return
	}

	driverName := slice.Spec.Driver
	nodeName := ""
	if slice.Spec.NodeName != nil {
		nodeName = *slice.Spec.NodeName
	}
	if nodeName == "" {
		klog.V(4).Infof("Skipping ResourceSlice %s/%s: no node name", slice.Namespace, slice.Name)
		return
	}

	poolName := slice.Spec.Pool.Name

	m.mu.Lock()
	defer m.mu.Unlock()

	// Store a deep copy of raw data for re-extraction when rules change.
	// The informer cache may reuse the slice object, so we must not hold references.
	// Key by slice name (not driver/pool) because multiple slices can share a pool.
	rawKey := slice.Name
	devicesCopy := make([]resourcev1.Device, len(slice.Spec.Devices))
	for i, d := range slice.Spec.Devices {
		devicesCopy[i] = *d.DeepCopy()
	}
	sharedCopy := make([]resourcev1.CounterSet, len(slice.Spec.SharedCounters))
	for i, set := range slice.Spec.SharedCounters {
		sharedCopy[i] = resourcev1.CounterSet{Name: set.Name, Counters: make(map[string]resourcev1.Counter, len(set.Counters))}
		for name, counter := range set.Counters {
			sharedCopy[i].Counters[name] = counter
		}
	}
	m.rawSlices[rawKey] = rawSliceData{
		SliceName:      slice.Name,
		DriverName:     driverName,
		NodeName:       nodeName,
		PoolName:       poolName,
		Generation:     slice.Spec.Pool.Generation,
		SliceCount:     slice.Spec.Pool.ResourceSliceCount,
		SharedCounters: sharedCopy,
		Devices:        devicesCopy,
	}

	// Extract and apply
	m.reextractAllLocked()

	klog.V(4).Infof("Updated topology model: node=%s driver=%s pool=%s devices=%d",
		nodeName, driverName, poolName, len(slice.Spec.Devices))
}

// applySliceDataLocked extracts topology devices from raw slice data
// and updates the node topology. Must be called with m.mu held.
func (m *TopologyModel) applySliceDataLocked(raw rawSliceData, capacities map[string]map[string]int64) {
	var devices []TopologyDevice
	for _, device := range raw.Devices {
		td := m.extractTopologyDevice(raw.DriverName, device.Name, raw.NodeName, raw.PoolName, device.Attributes)
		// Extract capacity info
		if device.Capacity != nil {
			td.Capacity = make(map[string]string)
			for capName, capSpec := range device.Capacity {
				td.Capacity[string(capName)] = capSpec.Value.String()
			}
		}
		// Extract counter consumption for KEP-4815 partitionable devices
		if len(device.ConsumesCounters) > 0 {
			td.ConsumesCounters = make([]resourcev1.DeviceCounterConsumption, len(device.ConsumesCounters))
			copy(td.ConsumesCounters, device.ConsumesCounters)
			td.CounterCapacity = make(map[string]map[string]int64)
			for _, cc := range device.ConsumesCounters {
				if cap, ok := capacities[cc.CounterSet]; ok {
					td.CounterCapacity[cc.CounterSet] = cap
				}
			}
		}
		devices = append(devices, td)
	}

	nt, ok := m.nodes[raw.NodeName]
	if !ok {
		nt = &NodeTopology{
			NodeName:        raw.NodeName,
			DevicesByDriver: make(map[string][]TopologyDevice),
		}
		m.nodes[raw.NodeName] = nt
	}

	nt.DevicesByDriver[raw.SliceName] = devices
}

// RemoveResourceSlice removes devices from a ResourceSlice that was deleted.
func (m *TopologyModel) RemoveResourceSlice(slice *resourcev1.ResourceSlice) {
	if slice == nil {
		return
	}

	nodeName := ""
	if slice.Spec.NodeName != nil {
		nodeName = *slice.Spec.NodeName
	}
	if nodeName == "" {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Remove raw data
	delete(m.rawSlices, slice.Name)
	m.reextractAllLocked()

	klog.V(4).Infof("Removed from topology model: node=%s slice=%s", nodeName, slice.Name)
}

// GetNodeTopology returns a deep copy of the topology for a specific node.
func (m *TopologyModel) GetNodeTopology(nodeName string) *NodeTopology {
	m.mu.RLock()
	defer m.mu.RUnlock()
	nt, ok := m.nodes[nodeName]
	if !ok {
		return nil
	}
	return nt.deepCopy()
}

// GetAllNodes returns the names of all nodes with topology information.
func (m *TopologyModel) GetAllNodes() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	nodes := make([]string, 0, len(m.nodes))
	for name := range m.nodes {
		nodes = append(nodes, name)
	}
	return nodes
}

// GetNodeTopologies returns a deep copy of all node topologies.
func (m *TopologyModel) GetNodeTopologies() map[string]*NodeTopology {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make(map[string]*NodeTopology, len(m.nodes))
	for k, v := range m.nodes {
		result[k] = v.deepCopy()
	}
	return result
}

// deepCopy returns a deep copy of the NodeTopology.
func (nt *NodeTopology) deepCopy() *NodeTopology {
	cp := &NodeTopology{
		NodeName:        nt.NodeName,
		DevicesByDriver: make(map[string][]TopologyDevice, len(nt.DevicesByDriver)),
	}
	for driver, devices := range nt.DevicesByDriver {
		devicesCopy := make([]TopologyDevice, len(devices))
		for i, d := range devices {
			devicesCopy[i] = d.deepCopy()
		}
		cp.DevicesByDriver[driver] = devicesCopy
	}
	return cp
}

// deepCopy returns a deep copy of the TopologyDevice.
func (td TopologyDevice) deepCopy() TopologyDevice {
	cp := td
	if td.NUMANode != nil {
		v := *td.NUMANode
		cp.NUMANode = &v
	}
	if td.PCIeRoot != nil {
		v := *td.PCIeRoot
		cp.PCIeRoot = &v
	}
	if td.PCIBusID != nil {
		v := *td.PCIBusID
		cp.PCIBusID = &v
	}
	if td.CounterCapacity != nil {
		cp.CounterCapacity = make(map[string]map[string]int64, len(td.CounterCapacity))
		for set, counters := range td.CounterCapacity {
			values := make(map[string]int64, len(counters))
			for name, value := range counters {
				values[name] = value
			}
			cp.CounterCapacity[set] = values
		}
	}
	if td.Socket != nil {
		v := *td.Socket
		cp.Socket = &v
	}
	cp.ExtendedAttributes = make(map[string]DeviceAttributeValue, len(td.ExtendedAttributes))
	for k, v := range td.ExtendedAttributes {
		cp.ExtendedAttributes[k] = v
	}
	return cp
}

// IsConstraintSatisfiable checks whether at least one node in the topology model
// has a group of devices sharing the same value for the given attribute where
// every driver in driverCounts has at least the required number of devices.
// This is used by the webhook to decide whether to emit "preferred" constraints.
func (m *TopologyModel) IsConstraintSatisfiable(attribute string, driverCounts map[string]int) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, nt := range m.nodes {
		if m.isConstraintSatisfiableOnNode(nt, attribute, driverCounts) {
			return true
		}
	}
	return false
}

// isConstraintSatisfiableOnNode checks whether a single node has at least one
// group (same attribute value) that satisfies all driver count requirements.
func (m *TopologyModel) isConstraintSatisfiableOnNode(nt *NodeTopology, attribute string, driverCounts map[string]int) bool {
	// Collect all devices on this node, grouped by the attribute value.
	// Key: attribute value as string, Value: map[driverName]count
	groups := make(map[string]map[string]int)

	for _, devices := range nt.DevicesByDriver {
		for _, dev := range devices {
			val := deviceAttributeValueString(dev, attribute)
			if val == "" {
				continue
			}
			if groups[val] == nil {
				groups[val] = make(map[string]int)
			}
			groups[val][dev.DriverName]++
		}
	}

	// Check if any group satisfies all driver count requirements.
	for _, driverMap := range groups {
		satisfied := true
		for driver, needed := range driverCounts {
			if driverMap[driver] < needed {
				satisfied = false
				break
			}
		}
		if satisfied {
			return true
		}
	}
	return false
}

// deviceAttributeValueString returns the string representation of the device's
// value for the given attribute, checking standard attributes and extended attributes.
func deviceAttributeValueString(dev TopologyDevice, attribute string) string {
	vals := deviceAttributeValues(dev, attribute)
	if len(vals) > 0 {
		return vals[0]
	}
	return ""
}

func deviceAttributeValues(dev TopologyDevice, attribute string) []string {
	switch attribute {
	case AttrNUMANode:
		if len(dev.NUMANodes) > 0 {
			vals := make([]string, len(dev.NUMANodes))
			for i, n := range dev.NUMANodes {
				vals[i] = fmt.Sprintf("%d", n)
			}
			return vals
		}
		if dev.NUMANode != nil {
			return []string{fmt.Sprintf("%d", *dev.NUMANode)}
		}
	case AttrPCIeRoot:
		if len(dev.PCIeRoots) > 0 {
			return dev.PCIeRoots
		}
		if dev.PCIeRoot != nil {
			return []string{*dev.PCIeRoot}
		}
	case AttrPCIBusID:
		if dev.PCIBusID != nil {
			return []string{*dev.PCIBusID}
		}
	case AttrSocket:
		if dev.Socket != nil {
			return []string{fmt.Sprintf("%d", *dev.Socket)}
		}
	default:
		if v, ok := dev.ExtendedAttributes[attribute]; ok {
			return []string{v.String()}
		}
	}
	return nil
}

func deviceHasAttribute(dev TopologyDevice, attribute string) bool {
	switch attribute {
	case AttrNUMANode:
		return dev.NUMANode != nil || len(dev.NUMANodes) > 0
	case AttrPCIeRoot:
		return dev.PCIeRoot != nil || len(dev.PCIeRoots) > 0
	case AttrPCIBusID:
		return dev.PCIBusID != nil
	case AttrSocket:
		return dev.Socket != nil
	default:
		_, ok := dev.ExtendedAttributes[attribute]
		return ok
	}
}

func deviceHasScalarAttribute(dev TopologyDevice, attribute string) bool {
	switch attribute {
	case AttrNUMANode:
		return dev.NUMANode != nil
	case AttrPCIeRoot:
		return dev.PCIeRoot != nil
	case AttrPCIBusID:
		return dev.PCIBusID != nil
	case AttrSocket:
		return dev.Socket != nil
	default:
		_, ok := dev.ExtendedAttributes[attribute]
		return ok
	}
}

// extractTopologyDevice extracts topology attributes from a device's attributes.
func (m *TopologyModel) extractTopologyDevice(
	driverName, deviceName, nodeName, poolName string,
	attrs map[resourcev1.QualifiedName]resourcev1.DeviceAttribute,
) TopologyDevice {
	td := TopologyDevice{
		DriverName:         driverName,
		DeviceName:         deviceName,
		NodeName:           nodeName,
		PoolName:           poolName,
		ExtendedAttributes: make(map[string]DeviceAttributeValue),
	}

	for qn, attr := range attrs {
		name := string(qn)
		qualifiedName := qualifiedAttribute(driverName, name)

		// Check standard attribute names first
		switch name {
		case AttrNUMANode:
			if attr.IntValue != nil {
				td.NUMANode = attr.IntValue
			}
			continue
		case AttrPCIeRoot:
			if attr.StringValue != nil {
				td.PCIeRoot = attr.StringValue
			}
			continue
		case AttrPCIBusID:
			if attr.StringValue != nil {
				td.PCIBusID = attr.StringValue
			}
			continue
		case AttrSocket:
			if attr.IntValue != nil {
				td.Socket = attr.IntValue
			}
			continue
		}

		// Check NUMA node aliases — only set if not already found
		if td.NUMANode == nil {
			for _, alias := range numaNodeAliases {
				if name == alias && attr.IntValue != nil {
					td.NUMANode = attr.IntValue
					break
				}
			}
		}

		// Check socket aliases — only set if not already found
		if td.Socket == nil {
			for _, alias := range socketAliases {
				if name == alias && attr.IntValue != nil {
					td.Socket = attr.IntValue
					break
				}
			}
		}

		// Check topology rules for driver-specific attribute mappings
		for _, rule := range m.rules {
			if qualifiedName != qualifiedAttribute(rule.Driver, rule.Attribute) || driverName != rule.Driver {
				continue
			}

			// If the rule maps to a standard topology attribute, apply the mapping
			switch rule.MapsTo {
			case MapsToNUMANode:
				if attr.IntValue != nil {
					td.NUMANode = attr.IntValue
				}
			case MapsToPCIeRoot:
				if attr.StringValue != nil {
					td.PCIeRoot = attr.StringValue
				}
			case MapsToSocket:
				if attr.IntValue != nil {
					td.Socket = attr.IntValue
				}
			}

			// Also store as extended attribute for propagation to partition devices
			td.ExtendedAttributes[qualifiedName] = deviceAttributeFromDRA(attr)
		}
	}

	return td
}

// deviceAttributeFromDRA converts a DRA DeviceAttribute to our DeviceAttributeValue.
func deviceAttributeFromDRA(attr resourcev1.DeviceAttribute) DeviceAttributeValue {
	v := DeviceAttributeValue{}
	if attr.IntValue != nil {
		v.IntValue = attr.IntValue
	}
	if attr.StringValue != nil {
		v.StringValue = attr.StringValue
	}
	if attr.BoolValue != nil {
		v.BoolValue = attr.BoolValue
	}
	return v
}
