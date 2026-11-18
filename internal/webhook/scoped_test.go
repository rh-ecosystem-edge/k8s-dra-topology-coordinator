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
			wantPCI := []string{"bundle-0-gpu", "bundle-0-nic"}
			wantNUMA := []string{"bundle-0-gpu", "bundle-0-cpu", "bundle-0-memory"}
			if passthrough {
				wantPCI = []string{"bundle-0-gpu-0", "bundle-0-gpu-1", "bundle-0-nic-0", "bundle-0-nic-1"}
				wantNUMA = []string{"bundle-0-gpu-0", "bundle-0-gpu-1", "bundle-0-cpu", "bundle-0-memory"}
			}
			require.ElementsMatch(t, wantPCI, got[1].Requests)
			require.ElementsMatch(t, wantNUMA, got[2].Requests)
			for _, name := range got[0].Requests {
				require.NotContains(t, name, "cpu")
				require.NotContains(t, name, "memory")
			}
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
