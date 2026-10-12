package controller

import (
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	klog "k8s.io/klog/v2"
	"sigs.k8s.io/yaml"
)

// DeviceGroupingLabel identifies ConfigMaps containing device grouping definitions.
const DeviceGroupingLabel = "nodepartition.dra.k8s.io/device-grouping"

// GroupingDevice specifies a device class and count within a grouping.
type GroupingDevice struct {
	Class    string            `json:"class"`
	Count    int               `json:"count"`
	Capacity map[string]string `json:"capacity,omitempty"`
	// Profile optionally restricts the grouping to devices whose profile
	// attribute equals this value (for example a MIG profile "1g.5gb").
	Profile string `json:"profile,omitempty"`
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
	// Devices lists the device classes and counts required in this grouping.
	Devices []GroupingDevice
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
			gd := GroupingDevice{Class: d.Class, Count: d.Count}
			if d.Capacity != nil {
				gd.Capacity = make(map[string]string, len(d.Capacity))
				for k, v := range d.Capacity {
					gd.Capacity[k] = v
				}
			}
			devicesCopy[i] = gd
		}
		result = append(result, DeviceGrouping{
			Name:      g.Name,
			Alignment: g.Alignment,
			Fallback:  g.Fallback,
			Devices:   devicesCopy,
		})
	}
	return result
}

func parseDeviceGrouping(cm *corev1.ConfigMap) (DeviceGrouping, error) {
	g := DeviceGrouping{}

	name := cm.Data["name"]
	if name == "" {
		return g, fmt.Errorf("missing required field 'name'")
	}
	g.Name = name

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
		for j := 0; j < i; j++ {
			if devices[j].Class == d.Class && devices[j].Profile != d.Profile {
				return g, fmt.Errorf("device[%d] repeats class %q with a different profile; one profile per class is supported", i, d.Class)
			}
		}
	}

	g.Devices = devices
	return g, nil
}
