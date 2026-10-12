package controller

import (
	"fmt"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	klog "k8s.io/klog/v2"
)

// TopologyRuleLabel is the label that identifies ConfigMaps containing topology rules.
const TopologyRuleLabel = "nodepartition.dra.k8s.io/topology-rule"

// PartitioningMode defines how the coordinator uses a topology attribute during partition computation.
type PartitioningMode string

const (
	// PartitioningGroup means devices must share the same value of this attribute
	// to be grouped into a partition.
	PartitioningGroup PartitioningMode = "group"
	// PartitioningInfo means the attribute is included in partition device attributes
	// but does not constrain grouping.
	PartitioningInfo PartitioningMode = "info"
)

// ConstraintMode defines how the attribute is used in combined claims.
type ConstraintMode string

const (
	// ConstraintMatch means a matchAttribute constraint is added to the combined claim.
	ConstraintMatch ConstraintMode = "match"
	// ConstraintNone means no constraint is added.
	ConstraintNone ConstraintMode = "none"
)

// EnforcementMode defines whether a constraint is hard (must be satisfied) or
// best-effort (applied only when the cluster can satisfy it).
type EnforcementMode string

const (
	// EnforcementRequired means the constraint is always emitted. If it cannot
	// be satisfied, the pod will not schedule.
	EnforcementRequired EnforcementMode = "required"
	// EnforcementPreferred means the constraint is emitted only when the
	// topology model indicates it can be satisfied on at least one node.
	EnforcementPreferred EnforcementMode = "preferred"
)

// StandardTopologyAttribute identifies a standard topology attribute for mapping.
type StandardTopologyAttribute string

const (
	MapsToNUMANode StandardTopologyAttribute = "numaNode"
	MapsToPCIeRoot StandardTopologyAttribute = "pcieRoot"
	MapsToSocket   StandardTopologyAttribute = "socket"
	MapsToNone     StandardTopologyAttribute = ""
)

// TopologyRule represents a vendor-specific topology attribute rule loaded from a ConfigMap.
type TopologyRule struct {
	// Name is the ConfigMap name this rule was loaded from.
	Name string
	// Attribute is the fully qualified attribute name (e.g., "gpu.nvidia.com/nvlinkDomain").
	Attribute string
	// Type is the attribute value type: "int", "string", or "bool".
	Type string
	// Driver is the DRA driver that publishes this attribute.
	Driver string
	// MapsTo optionally maps this driver-specific attribute to a standard topology attribute.
	// When set, the coordinator treats this attribute as the specified standard attribute
	// (numaNode, pcieRoot, socket) for topology grouping purposes.
	MapsTo StandardTopologyAttribute
	// Partitioning defines how the coordinator uses this attribute during partition computation.
	Partitioning PartitioningMode
	// Constraint defines how the attribute is used in combined claims.
	Constraint ConstraintMode
	// Enforcement defines whether the constraint is hard or best-effort.
	// "required" (default) always emits the constraint.
	// "preferred" only emits the constraint when satisfiable.
	Enforcement EnforcementMode
	// FallbackAttribute specifies a looser attribute to use when the primary
	// constraint is unsatisfiable for a specific partition. For example, if
	// Attribute is pcieRoot and FallbackAttribute is "numaNode", partitions
	// where pcieRoot alignment is impossible fall back to numaNode alignment
	// (via per-driver CEL selectors already in place).
	FallbackAttribute string
	// DeviceClass overrides the DeviceClass name used in partition sub-resources
	// for this driver. By default the driver name is used, but some drivers
	// publish multiple device types under one driver name with separate
	// DeviceClasses (e.g., gpu.nvidia.com publishes "gpu" and "vfio" types
	// with DeviceClasses "gpu.nvidia.com" and "vfio.gpu.nvidia.com").
	DeviceClass string
	// WhenValue makes DeviceClass conditional per device: the override applies
	// only to devices whose Attribute equals WhenValue (for example
	// attribute gpu.nvidia.com/type, whenValue mig, deviceClass mig.nvidia.com).
	// It also marks those devices as co-allocatable slices rather than
	// exclusive modes of one physical resource.
	WhenValue string
	// Description is a human-readable description of the attribute.
	Description string
}

// TopologyRuleStore loads and manages topology rules from ConfigMaps.
type TopologyRuleStore struct {
	mu    sync.RWMutex
	rules map[string]TopologyRule // keyed by ConfigMap name
}

// NewTopologyRuleStore creates an empty rule store.
func NewTopologyRuleStore() *TopologyRuleStore {
	return &TopologyRuleStore{
		rules: make(map[string]TopologyRule),
	}
}

// LoadFromConfigMap extracts a topology rule from a ConfigMap.
// Returns an error if the ConfigMap is missing required fields.
func (s *TopologyRuleStore) LoadFromConfigMap(cm *corev1.ConfigMap) error {
	if cm == nil {
		return fmt.Errorf("configmap cannot be nil")
	}

	// Verify the label is present
	if cm.Labels[TopologyRuleLabel] != "true" {
		return fmt.Errorf("configmap %s/%s is not a topology rule (missing label)", cm.Namespace, cm.Name)
	}

	rule, err := parseTopologyRule(cm)
	if err != nil {
		return fmt.Errorf("failed to parse topology rule from %s/%s: %w", cm.Namespace, cm.Name, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules[cm.Namespace+"/"+cm.Name] = rule

	klog.Infof("Loaded topology rule %q: attribute=%s driver=%s mapsTo=%s partitioning=%s constraint=%s enforcement=%s fallback=%s deviceClass=%s",
		rule.Name, rule.Attribute, rule.Driver, rule.MapsTo, rule.Partitioning, rule.Constraint, rule.Enforcement, rule.FallbackAttribute, rule.DeviceClass)
	return nil
}

// RemoveConfigMap removes the topology rule associated with a ConfigMap.
func (s *TopologyRuleStore) RemoveConfigMap(namespace, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := namespace + "/" + name
	if _, ok := s.rules[key]; ok {
		delete(s.rules, key)
		klog.Infof("Removed topology rule from %s", key)
	}
}

// GetRules returns all loaded topology rules.
func (s *TopologyRuleStore) GetRules() []TopologyRule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]TopologyRule, 0, len(s.rules))
	for _, rule := range s.rules {
		result = append(result, rule)
	}
	return result
}

// GetGroupingRules returns only rules with PartitioningGroup mode.
func (s *TopologyRuleStore) GetGroupingRules() []TopologyRule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []TopologyRule
	for _, rule := range s.rules {
		if rule.Partitioning == PartitioningGroup {
			result = append(result, rule)
		}
	}
	return result
}

// GetMatchConstraintRules returns only rules with ConstraintMatch mode.
// GetMatchConstraintRules returns only rules with ConstraintMatch mode.
// If no explicit pcieRoot match rule exists, a built-in default is appended
// that aligns devices by resource.kubernetes.io/pcieRoot with numaNode fallback.
func (s *TopologyRuleStore) GetMatchConstraintRules() []TopologyRule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []TopologyRule
	for _, rule := range s.rules {
		if rule.Constraint == ConstraintMatch {
			result = append(result, rule)
		}
	}
	hasPCIeRoot := false
	for _, rule := range result {
		if rule.Attribute == AttrPCIeRoot {
			hasPCIeRoot = true
			break
		}
	}
	if !hasPCIeRoot {
		result = append(result, TopologyRule{
			Name:              "builtin-pcieroot-default",
			Attribute:         AttrPCIeRoot,
			Type:              "string",
			Constraint:        ConstraintMatch,
			Enforcement:       EnforcementRequired,
			FallbackAttribute: AttrNUMANode,
		})
	}
	return result
}

// GetNUMAAttributeForDriver returns the fully qualified attribute name that a
// specific driver uses for NUMA node topology. It searches topology rules for
// a rule matching the driver with mapsTo=numaNode.
// Returns ("", false) if no rule is found for the driver.
func (s *TopologyRuleStore) GetNUMAAttributeForDriver(driverName string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, rule := range s.rules {
		if rule.Driver == driverName && rule.MapsTo == MapsToNUMANode {
			return rule.Attribute, true
		}
	}
	return "", false
}

// GetDeviceClassForDriver returns the DeviceClass name to use for a driver's
// sub-resources. If a topology rule specifies a deviceClass override, that is
// returned. Otherwise the driver name itself is used as the DeviceClass.
func (s *TopologyRuleStore) GetDeviceClassForDriver(driverName string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, rule := range s.rules {
		if rule.Driver == driverName && rule.DeviceClass != "" && rule.WhenValue == "" {
			return rule.DeviceClass
		}
	}
	return driverName
}

// GetDeviceClassForDevice returns the DeviceClass for one device. A rule with
// whenValue wins when the device's attribute matches; otherwise the
// driver-level class applies.
func (s *TopologyRuleStore) GetDeviceClassForDevice(d TopologyDevice) string {
	driver := baseDriverName(d.DriverName)

	s.mu.RLock()
	for _, rule := range s.rules {
		if rule.Driver == driver && rule.DeviceClass != "" && rule.WhenValue != "" &&
			deviceAttributeValueString(d, rule.Attribute) == rule.WhenValue {
			s.mu.RUnlock()
			return rule.DeviceClass
		}
	}
	s.mu.RUnlock()

	return s.GetDeviceClassForDriver(driver)
}

// BuildNUMACELSelector generates a CEL expression that pins devices from a
// specific driver to the given NUMA node value(s), using the driver's own
// attribute namespace. For example:
//
//	attribute="gpu.amd.com/numaNode", values=[0]
//	→ device.attributes["gpu.amd.com"].numaNode == 0
//
//	attribute="dra.cpu/numaNodeID", values=[0, 1]
//	→ device.attributes["dra.cpu"].numaNodeID == 0 || device.attributes["dra.cpu"].numaNodeID == 1
func BuildNUMACELSelector(attribute string, numaValues []int64) string {
	parts := strings.SplitN(attribute, "/", 2)
	if len(parts) != 2 {
		return ""
	}
	domain := parts[0]
	name := parts[1]

	attrRef := fmt.Sprintf(`device.attributes["%s"].%s`, domain, name)
	hasGuard := fmt.Sprintf(`has(%s)`, attrRef)

	if len(numaValues) == 1 {
		return fmt.Sprintf(`%s && %s == %d`, hasGuard, attrRef, numaValues[0])
	}

	// Multiple NUMA values: has() && (v1 || v2 || ...)
	var clauses []string
	for _, v := range numaValues {
		clauses = append(clauses, fmt.Sprintf(`%s == %d`, attrRef, v))
	}
	return fmt.Sprintf(`%s && (%s)`, hasGuard, strings.Join(clauses, " || "))
}

// parseTopologyRule parses a TopologyRule from a ConfigMap's data fields.
func parseTopologyRule(cm *corev1.ConfigMap) (TopologyRule, error) {
	rule := TopologyRule{
		Name: cm.Name,
	}

	// Required fields
	attr, ok := cm.Data["attribute"]
	if !ok || attr == "" {
		return rule, fmt.Errorf("missing required field 'attribute'")
	}
	rule.Attribute = attr

	attrType, ok := cm.Data["type"]
	if !ok || attrType == "" {
		return rule, fmt.Errorf("missing required field 'type'")
	}
	if attrType != "int" && attrType != "string" && attrType != "bool" {
		return rule, fmt.Errorf("invalid type %q: must be int, string, or bool", attrType)
	}
	rule.Type = attrType

	driver, ok := cm.Data["driver"]
	if !ok || driver == "" {
		return rule, fmt.Errorf("missing required field 'driver'")
	}
	rule.Driver = driver

	// Optional fields with defaults
	mapsTo := cm.Data["mapsTo"]
	switch StandardTopologyAttribute(mapsTo) {
	case MapsToNUMANode, MapsToPCIeRoot, MapsToSocket, MapsToNone:
		rule.MapsTo = StandardTopologyAttribute(mapsTo)
	default:
		return rule, fmt.Errorf("invalid mapsTo %q: must be numaNode, pcieRoot, socket, or empty", mapsTo)
	}

	partitioning := cm.Data["partitioning"]
	switch PartitioningMode(partitioning) {
	case PartitioningGroup:
		rule.Partitioning = PartitioningGroup
	case PartitioningInfo:
		rule.Partitioning = PartitioningInfo
	case "":
		rule.Partitioning = PartitioningInfo // default
	default:
		return rule, fmt.Errorf("invalid partitioning mode %q: must be group or info", partitioning)
	}

	constraint := cm.Data["constraint"]
	switch ConstraintMode(constraint) {
	case ConstraintMatch:
		rule.Constraint = ConstraintMatch
	case ConstraintNone:
		rule.Constraint = ConstraintNone
	case "":
		rule.Constraint = ConstraintNone // default
	default:
		return rule, fmt.Errorf("invalid constraint mode %q: must be match or none", constraint)
	}

	enforcement := cm.Data["enforcement"]
	switch EnforcementMode(enforcement) {
	case EnforcementRequired:
		rule.Enforcement = EnforcementRequired
	case EnforcementPreferred:
		rule.Enforcement = EnforcementPreferred
	case "":
		rule.Enforcement = EnforcementRequired // default
	default:
		return rule, fmt.Errorf("invalid enforcement mode %q: must be required or preferred", enforcement)
	}

	rule.FallbackAttribute = cm.Data["fallbackAttribute"]

	rule.DeviceClass = cm.Data["deviceClass"]

	rule.WhenValue = cm.Data["whenValue"]
	if rule.WhenValue != "" && rule.DeviceClass == "" {
		return rule, fmt.Errorf("whenValue requires deviceClass")
	}

	rule.Description = cm.Data["description"]

	return rule, nil
}
