package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// migRules loads the rules an admin needs for dynamic MIG: info rules for the
// type and profile attributes, and a conditional class for MIG slices.
func migRules(t *testing.T) *TopologyRuleStore {
	t.Helper()
	rules := NewTopologyRuleStore()
	for name, data := range map[string]map[string]string{
		"mig-class": {"attribute": migDriver + "/type", "type": "string", "driver": migDriver, "deviceClass": "mig.nvidia.com", "whenValue": "mig"},
		"gpu-class": {"attribute": migDriver + "/profile", "type": "string", "driver": migDriver},
	} {
		require.NoError(t, rules.LoadFromConfigMap(&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: map[string]string{TopologyRuleLabel: "true"}},
			Data:       data,
		}))
	}
	return rules
}

func TestTopologyRule_WhenValueRequiresDeviceClass(t *testing.T) {
	err := NewTopologyRuleStore().LoadFromConfigMap(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "bad", Labels: map[string]string{TopologyRuleLabel: "true"}},
		Data:       map[string]string{"attribute": migDriver + "/type", "type": "string", "driver": migDriver, "whenValue": "mig"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "whenValue requires deviceClass")
}

func TestTopologyRule_ConditionalClassPerDevice(t *testing.T) {
	rules := migRules(t)
	devices := migGPU(0)

	assert.Equal(t, migDriver, rules.GetDeviceClassForDevice(devices[0]), "full GPU keeps the driver class")
	assert.Equal(t, "mig.nvidia.com", rules.GetDeviceClassForDevice(devices[1]))
	assert.Equal(t, migDriver, rules.GetDeviceClassForDriver(migDriver), "conditional rule must not become the driver default")
}

// migNode builds a node with two GPUs (each: full GPU + seven 1g slices) and
// one RDMA NIC per GPU on the same PCIe root.
func migNode() *TopologyModel {
	node := &NodeTopology{NodeName: "node-1", DevicesByDriver: map[string][]TopologyDevice{}}
	for gpu := 0; gpu < 2; gpu++ {
		node.DevicesByDriver[migDriver] = append(node.DevicesByDriver[migDriver], migGPU(gpu)...)
		nic := makeTestDevice("rdma.example.com", "nic-"+string(rune('0'+gpu)), int64Ptr(0), stringPtr("pci-"+string(rune('0'+gpu))))
		node.DevicesByDriver["rdma.example.com"] = append(node.DevicesByDriver["rdma.example.com"], nic)
	}
	model := NewTopologyModel()
	model.mu.Lock()
	model.nodes = map[string]*NodeTopology{"node-1": node}
	model.mu.Unlock()
	return model
}

func buildMIGGrouping(t *testing.T, count int, profile string) []GroupingInstance {
	t.Helper()
	return buildMIGGroupingWith(t, migRules(t), count, profile)
}

func buildMIGGroupingWith(t *testing.T, rules *TopologyRuleStore, count int, profile string) []GroupingInstance {
	t.Helper()
	builder := NewGroupingBuilder(migNode(), rules)
	results := builder.BuildGroupings([]DeviceGrouping{{
		Name:      "mig-nic",
		Alignment: "pcieRoot",
		Devices: []GroupingDevice{
			{Class: "mig.nvidia.com", Count: count, Profile: profile},
			{Class: "rdma.example.com", Count: 1},
		},
	}})
	if len(results) == 0 {
		return nil
	}
	return results[0].Instances
}

func TestGroupingBuilder_MIGSlicesWithNIC(t *testing.T) {
	instances := buildMIGGrouping(t, 2, "1g.5gb")

	require.Len(t, instances, 2, "one instance per PCIe root; 7 packed slices / 2 per instance = 3, but only one NIC")
	for _, inst := range instances {
		assert.Equal(t, 2, inst.DeviceCounts["mig.nvidia.com"])
		assert.Equal(t, 1, inst.DeviceCounts["rdma.example.com"])
		assert.Zero(t, inst.DeviceCounts[migDriver], "full GPU must not be picked for a slice entry")
		assert.Equal(t, "1g.5gb", inst.DeviceProfiles["mig.nvidia.com"])
	}
}

func TestGroupingBuilder_MIGCountAbovePackedIsUnsatisfiable(t *testing.T) {
	assert.Empty(t, buildMIGGrouping(t, 8, "1g.5gb"), "only 7 1g slices fit one GPU")
}

func TestGroupingBuilder_MIGUnknownProfileIsUnsatisfiable(t *testing.T) {
	assert.Empty(t, buildMIGGrouping(t, 1, "9g.99gb"))
}

func TestGroupingBuilder_MIGWithoutRulesDropsGrouping(t *testing.T) {
	builder := NewGroupingBuilder(migNode(), NewTopologyRuleStore())
	results := builder.BuildGroupings([]DeviceGrouping{{
		Name: "mig-nic", Alignment: "pcieRoot",
		Devices: []GroupingDevice{{Class: "mig.nvidia.com", Count: 1}, {Class: "rdma.example.com", Count: 1}},
	}})
	assert.Empty(t, results)
}

func TestDeviceClassManager_GroupingConfigHasProfileSelector(t *testing.T) {
	rules := migRules(t)
	mgr := NewDeviceClassManager(fake.NewSimpleClientset(), "", rules)

	instances := buildMIGGrouping(t, 2, "1g.5gb")
	require.NotEmpty(t, instances)
	cfg := mgr.buildGroupingAggregateConfig(instances[0])

	var found bool
	for _, sr := range cfg.SubResources {
		if sr.DeviceClass == "mig.nvidia.com" {
			found = true
			assert.Equal(t, 2, sr.Count)
			assert.Contains(t, sr.Selectors, `device.attributes["gpu.nvidia.com"].profile == "1g.5gb"`)
		}
	}
	assert.True(t, found, "mig.nvidia.com sub-resource expected")
}

func TestGroupingBuilder_FullGPUAndOwnSlicesAreExclusive(t *testing.T) {
	// One GPU per PCIe root: a full GPU plus a slice would need the same GPU twice.
	builder := NewGroupingBuilder(migNode(), migRules(t))
	grouping := DeviceGrouping{Name: "full-and-slice", Alignment: "pcieRoot", Devices: []GroupingDevice{
		{Class: migDriver, Count: 1}, {Class: "mig.nvidia.com", Count: 1, Profile: "1g.5gb"},
	}}
	assert.Empty(t, builder.BuildGroupings([]DeviceGrouping{grouping}))

	// Two GPUs on one PCIe root: the full GPU and the slice come from different GPUs.
	node := &NodeTopology{NodeName: "node-1", DevicesByDriver: map[string][]TopologyDevice{}}
	for gpu := 0; gpu < 2; gpu++ {
		for _, d := range migGPU(gpu) {
			d.PCIeRoot = strPtr("pci-shared")
			node.DevicesByDriver[migDriver] = append(node.DevicesByDriver[migDriver], d)
		}
	}
	model := NewTopologyModel()
	model.mu.Lock()
	model.nodes = map[string]*NodeTopology{"node-1": node}
	model.mu.Unlock()

	results := NewGroupingBuilder(model, migRules(t)).BuildGroupings([]DeviceGrouping{grouping})
	require.Len(t, results, 1)
	require.Len(t, results[0].Instances, 1, "the second GPU's full/slice pairing would reuse the first GPU in the other class")
}

func TestGroupingBuilder_RepresentativePrefersDefaultClassOnTie(t *testing.T) {
	builder := NewGroupingBuilder(NewTopologyModel(), migRules(t))
	full := migDevice(0, "gpu-0", "gpu", "", 0, migSlices)
	slice7g := migDevice(0, "a-first-7g", "mig", "7g.40gb", 0, migSlices) // sorts before "gpu-0"
	assert.Equal(t, "gpu-0", builder.representative([]TopologyDevice{slice7g, full}).DeviceName)
}

func TestDeviceProfile_UsesOwnDriverNamespace(t *testing.T) {
	d := migDevice(0, "s", "mig", "1g.5gb", 0, 1)
	d.ExtendedAttributes["other.example.com/profile"] = strAttr("wrong")
	assert.Equal(t, "1g.5gb", deviceProfile(d))
}

func TestParseDeviceGrouping_RejectsClassWithTwoProfiles(t *testing.T) {
	store := NewGroupingStore()
	err := store.LoadFromConfigMap(makeGroupingConfigMap("two-profiles", map[string]string{
		"name": "two-profiles", "alignment": "pcieRoot",
		"devices": "- class: mig.nvidia.com\n  profile: 1g.5gb\n  count: 1\n- class: mig.nvidia.com\n  profile: 2g.10gb\n  count: 1\n",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "different profile")
}

func TestDeviceClassManager_MIGGroupingKeepsDriverMatchConstraint(t *testing.T) {
	rules := migRules(t)
	require.NoError(t, rules.LoadFromConfigMap(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "pcie-match", Labels: map[string]string{TopologyRuleLabel: "true"}},
		Data:       map[string]string{"attribute": AttrPCIeRoot, "type": "string", "driver": migDriver, "constraint": "match"},
	}))
	mgr := NewDeviceClassManager(fake.NewSimpleClientset(), "", rules)

	instances := buildMIGGroupingWith(t, rules, 1, "1g.5gb")
	require.NotEmpty(t, instances)
	cfg := mgr.buildGroupingAggregateConfig(instances[0])
	assert.NotEmpty(t, cfg.Alignments, "match rule for gpu.nvidia.com must apply when only mig.nvidia.com is present")
}
