package controller

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	klog "k8s.io/klog/v2"
	"sigs.k8s.io/yaml"
)

// DeviceGroupingLabel identifies ConfigMaps containing device grouping definitions.
const DeviceGroupingLabel = "nodepartition.dra.k8s.io/device-grouping"

// OfferingMode controls whether an offering is intended for ordinary
// containers or explicit VM passthrough.
type OfferingMode string

const (
	OfferingModeContainer   OfferingMode = "container"
	OfferingModePassthrough OfferingMode = "passthrough"
)

// GroupingDevice specifies a device class and count within a grouping.
type GroupingDevice struct {
	Class       string            `json:"class"`
	Count       int               `json:"count"`
	Capacity    map[string]string `json:"capacity,omitempty"`
	Passthrough bool              `json:"passthrough,omitempty"`
}

// IntentMetadata adds optional human-readable metadata to a device grouping.
type IntentMetadata struct {
	DisplayName   string
	Description   string
	IntentLevel   string
	Categories    []string
	WorkloadTypes []string
	Visibility    string
}

// IsOffering reports whether the grouping has any intent metadata.
func (m IntentMetadata) IsOffering() bool {
	return m.DisplayName != "" || m.Description != "" || m.IntentLevel != "" ||
		len(m.Categories) > 0 || len(m.WorkloadTypes) > 0 || m.Visibility != ""
}

// DeviceGrouping defines a named combination of devices that should be
// co-located at a specific topology alignment level.
type DeviceGrouping struct {
	// Name is the grouping identifier, used in DeviceClass naming.
	Name string
	// Alignment is the preferred topology attribute for co-location
	// (e.g., "pcieRoot", "numaNode", "socket").
	Alignment string
	// Fallback is an optional looser alignment attribute to use when
	// the preferred alignment is unsatisfiable for some device instances.
	Fallback string
	// Mode is the intended consumer mode for this offering.
	Mode OfferingMode
	// Rail marks an explicit GPU+NIC PCIe-root rail offering.
	Rail bool
	// Devices lists the device classes and counts required in this grouping.
	Devices []GroupingDevice
	// Intent contains optional offering metadata from the source ConfigMap.
	Intent IntentMetadata

	sourceConfigMap string
}

// GroupingStore loads and manages device groupings from ConfigMaps.
type GroupingStore struct {
	mu        sync.RWMutex
	groupings map[string]DeviceGrouping // keyed by namespace/name
}

// NewGroupingStore creates an empty grouping store.
func NewGroupingStore() *GroupingStore {
	return &GroupingStore{
		groupings: make(map[string]DeviceGrouping),
	}
}

// LoadFromConfigMap extracts a device grouping from a ConfigMap.
func (s *GroupingStore) LoadFromConfigMap(cm *corev1.ConfigMap) error {
	if cm == nil {
		return fmt.Errorf("configmap cannot be nil")
	}

	if cm.Labels[DeviceGroupingLabel] != "true" {
		return fmt.Errorf("configmap %s/%s is not a device grouping (missing label)", cm.Namespace, cm.Name)
	}

	grouping, err := parseDeviceGrouping(cm)
	if err != nil {
		return fmt.Errorf("failed to parse device grouping from %s/%s: %w", cm.Namespace, cm.Name, err)
	}
	grouping.sourceConfigMap = cm.Namespace + "/" + cm.Name

	s.mu.Lock()
	defer s.mu.Unlock()
	s.groupings[cm.Namespace+"/"+cm.Name] = grouping

	klog.Infof("Loaded device grouping %q: alignment=%s fallback=%s devices=%d",
		grouping.Name, grouping.Alignment, grouping.Fallback, len(grouping.Devices))
	return nil
}

// RemoveConfigMap removes the device grouping associated with a ConfigMap.
func (s *GroupingStore) RemoveConfigMap(namespace, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := namespace + "/" + name
	if _, ok := s.groupings[key]; ok {
		delete(s.groupings, key)
		klog.Infof("Removed device grouping from %s", key)
	}
}

// GetGroupings returns all loaded device groupings (deep copied).
func (s *GroupingStore) GetGroupings() []DeviceGrouping {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]DeviceGrouping, 0, len(s.groupings))
	for _, g := range s.groupings {
		devicesCopy := make([]GroupingDevice, len(g.Devices))
		for i, d := range g.Devices {
			gd := GroupingDevice{Class: d.Class, Count: d.Count, Passthrough: d.Passthrough}
			if d.Capacity != nil {
				gd.Capacity = make(map[string]string, len(d.Capacity))
				for k, v := range d.Capacity {
					gd.Capacity[k] = v
				}
			}
			devicesCopy[i] = gd
		}
		intentCopy := g.Intent
		intentCopy.Categories = append([]string(nil), g.Intent.Categories...)
		intentCopy.WorkloadTypes = append([]string(nil), g.Intent.WorkloadTypes...)
		result = append(result, DeviceGrouping{
			Name:            g.Name,
			Alignment:       g.Alignment,
			Fallback:        g.Fallback,
			Mode:            g.Mode,
			Rail:            g.Rail,
			Devices:         devicesCopy,
			Intent:          intentCopy,
			sourceConfigMap: g.sourceConfigMap,
		})
	}
	return result
}

func parseDeviceGrouping(cm *corev1.ConfigMap) (DeviceGrouping, error) {
	g := DeviceGrouping{}
	g.Mode = OfferingMode(cm.Data["mode"])
	if g.Mode == "" {
		g.Mode = OfferingModeContainer
	}
	if g.Mode != OfferingModeContainer && g.Mode != OfferingModePassthrough {
		return g, fmt.Errorf("invalid mode %q: must be container or passthrough", g.Mode)
	}
	if rail := cm.Data["rail"]; rail != "" {
		parsed, err := strconv.ParseBool(rail)
		if err != nil {
			return g, fmt.Errorf("invalid rail %q: must be true or false", rail)
		}
		g.Rail = parsed
	}

	name := cm.Data["name"]
	if name == "" {
		return g, fmt.Errorf("missing required field 'name'")
	}
	g.Name = name
	sanitizedName := sanitizeForName(name)
	if len(sanitizedName) > 56 {
		return g, fmt.Errorf("invalid field 'name': sanitized value %q exceeds 56 characters", sanitizedName)
	}
	if problems := validation.IsDNS1123Label(sanitizedName); len(problems) > 0 {
		return g, fmt.Errorf("invalid field 'name': sanitized value %q is not a DNS-1123 label: %s",
			sanitizedName, strings.Join(problems, "; "))
	}

	intent := IntentMetadata{
		DisplayName: cm.Data["displayName"],
		Description: cm.Data["description"],
	}
	if utf8.RuneCountInString(intent.DisplayName) > 63 {
		return g, fmt.Errorf("invalid field 'displayName': must be at most 63 characters")
	}
	if utf8.RuneCountInString(intent.Description) > 1024 {
		return g, fmt.Errorf("invalid field 'description': must be at most 1024 characters")
	}

	if intent.IntentLevel = cm.Data["intentLevel"]; intent.IntentLevel != "" {
		switch intent.IntentLevel {
		case "workload", "capability", "hardware-specific":
		default:
			return g, fmt.Errorf("invalid intentLevel %q: must be workload, capability, or hardware-specific", intent.IntentLevel)
		}
	}

	var err error
	if intent.Categories, err = parseCommaSeparatedValues("categories", cm.Data["categories"]); err != nil {
		return g, err
	}
	if intent.WorkloadTypes, err = parseCommaSeparatedValues("workloadTypes", cm.Data["workloadTypes"]); err != nil {
		return g, err
	}

	if intent.Visibility = cm.Data["visibility"]; intent.Visibility != "" {
		switch intent.Visibility {
		case "public", "internal":
		default:
			return g, fmt.Errorf("invalid visibility %q: must be public or internal", intent.Visibility)
		}
	}
	g.Intent = intent

	alignment := cm.Data["alignment"]
	if alignment == "" {
		return g, fmt.Errorf("missing required field 'alignment'")
	}
	switch alignment {
	case "pcieRoot", "numaNode", "socket":
		g.Alignment = alignment
	default:
		return g, fmt.Errorf("invalid alignment %q: must be pcieRoot, numaNode, or socket", alignment)
	}

	fallback := cm.Data["fallback"]
	if fallback != "" {
		switch fallback {
		case "pcieRoot", "numaNode", "socket":
			g.Fallback = fallback
		default:
			return g, fmt.Errorf("invalid fallback %q: must be pcieRoot, numaNode, or socket", fallback)
		}
	}

	devicesYAML := cm.Data["devices"]
	if devicesYAML == "" {
		return g, fmt.Errorf("missing required field 'devices'")
	}

	var devices []GroupingDevice
	if err := yaml.Unmarshal([]byte(devicesYAML), &devices); err != nil {
		return g, fmt.Errorf("failed to parse devices YAML: %w", err)
	}

	if len(devices) == 0 {
		return g, fmt.Errorf("devices list cannot be empty")
	}

	for i, d := range devices {
		if d.Class == "" {
			return g, fmt.Errorf("device[%d] missing required field 'class'", i)
		}
		if d.Count < 1 {
			return g, fmt.Errorf("device[%d] count must be >= 1, got %d", i, d.Count)
		}
	}
	if g.Rail {
		if g.Alignment != "pcieRoot" {
			return g, fmt.Errorf("rail offerings require alignment pcieRoot")
		}
		if len(devices) != 2 {
			return g, fmt.Errorf("rail offerings require exactly two devices")
		}
		for i, d := range devices {
			if d.Count != 1 {
				return g, fmt.Errorf("rail device[%d] requires count 1, got %d", i, d.Count)
			}
		}
	}

	g.Devices = devices
	return g, nil
}

func parseCommaSeparatedValues(field, value string) ([]string, error) {
	const (
		maxEntries     = 16
		maxEntryLength = 63
	)

	seen := make(map[string]struct{})
	var values []string
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if utf8.RuneCountInString(entry) > maxEntryLength {
			return nil, fmt.Errorf("invalid %s entry %q: must be at most %d characters", field, entry, maxEntryLength)
		}
		if _, ok := seen[entry]; ok {
			continue
		}
		seen[entry] = struct{}{}
		values = append(values, entry)
		if len(values) > maxEntries {
			return nil, fmt.Errorf("invalid %s: must contain at most %d entries", field, maxEntries)
		}
	}
	return values, nil
}
