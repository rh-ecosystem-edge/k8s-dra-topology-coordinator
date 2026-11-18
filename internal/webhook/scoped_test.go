package webhook

import (
	"context"
	"fmt"
	"testing"

	"github.com/rh-ecosystem-edge/k8s-dra-topology-coordinator/internal/controller"
	"github.com/stretchr/testify/require"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func scopedConfig() controller.PartitionConfig {
	c := controller.PartitionConfig{APIVersion: controller.PartitionConfigV1Alpha3, Kind: "PartitionConfig", Mode: controller.OfferingModeContainer}
	for _, name := range []string{"gpu", "nic", "cpu", "memory"} {
		c.SubResources = append(c.SubResources, controller.SubResourceConfig{Name: name, DeviceClass: name + ".example", Count: 1})
	}
	c.Alignments = []controller.AlignmentConfig{{Attribute: controller.AttrPCIeRoot, Scope: "group", Requests: []string{"gpu", "nic"}, Enforcement: controller.EnforcementRequired}, {Attribute: controller.AttrNUMANode, Scope: "group", Requests: []string{"gpu", "cpu", "memory"}, Enforcement: controller.EnforcementRequired}}
	return c
}

func TestScopedSingletonConstraintIsEmitted(t *testing.T) {
	c := scopedConfig()
	c.Alignments = c.Alignments[:1]
	c.Alignments[0].Requests = []string{"gpu"}
	req := resourcev1.DeviceRequest{Name: "bundle", Exactly: &resourcev1.ExactDeviceRequest{Count: 1}}
	_, constraints, _, err := NewClaimExpander(fake.NewSimpleClientset()).expandRequest(req, &c)
	require.NoError(t, err)
	require.Len(t, constraints, 1)
	require.Equal(t, []string{"bundle-gpu"}, constraints[0].Requests)
}

func TestScopedAdmissionCountsAndCallerRemapping(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprint(passthrough), func(t *testing.T) {
			c := scopedConfig()
			if passthrough {
				c.Mode = controller.OfferingModePassthrough
				for i := 0; i < 2; i++ {
					c.SubResources[i].Passthrough = true
					c.SubResources[i].Count = 2
				}
			}
			dc := makePartitionDeviceClass("local", c)
			dc.Labels = map[string]string{controller.LabelManaged: "true"}
			ce := NewClaimExpander(fake.NewSimpleClientset(dc))
			attr := resourcev1.FullyQualifiedName(controller.AttrPCIeRoot)
			claim := &resourcev1.ResourceClaim{Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{Name: "bundle", Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: "local", Count: 2}}}, Constraints: []resourcev1.DeviceConstraint{{Requests: []string{"bundle"}, MatchAttribute: &attr}}}}}
			patches, err := ce.expandClaim(context.Background(), claim)
			require.NoError(t, err)
			var got []resourcev1.DeviceConstraint
			for _, p := range patches {
				if p.Path == "/spec/devices/constraints" {
					got = p.Value.([]resourcev1.DeviceConstraint)
				}
			}
			require.Len(t, got, 5)
			var wantCallerPCI []string
			for bundle := 0; bundle < 2; bundle++ {
				prefix := fmt.Sprintf("bundle-%d", bundle)
				wantPCI := []string{prefix + "-gpu", prefix + "-nic"}
				wantNUMA := []string{prefix + "-gpu", prefix + "-cpu", prefix + "-memory"}
				if passthrough {
					wantPCI = []string{prefix + "-gpu-0", prefix + "-gpu-1", prefix + "-nic-0", prefix + "-nic-1"}
					wantNUMA = []string{prefix + "-gpu-0", prefix + "-gpu-1", prefix + "-cpu", prefix + "-memory"}
				}
				pci, numa := got[1+2*bundle], got[2+2*bundle]
				require.NotNil(t, pci.MatchAttribute)
				require.Equal(t, resourcev1.FullyQualifiedName(controller.AttrPCIeRoot), *pci.MatchAttribute)
				require.ElementsMatch(t, wantPCI, pci.Requests)
				require.NotNil(t, numa.MatchAttribute)
				require.Equal(t, resourcev1.FullyQualifiedName(controller.AttrNUMANode), *numa.MatchAttribute)
				require.ElementsMatch(t, wantNUMA, numa.Requests)
				wantCallerPCI = append(wantCallerPCI, wantPCI...)
			}
			require.NotNil(t, got[0].MatchAttribute)
			require.Equal(t, attr, *got[0].MatchAttribute)
			require.ElementsMatch(t, wantCallerPCI, got[0].Requests)
		})
	}
}

func TestScopedClaimConstraintLimit(t *testing.T) {
	c := scopedConfig()
	dc := makePartitionDeviceClass("local", c)
	dc.Labels = map[string]string{controller.LabelManaged: "true"}
	claim := &resourcev1.ResourceClaim{Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{Name: "bundle", Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: "local", Count: 1}}}, Constraints: make([]resourcev1.DeviceConstraint, resourcev1.DeviceConstraintsMaxSize)}}}
	_, err := NewClaimExpander(fake.NewSimpleClientset(dc)).expandClaim(context.Background(), claim)
	require.Error(t, err)
}
