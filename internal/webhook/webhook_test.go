package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/rh-ecosystem-edge/k8s-dra-topology-coordinator/internal/controller"
)

// makePartitionDeviceClass creates a DeviceClass with a PartitionConfig opaque config.
func makePartitionDeviceClass(name string, config controller.PartitionConfig) *resourcev1.DeviceClass {
	configJSON, _ := json.Marshal(config)
	return makePartitionDeviceClassJSON(name, configJSON)
}

func makePartitionDeviceClassJSON(name string, configJSON []byte) *resourcev1.DeviceClass {
	return &resourcev1.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: resourcev1.DeviceClassSpec{
			Config: []resourcev1.DeviceClassConfiguration{
				{
					DeviceConfiguration: resourcev1.DeviceConfiguration{
						Opaque: &resourcev1.OpaqueDeviceConfiguration{
							Driver:     driverName,
							Parameters: runtime.RawExtension{Raw: configJSON},
						},
					},
				},
			},
		},
	}
}

// makeRegularDeviceClass creates a DeviceClass without a PartitionConfig.
func makeRegularDeviceClass(name string) *resourcev1.DeviceClass {
	return &resourcev1.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: resourcev1.DeviceClassSpec{
			Selectors: []resourcev1.DeviceSelector{
				{
					CEL: &resourcev1.CELDeviceSelector{
						Expression: `device.driver == "some.driver"`,
					},
				},
			},
		},
	}
}

// makeAdmissionReview builds an AdmissionReview for a ResourceClaim.
func makeAdmissionReview(claim *resourcev1.ResourceClaim, operation admissionv1.Operation) admissionv1.AdmissionReview { //nolint:unparam // test helper, operation may vary in future tests
	claimJSON, _ := json.Marshal(claim)
	return admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Request: &admissionv1.AdmissionRequest{
			UID: "test-uid",
			Resource: metav1.GroupVersionResource{
				Group:    "resource.k8s.io",
				Version:  "v1",
				Resource: "resourceclaims",
			},
			Operation: operation,
			Object: runtime.RawExtension{
				Raw: claimJSON,
			},
		},
	}
}

// sendAdmissionReview sends an AdmissionReview to the webhook handler and returns the response.
func sendAdmissionReview(t *testing.T, handler http.Handler, review admissionv1.AdmissionReview) admissionv1.AdmissionReview {
	t.Helper()
	body, err := json.Marshal(review)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code)

	var resp admissionv1.AdmissionReview
	err = json.Unmarshal(recorder.Body.Bytes(), &resp)
	require.NoError(t, err)
	require.NotNil(t, resp.Response)

	return resp
}

func TestNonPartitionClaimPassesThrough(t *testing.T) {
	regularClass := makeRegularDeviceClass("regular-class")
	client := fake.NewSimpleClientset(regularClass)
	expander := NewClaimExpander(client)

	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "default",
		},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{
					{
						Name: "my-device",
						Exactly: &resourcev1.ExactDeviceRequest{
							DeviceClassName: "regular-class",
							Count:           1,
						},
					},
				},
			},
		},
	}

	review := makeAdmissionReview(claim, admissionv1.Create)
	resp := sendAdmissionReview(t, expander.Handler(), review)

	assert.True(t, resp.Response.Allowed)
	assert.Nil(t, resp.Response.PatchType, "non-partition claim should not be mutated")
	assert.Empty(t, resp.Response.Patch, "non-partition claim should have no patch")
}

func TestPartitionClaimIsExpanded(t *testing.T) {
	partConfig := controller.PartitionConfig{
		Kind: "PartitionConfig",
		SubResources: []controller.SubResourceConfig{
			{DeviceClass: "gpu.nvidia.com", Count: 4},
			{DeviceClass: "rdma.mellanox.com", Count: 4},
		},
		Alignments: []controller.AlignmentConfig{
			{
				Attribute: "resource.kubernetes.io/pcieRoot",
				Requests:  []string{"gpu.nvidia.com", "rdma.mellanox.com"},
			},
			{
				Attribute: controller.AttrNUMANode,
				Requests:  []string{"gpu.nvidia.com", "rdma.mellanox.com"},
			},
		},
	}

	partClass := makePartitionDeviceClass("test-partition", partConfig)
	client := fake.NewSimpleClientset(partClass)
	expander := NewClaimExpander(client)

	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "partition-claim",
			Namespace: "default",
		},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{
					{
						Name: "partition",
						Exactly: &resourcev1.ExactDeviceRequest{
							DeviceClassName: "test-partition",
							Count:           1,
						},
					},
				},
			},
		},
	}

	review := makeAdmissionReview(claim, admissionv1.Create)
	resp := sendAdmissionReview(t, expander.Handler(), review)

	assert.True(t, resp.Response.Allowed)
	require.NotNil(t, resp.Response.PatchType)
	assert.Equal(t, admissionv1.PatchTypeJSONPatch, *resp.Response.PatchType)
	require.NotEmpty(t, resp.Response.Patch)

	var patches []jsonPatch
	err := json.Unmarshal(resp.Response.Patch, &patches)
	require.NoError(t, err)

	// Should have at least a requests replacement and constraints addition
	require.GreaterOrEqual(t, len(patches), 2)

	// Find the requests patch
	var requestsPatch *jsonPatch
	var constraintsPatch *jsonPatch
	for i := range patches {
		if patches[i].Path == "/spec/devices/requests" {
			requestsPatch = &patches[i]
		}
		if patches[i].Path == "/spec/devices/constraints" {
			constraintsPatch = &patches[i]
		}
	}

	require.NotNil(t, requestsPatch, "should have a requests patch")
	require.NotNil(t, constraintsPatch, "should have a constraints patch")

	// Verify expanded requests
	reqBytes, err := json.Marshal(requestsPatch.Value)
	require.NoError(t, err)

	var expandedRequests []resourcev1.DeviceRequest
	err = json.Unmarshal(reqBytes, &expandedRequests)
	require.NoError(t, err)

	assert.Len(t, expandedRequests, 2, "should have 2 sub-resource requests")

	requestNames := make(map[string]bool)
	for _, r := range expandedRequests {
		assert.Contains(t, r.Name, "partition-")
		requestNames[r.Name] = true
		require.NotNil(t, r.Exactly)
		assert.Equal(t, int64(4), r.Exactly.Count)
	}

	// Verify constraints
	conBytes, err := json.Marshal(constraintsPatch.Value)
	require.NoError(t, err)

	var expandedConstraints []resourcev1.DeviceConstraint
	err = json.Unmarshal(conBytes, &expandedConstraints)
	require.NoError(t, err)

	assert.GreaterOrEqual(t, len(expandedConstraints), 1, "should have at least one constraint")

	// Verify constraint references the expanded request names
	for _, c := range expandedConstraints {
		require.NotNil(t, c.MatchAttribute)
		for _, reqName := range c.Requests {
			assert.True(t, requestNames[reqName], "constraint should reference an expanded request name: %s", reqName)
		}
	}
}

func TestContainerOfferingDoesNotInjectVFIO(t *testing.T) {
	partConfig := controller.PartitionConfig{
		Kind: "PartitionConfig",
		Mode: controller.OfferingModeContainer,
		SubResources: []controller.SubResourceConfig{
			{DeviceClass: "rdma.mellanox.com", Count: 1},
		},
	}
	client := fake.NewSimpleClientset(makePartitionDeviceClass("container-rdma", partConfig))
	expander := NewClaimExpander(client)
	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "container-claim", Namespace: "default"},
		Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
			Name:    "partition",
			Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: "container-rdma", Count: 1},
		}}}},
	}

	resp := sendAdmissionReview(t, expander.Handler(), makeAdmissionReview(claim, admissionv1.Create))
	require.True(t, resp.Response.Allowed)
	var patches []jsonPatch
	require.NoError(t, json.Unmarshal(resp.Response.Patch, &patches))
	for _, patch := range patches {
		assert.NotEqual(t, "/spec/devices/config", patch.Path, "container offering must not inject VFIO")
	}
}

func TestPassthroughOfferingWithoutExplicitConfigDoesNotInjectVFIO(t *testing.T) {
	partConfig := controller.PartitionConfig{
		Kind: "PartitionConfig",
		Mode: controller.OfferingModePassthrough,
		SubResources: []controller.SubResourceConfig{
			{DeviceClass: "sriovnetwork.k8snetworkplumbingwg.io", Count: 1, Passthrough: true},
		},
	}
	client := fake.NewSimpleClientset(makePartitionDeviceClass("vfio-rdma-no-config", partConfig))
	expander := NewClaimExpander(client)
	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "vfio-claim-no-config", Namespace: "default"},
		Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
			Name:    "partition",
			Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: "vfio-rdma-no-config", Count: 1},
		}}}},
	}

	resp := sendAdmissionReview(t, expander.Handler(), makeAdmissionReview(claim, admissionv1.Create))
	require.True(t, resp.Response.Allowed)
	var patches []jsonPatch
	require.NoError(t, json.Unmarshal(resp.Response.Patch, &patches))
	for _, patch := range patches {
		assert.NotEqual(t, "/spec/devices/config", patch.Path,
			"passthrough alone must not synthesize a driver configuration")
	}
}

func TestPassthroughOfferingCopiesExplicitOpaqueConfigToExpandedRequests(t *testing.T) {
	configJSON := []byte(`{
		"kind":"PartitionConfig",
		"mode":"passthrough",
		"subResources":[{
			"deviceClass":"gpu.amd.com",
			"count":1,
			"passthrough":true,
			"opaque":{
				"driver":"gpu.resource.amd.com",
				"parameters":{"apiVersion":"gpu.resource.amd.com/v1alpha1","kind":"VfioDeviceConfig"}
			}
		}]
	}`)
	client := fake.NewSimpleClientset(makePartitionDeviceClassJSON("amd-gim-vfio", configJSON))
	expander := NewClaimExpander(client)
	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "amd-gim-claim", Namespace: "default"},
		Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
			Name:    "partition",
			Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: "amd-gim-vfio", Count: 1},
		}}}},
	}

	patches, err := expander.expandClaim(context.Background(), claim)
	require.NoError(t, err)
	var configPatch *jsonPatch
	for i := range patches {
		if patches[i].Path == "/spec/devices/config" {
			configPatch = &patches[i]
			break
		}
	}
	require.NotNil(t, configPatch)
	var configs []resourcev1.DeviceClaimConfiguration
	configBytes, err := json.Marshal(configPatch.Value)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(configBytes, &configs))
	require.Len(t, configs, 1)
	require.NotNil(t, configs[0].Opaque)
	assert.Equal(t, "gpu.resource.amd.com", configs[0].Opaque.Driver)
	assert.Equal(t, []string{"partition-gpu-amd-com"}, configs[0].Requests)
	assert.JSONEq(t,
		`{"apiVersion":"gpu.resource.amd.com/v1alpha1","kind":"VfioDeviceConfig"}`,
		string(configs[0].Opaque.Parameters.Raw))
}

func TestManagedEmptyPartitionConfigIsRejected(t *testing.T) {
	dc := makePartitionDeviceClassJSON("empty-partition", []byte(`{"kind":"PartitionConfig","mode":"container","subResources":[]}`))
	dc.Labels = map[string]string{controller.CoordinatorDriverName + "/managed": "true"}
	client := fake.NewSimpleClientset(dc)
	expander := NewClaimExpander(client)
	claim := &resourcev1.ResourceClaim{Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
		Name:    "partition",
		Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: dc.Name, Count: 1},
	}}}}}

	resp := sendAdmissionReview(t, expander.Handler(), makeAdmissionReview(claim, admissionv1.Create))
	assert.False(t, resp.Response.Allowed)
	assert.Contains(t, resp.Response.Result.Message, "subResources")
}

func TestManagedPartitionConfigRejectsDuplicateRequestClasses(t *testing.T) {
	partConfig := controller.PartitionConfig{
		Kind: "PartitionConfig",
		Mode: controller.OfferingModePassthrough,
		SubResources: []controller.SubResourceConfig{
			{DeviceClass: "vfio.gpu.nvidia.com", Count: 1, Passthrough: true},
			{DeviceClass: "vfio.gpu.nvidia.com", Count: 1, Passthrough: true},
		},
	}
	dc := makePartitionDeviceClass("duplicate-request-class", partConfig)
	dc.Labels = map[string]string{controller.CoordinatorDriverName + "/managed": "true"}
	client := fake.NewSimpleClientset(dc)
	expander := NewClaimExpander(client)
	claim := &resourcev1.ResourceClaim{Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
		Name:    "partition",
		Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: dc.Name, Count: 1},
	}}}}}

	resp := sendAdmissionReview(t, expander.Handler(), makeAdmissionReview(claim, admissionv1.Create))
	assert.False(t, resp.Response.Allowed)
	assert.Contains(t, resp.Response.Result.Message, "duplicate deviceClass")
}

func TestManagedPartitionConfigRejectsUnknownAlignmentRequest(t *testing.T) {
	partConfig := controller.PartitionConfig{
		Kind:         "PartitionConfig",
		SubResources: []controller.SubResourceConfig{{DeviceClass: "gpu.amd.com", Count: 1}},
		Alignments: []controller.AlignmentConfig{{
			Attribute: controller.AttrPCIeRoot,
			Requests:  []string{"gpu.amd.com", "missing.class"},
		}},
	}
	dc := makePartitionDeviceClass("unknown-alignment-request", partConfig)
	dc.Labels = map[string]string{controller.CoordinatorDriverName + "/managed": "true"}
	expander := NewClaimExpander(fake.NewSimpleClientset(dc))
	claim := &resourcev1.ResourceClaim{Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
		Name:    "partition",
		Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: dc.Name, Count: 1},
	}}}}}

	resp := sendAdmissionReview(t, expander.Handler(), makeAdmissionReview(claim, admissionv1.Create))
	assert.False(t, resp.Response.Allowed)
	assert.Contains(t, resp.Response.Result.Message, "unknown deviceClass")
}

func TestExpansionRejectsCollidingGeneratedRequestNames(t *testing.T) {
	partConfig := controller.PartitionConfig{
		Kind: "PartitionConfig",
		SubResources: []controller.SubResourceConfig{
			{DeviceClass: "foo.a", Count: 1},
			{DeviceClass: "foo-a", Count: 1},
		},
	}
	dc := makePartitionDeviceClass("colliding-request-names", partConfig)
	dc.Labels = map[string]string{controller.CoordinatorDriverName + "/managed": "true"}
	expander := NewClaimExpander(fake.NewSimpleClientset(dc))
	claim := &resourcev1.ResourceClaim{Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
		Name:    "partition",
		Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: dc.Name, Count: 1},
	}}}}}

	resp := sendAdmissionReview(t, expander.Handler(), makeAdmissionReview(claim, admissionv1.Create))
	assert.False(t, resp.Response.Allowed)
	assert.Contains(t, resp.Response.Result.Message, "duplicated")
}

func TestEquivalentExistingOpaqueConfigIsNotDuplicated(t *testing.T) {
	partConfig := controller.PartitionConfig{
		Kind: "PartitionConfig",
		Mode: controller.OfferingModePassthrough,
		SubResources: []controller.SubResourceConfig{{
			DeviceClass: "gpu.amd.com",
			Count:       1,
			Passthrough: true,
			Opaque: &controller.OpaqueConfig{
				Driver:     "gpu.resource.amd.com",
				Parameters: json.RawMessage(`{"apiVersion":"gpu.resource.amd.com/v1alpha1","kind":"VfioDeviceConfig"}`),
			},
		}},
	}
	dc := makePartitionDeviceClass("existing-config", partConfig)
	client := fake.NewSimpleClientset(dc)
	expander := NewClaimExpander(client)
	claim := &resourcev1.ResourceClaim{Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{
		Requests: []resourcev1.DeviceRequest{{
			Name:    "partition",
			Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: dc.Name, Count: 1},
		}},
		Config: []resourcev1.DeviceClaimConfiguration{{
			DeviceConfiguration: resourcev1.DeviceConfiguration{Opaque: &resourcev1.OpaqueDeviceConfiguration{
				Driver:     "gpu.resource.amd.com",
				Parameters: runtime.RawExtension{Raw: []byte(`{"kind":"VfioDeviceConfig","apiVersion":"gpu.resource.amd.com/v1alpha1"}`)},
			}},
		}},
	}}}

	patches, err := expander.expandClaim(context.Background(), claim)
	require.NoError(t, err)
	for _, patch := range patches {
		assert.NotEqual(t, "/spec/devices/config", patch.Path)
	}
}

func TestExpansionRemapsExistingConstraintAndConfigReferences(t *testing.T) {
	partConfig := controller.PartitionConfig{
		Kind:         "PartitionConfig",
		SubResources: []controller.SubResourceConfig{{DeviceClass: "gpu.amd.com", Count: 1}},
	}
	dc := makePartitionDeviceClass("remap-references", partConfig)
	client := fake.NewSimpleClientset(dc)
	expander := NewClaimExpander(client)
	attribute := resourcev1.FullyQualifiedName(controller.AttrPCIeRoot)
	claim := &resourcev1.ResourceClaim{Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{
		Requests: []resourcev1.DeviceRequest{{
			Name:    "partition",
			Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: dc.Name, Count: 1},
		}},
		Constraints: []resourcev1.DeviceConstraint{{
			Requests:       []string{"partition"},
			MatchAttribute: &attribute,
		}},
		Config: []resourcev1.DeviceClaimConfiguration{{
			Requests: []string{"partition"},
			DeviceConfiguration: resourcev1.DeviceConfiguration{Opaque: &resourcev1.OpaqueDeviceConfiguration{
				Driver:     "example.com/driver",
				Parameters: runtime.RawExtension{Raw: []byte(`{"kind":"ExampleConfig"}`)},
			}},
		}},
	}}}

	patches, err := expander.expandClaim(context.Background(), claim)
	require.NoError(t, err)
	var constraintsPatch, configsPatch *jsonPatch
	for i := range patches {
		switch patches[i].Path {
		case "/spec/devices/constraints":
			constraintsPatch = &patches[i]
		case "/spec/devices/config":
			configsPatch = &patches[i]
		}
	}
	require.NotNil(t, constraintsPatch)
	require.NotNil(t, configsPatch)
	constraintBytes, err := json.Marshal(constraintsPatch.Value)
	require.NoError(t, err)
	assert.NotContains(t, string(constraintBytes), `"partition"`)
	assert.Contains(t, string(constraintBytes), "partition-gpu-amd-com")
	configBytes, err := json.Marshal(configsPatch.Value)
	require.NoError(t, err)
	assert.NotContains(t, string(configBytes), `"partition"`)
	assert.Contains(t, string(configBytes), "partition-gpu-amd-com")
}

func TestUserConstraintNarrowedToAlignedSubRequests(t *testing.T) {
	partConfig := controller.PartitionConfig{
		Kind: "PartitionConfig",
		SubResources: []controller.SubResourceConfig{
			{DeviceClass: "gpu.amd.com", Count: 1},
			{DeviceClass: "nic.example.com", Count: 1},
			{DeviceClass: "cpu.example.com", Count: 1},
		},
		Alignments: []controller.AlignmentConfig{{
			Attribute:   controller.AttrPCIeRoot,
			Requests:    []string{"gpu.amd.com", "nic.example.com"},
			Enforcement: controller.EnforcementRequired,
		}},
	}
	dc := makePartitionDeviceClass("narrow-constraint", partConfig)
	expander := NewClaimExpander(fake.NewSimpleClientset(dc))
	attribute := resourcev1.FullyQualifiedName(controller.AttrPCIeRoot)
	claim := &resourcev1.ResourceClaim{Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{
		Requests: []resourcev1.DeviceRequest{{
			Name:    "vm",
			Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: dc.Name, Count: 1},
		}},
		Constraints: []resourcev1.DeviceConstraint{{Requests: []string{"vm"}, MatchAttribute: &attribute}},
	}}}

	patches, err := expander.expandClaim(context.Background(), claim)
	require.NoError(t, err)
	var constraints []resourcev1.DeviceConstraint
	for _, patch := range patches {
		if patch.Path == "/spec/devices/constraints" {
			var ok bool
			constraints, ok = patch.Value.([]resourcev1.DeviceConstraint)
			require.True(t, ok)
		}
	}
	require.NotEmpty(t, constraints)
	// The user's constraint is the first entry; generated alignments follow.
	assert.ElementsMatch(t, []string{"vm-gpu-amd-com", "vm-nic-example-com"}, constraints[0].Requests)
}

func TestPassthroughConfigsHaveStableOrder(t *testing.T) {
	partConfig := controller.PartitionConfig{
		Kind: "PartitionConfig",
		Mode: controller.OfferingModePassthrough,
		SubResources: []controller.SubResourceConfig{
			{DeviceClass: "sriovnetwork.k8snetworkplumbingwg.io", Count: 1, Passthrough: true, Opaque: &controller.OpaqueConfig{
				Driver:     "sriovnetwork.k8snetworkplumbingwg.io",
				Parameters: json.RawMessage(`{"apiVersion":"sriovnetwork.k8snetworkplumbingwg.io/v1alpha1","kind":"VfConfig","driver":"vfio-pci"}`),
			}},
			{DeviceClass: "rdma.mellanox.com", Count: 1, Passthrough: true, Opaque: &controller.OpaqueConfig{
				Driver:     "rdma.mellanox.com",
				Parameters: json.RawMessage(`{"apiVersion":"rdma.example/v1","kind":"VfioConfig"}`),
			}},
		},
	}
	client := fake.NewSimpleClientset(makePartitionDeviceClass("vfio-order", partConfig))
	expander := NewClaimExpander(client)
	claim := &resourcev1.ResourceClaim{Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{
		Requests: []resourcev1.DeviceRequest{{Name: "partition", Exactly: &resourcev1.ExactDeviceRequest{
			DeviceClassName: "vfio-order", Count: 1,
		}}},
	}}}

	for i := 0; i < 20; i++ {
		patches, err := expander.expandClaim(context.Background(), claim)
		require.NoError(t, err)
		var configPatch *jsonPatch
		for j := range patches {
			if patches[j].Path == "/spec/devices/config" {
				configPatch = &patches[j]
				break
			}
		}
		require.NotNil(t, configPatch)
		configJSON, err := json.Marshal(configPatch.Value)
		require.NoError(t, err)
		var configs []resourcev1.DeviceClaimConfiguration
		require.NoError(t, json.Unmarshal(configJSON, &configs))
		require.Len(t, configs, 2)
		require.NotNil(t, configs[0].Opaque)
		require.NotNil(t, configs[1].Opaque)
		assert.Equal(t, "rdma.mellanox.com", configs[0].Opaque.Driver)
		assert.Equal(t, "sriovnetwork.k8snetworkplumbingwg.io", configs[1].Opaque.Driver)
	}
}

func TestPassthroughCountSplitsRequests(t *testing.T) {
	partConfig := controller.PartitionConfig{
		Kind: "PartitionConfig",
		Mode: controller.OfferingModePassthrough,
		SubResources: []controller.SubResourceConfig{
			{DeviceClass: "gpu.amd.com", Count: 2, Passthrough: true},
		},
	}
	client := fake.NewSimpleClientset(makePartitionDeviceClass("vfio-gpu", partConfig))
	expander := NewClaimExpander(client)
	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "vfio-count-claim", Namespace: "default"},
		Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
			Name:    "partition",
			Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: "vfio-gpu", Count: 1},
		}}}},
	}

	patches, err := expander.expandClaim(context.Background(), claim)
	require.NoError(t, err)
	var requestsPatch *jsonPatch
	for i := range patches {
		if patches[i].Path == "/spec/devices/requests" {
			requestsPatch = &patches[i]
		}
	}
	require.NotNil(t, requestsPatch)
	data, err := json.Marshal(requestsPatch.Value)
	require.NoError(t, err)
	var requests []resourcev1.DeviceRequest
	require.NoError(t, json.Unmarshal(data, &requests))
	require.Len(t, requests, 2)
	assert.Equal(t, int64(1), requests[0].Exactly.Count)
	assert.Equal(t, int64(1), requests[1].Exactly.Count)
	assert.NotEqual(t, requests[0].Name, requests[1].Name)
}

func TestMixedClaimOnlyExpandsPartition(t *testing.T) {
	partConfig := controller.PartitionConfig{
		Kind: "PartitionConfig",
		SubResources: []controller.SubResourceConfig{
			{DeviceClass: "gpu.nvidia.com", Count: 2},
		},
		Alignments: []controller.AlignmentConfig{},
	}

	partClass := makePartitionDeviceClass("partition-class", partConfig)
	regularClass := makeRegularDeviceClass("regular-class")
	client := fake.NewSimpleClientset(partClass, regularClass)
	expander := NewClaimExpander(client)

	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mixed-claim",
			Namespace: "default",
		},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{
					{
						Name: "regular",
						Exactly: &resourcev1.ExactDeviceRequest{
							DeviceClassName: "regular-class",
							Count:           1,
						},
					},
					{
						Name: "partition",
						Exactly: &resourcev1.ExactDeviceRequest{
							DeviceClassName: "partition-class",
							Count:           1,
						},
					},
				},
			},
		},
	}

	review := makeAdmissionReview(claim, admissionv1.Create)
	resp := sendAdmissionReview(t, expander.Handler(), review)

	assert.True(t, resp.Response.Allowed)
	require.NotNil(t, resp.Response.PatchType)
	require.NotEmpty(t, resp.Response.Patch)

	var patches []jsonPatch
	err := json.Unmarshal(resp.Response.Patch, &patches)
	require.NoError(t, err)

	// Find the requests patch
	var requestsPatch *jsonPatch
	for i := range patches {
		if patches[i].Path == "/spec/devices/requests" {
			requestsPatch = &patches[i]
		}
	}
	require.NotNil(t, requestsPatch)

	reqBytes, err := json.Marshal(requestsPatch.Value)
	require.NoError(t, err)

	var expandedRequests []resourcev1.DeviceRequest
	err = json.Unmarshal(reqBytes, &expandedRequests)
	require.NoError(t, err)

	assert.Len(t, expandedRequests, 2)

	foundRegular := false
	foundGPU := false
	for _, r := range expandedRequests {
		if r.Name == "regular" {
			foundRegular = true
			require.NotNil(t, r.Exactly)
			assert.Equal(t, "regular-class", r.Exactly.DeviceClassName)
		}
		if strings.Contains(r.Name, "gpu-nvidia-com") {
			foundGPU = true
			require.NotNil(t, r.Exactly)
			assert.Equal(t, "gpu.nvidia.com", r.Exactly.DeviceClassName)
			assert.Equal(t, int64(2), r.Exactly.Count)
		}
	}
	assert.True(t, foundRegular, "regular request should be preserved")
	assert.True(t, foundGPU, "GPU request should be expanded")
}

func TestDeviceClassNotFoundReturnsAllow(t *testing.T) {
	client := fake.NewSimpleClientset() // No DeviceClasses
	expander := NewClaimExpander(client)

	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "missing-class-claim",
			Namespace: "default",
		},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{
					{
						Name: "req",
						Exactly: &resourcev1.ExactDeviceRequest{
							DeviceClassName: "nonexistent-class",
							Count:           1,
						},
					},
				},
			},
		},
	}

	review := makeAdmissionReview(claim, admissionv1.Create)
	resp := sendAdmissionReview(t, expander.Handler(), review)

	assert.True(t, resp.Response.Allowed)
	assert.Nil(t, resp.Response.PatchType, "missing DeviceClass should not cause mutation")
	assert.Empty(t, resp.Response.Patch)
}

func TestManagedDeviceClassWithoutPartitionConfigIsRejected(t *testing.T) {
	dc := &resourcev1.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "managed-without-config",
			Labels: map[string]string{controller.CoordinatorDriverName + "/managed": "true"},
		},
	}
	client := fake.NewSimpleClientset(dc)
	expander := NewClaimExpander(client)
	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "managed-claim", Namespace: "default"},
		Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
			Name:    "partition",
			Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: dc.Name, Count: 1},
		}}}},
	}

	resp := sendAdmissionReview(t, expander.Handler(), makeAdmissionReview(claim, admissionv1.Create))
	assert.False(t, resp.Response.Allowed)
}

func TestCustomDriverNameExpandsManagedDeviceClass(t *testing.T) {
	const customDriver = "example.coordinator"
	partConfig := controller.PartitionConfig{
		Kind: "PartitionConfig",
		SubResources: []controller.SubResourceConfig{{
			DeviceClass: "gpu.example.com",
			Count:       1,
		}},
	}
	dc := makePartitionDeviceClass("custom-offering", partConfig)
	dc.Labels = map[string]string{controller.CoordinatorDriverName + "/managed": "true"}
	dc.Spec.Config[0].Opaque.Driver = customDriver

	client := fake.NewSimpleClientset(dc)
	expander := NewClaimExpanderWithDriver(client, customDriver)
	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "custom-claim", Namespace: "default"},
		Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
			Name:    "partition",
			Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: dc.Name, Count: 1},
		}}}},
	}

	resp := sendAdmissionReview(t, expander.Handler(), makeAdmissionReview(claim, admissionv1.Create))
	assert.True(t, resp.Response.Allowed)
	assert.NotNil(t, resp.Response.PatchType)

	var patches []jsonPatch
	require.NoError(t, json.Unmarshal(resp.Response.Patch, &patches))
	var requestsPatch *jsonPatch
	for i := range patches {
		if patches[i].Path == "/spec/devices/requests" {
			requestsPatch = &patches[i]
		}
	}
	require.NotNil(t, requestsPatch)
	requestJSON, err := json.Marshal(requestsPatch.Value)
	require.NoError(t, err)
	var requests []resourcev1.DeviceRequest
	require.NoError(t, json.Unmarshal(requestJSON, &requests))
	require.Len(t, requests, 1)
	assert.Equal(t, "gpu.example.com", requests[0].Exactly.DeviceClassName)
}

func TestDeviceClassLookupErrorIsRejected(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "deviceclasses", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unavailable")
	})
	expander := NewClaimExpander(client)
	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "lookup-error-claim", Namespace: "default"},
		Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
			Name:    "partition",
			Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: "coordinator-offering", Count: 1},
		}}}},
	}

	resp := sendAdmissionReview(t, expander.Handler(), makeAdmissionReview(claim, admissionv1.Create))
	assert.False(t, resp.Response.Allowed)
}

func TestInvalidPartitionConfigReturnsAllow(t *testing.T) {
	// Create a DeviceClass with our driver but invalid/non-PartitionConfig opaque data
	invalidJSON := []byte(`{"kind":"SomethingElse","foo":"bar"}`)
	dc := &resourcev1.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "invalid-config-class",
		},
		Spec: resourcev1.DeviceClassSpec{
			Config: []resourcev1.DeviceClassConfiguration{
				{
					DeviceConfiguration: resourcev1.DeviceConfiguration{
						Opaque: &resourcev1.OpaqueDeviceConfiguration{
							Driver:     driverName,
							Parameters: runtime.RawExtension{Raw: invalidJSON},
						},
					},
				},
			},
		},
	}

	client := fake.NewSimpleClientset(dc)
	expander := NewClaimExpander(client)

	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "invalid-config-claim",
			Namespace: "default",
		},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{
					{
						Name: "req",
						Exactly: &resourcev1.ExactDeviceRequest{
							DeviceClassName: "invalid-config-class",
							Count:           1,
						},
					},
				},
			},
		},
	}

	review := makeAdmissionReview(claim, admissionv1.Create)
	resp := sendAdmissionReview(t, expander.Handler(), review)

	assert.True(t, resp.Response.Allowed)
	assert.Nil(t, resp.Response.PatchType, "invalid PartitionConfig should not cause mutation")
	assert.Empty(t, resp.Response.Patch)
}

func TestIntentMetadataDoesNotChangeClaimExpansion(t *testing.T) {
	partConfig := controller.PartitionConfig{
		Kind: "PartitionConfig",
		SubResources: []controller.SubResourceConfig{
			{DeviceClass: "gpu.nvidia.com", Count: 2},
			{DeviceClass: "rdma.mellanox.com", Count: 2},
		},
		Alignments: []controller.AlignmentConfig{{
			Attribute: "resource.kubernetes.io/pcieRoot",
			Requests:  []string{"gpu.nvidia.com", "rdma.mellanox.com"},
		}},
	}
	bare := makePartitionDeviceClass("bare-offering", partConfig)
	withIntent := makePartitionDeviceClass("intent-offering", partConfig)
	withIntent.Labels = map[string]string{
		controller.CoordinatorDriverName + "/intent-level": "workload",
		controller.CoordinatorDriverName + "/visibility":   "public",
	}
	withIntent.Annotations = map[string]string{
		controller.CoordinatorDriverName + "/display-name":   "GPU inference",
		controller.CoordinatorDriverName + "/categories":     "ai,inference",
		controller.CoordinatorDriverName + "/workload-types": "training,serving",
	}
	expander := NewClaimExpander(fake.NewSimpleClientset(bare, withIntent))
	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: "default"},
		Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
			Name: "offering",
			Exactly: &resourcev1.ExactDeviceRequest{
				DeviceClassName: "bare-offering",
				Count:           1,
			},
		}}}},
	}
	barePatches, err := expander.expandClaim(context.Background(), claim)
	require.NoError(t, err)

	intentClaim := claim.DeepCopy()
	intentClaim.Spec.Devices.Requests[0].Exactly.DeviceClassName = "intent-offering"
	intentPatches, err := expander.expandClaim(context.Background(), intentClaim)
	require.NoError(t, err)
	assert.Equal(t, barePatches, intentPatches, "intent metadata must not change request or constraint patches")

	bareConfig, err := expander.getPartitionConfig(context.Background(), "bare-offering")
	require.NoError(t, err)
	intentConfig, err := expander.getPartitionConfig(context.Background(), "intent-offering")
	require.NoError(t, err)
	require.NotNil(t, bareConfig)
	require.NotNil(t, intentConfig)
	assert.Equal(t, partConfig, *bareConfig)
	assert.Equal(t, *bareConfig, *intentConfig)
}

func TestExpandClaimDirectly(t *testing.T) {
	partConfig := controller.PartitionConfig{
		Kind: "PartitionConfig",
		SubResources: []controller.SubResourceConfig{
			{DeviceClass: "gpu.nvidia.com", Count: 4},
			{DeviceClass: "rdma.mellanox.com", Count: 4},
		},
		Alignments: []controller.AlignmentConfig{
			{
				Attribute: controller.AttrNUMANode,
				Requests:  []string{"gpu.nvidia.com", "rdma.mellanox.com"},
			},
			{
				Attribute: "resource.kubernetes.io/pcieRoot",
				Requests:  []string{"gpu.nvidia.com", "rdma.mellanox.com"},
			},
		},
	}

	partClass := makePartitionDeviceClass("test-partition", partConfig)
	client := fake.NewSimpleClientset(partClass)
	expander := NewClaimExpander(client)

	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "default",
		},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{
					{
						Name: "my-partition",
						Exactly: &resourcev1.ExactDeviceRequest{
							DeviceClassName: "test-partition",
							Count:           1,
						},
					},
				},
			},
		},
	}

	patches, err := expander.expandClaim(context.Background(), claim)
	require.NoError(t, err)
	require.NotEmpty(t, patches)

	// Verify request names use the original request name as prefix
	var requestsPatch *jsonPatch
	for i := range patches {
		if patches[i].Path == "/spec/devices/requests" {
			requestsPatch = &patches[i]
		}
	}
	require.NotNil(t, requestsPatch)

	reqBytes, _ := json.Marshal(requestsPatch.Value)
	var reqs []resourcev1.DeviceRequest
	require.NoError(t, json.Unmarshal(reqBytes, &reqs))

	assert.Len(t, reqs, 2)
	expectedNames := map[string]string{
		"my-partition-gpu-nvidia-com":    "gpu.nvidia.com",
		"my-partition-rdma-mellanox-com": "rdma.mellanox.com",
	}
	for _, r := range reqs {
		expectedClass, ok := expectedNames[r.Name]
		assert.True(t, ok, "unexpected request name: %s", r.Name)
		require.NotNil(t, r.Exactly)
		assert.Equal(t, expectedClass, r.Exactly.DeviceClassName)
		assert.Equal(t, int64(4), r.Exactly.Count)
	}
}

func TestPreferredConstraintSkippedWhenUnsatisfiable(t *testing.T) {
	partConfig := controller.PartitionConfig{
		Kind: "PartitionConfig",
		SubResources: []controller.SubResourceConfig{
			{DeviceClass: "gpu.nvidia.com", Count: 2},
			{DeviceClass: "rdma.mellanox.com", Count: 1},
		},
		Alignments: []controller.AlignmentConfig{
			{
				Attribute:   controller.AttrNUMANode,
				Requests:    []string{"gpu.nvidia.com", "rdma.mellanox.com"},
				Enforcement: controller.EnforcementPreferred,
			},
			{
				Attribute:   "resource.kubernetes.io/pcieRoot",
				Requests:    []string{"gpu.nvidia.com", "rdma.mellanox.com"},
				Enforcement: controller.EnforcementRequired,
			},
		},
	}

	partClass := makePartitionDeviceClass("test-partition", partConfig)
	client := fake.NewSimpleClientset(partClass)

	// Create a topology model where NUMA alignment is NOT satisfiable:
	// GPUs on NUMA 0, NIC on NUMA 1
	model := controller.NewTopologyModel()
	model.UpdateFromResourceSlice(makeTopologyResourceSlice("gpu-slice", "gpu.nvidia.com", "gpu-pool", 0, "pcie-0", 2))
	model.UpdateFromResourceSlice(makeTopologyResourceSlice("nic-slice", "rdma.mellanox.com", "nic-pool", 1, "pcie-1", 1))

	expander := NewClaimExpander(client, model)

	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim", Namespace: "default"},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{
					{
						Name: "partition",
						Exactly: &resourcev1.ExactDeviceRequest{
							DeviceClassName: "test-partition",
							Count:           1,
						},
					},
				},
			},
		},
	}

	patches, err := expander.expandClaim(context.Background(), claim)
	require.NoError(t, err)
	require.NotEmpty(t, patches)

	// Find constraints patch
	var constraintsPatch *jsonPatch
	for i := range patches {
		if patches[i].Path == "/spec/devices/constraints" {
			constraintsPatch = &patches[i]
		}
	}

	require.NotNil(t, constraintsPatch)
	conBytes, _ := json.Marshal(constraintsPatch.Value)
	var constraints []resourcev1.DeviceConstraint
	require.NoError(t, json.Unmarshal(conBytes, &constraints))

	// Only the required PCIe constraint should remain; NUMA preferred should be skipped
	assert.Len(t, constraints, 1, "preferred unsatisfiable constraint should be skipped")
	assert.Equal(t, resourcev1.FullyQualifiedName("resource.kubernetes.io/pcieRoot"), *constraints[0].MatchAttribute)
}

func TestPreferredConstraintEmittedWhenSatisfiable(t *testing.T) {
	partConfig := controller.PartitionConfig{
		Kind: "PartitionConfig",
		SubResources: []controller.SubResourceConfig{
			{DeviceClass: "gpu.nvidia.com", Count: 2},
			{DeviceClass: "rdma.mellanox.com", Count: 1},
		},
		Alignments: []controller.AlignmentConfig{
			{
				Attribute:   controller.AttrNUMANode,
				Requests:    []string{"gpu.nvidia.com", "rdma.mellanox.com"},
				Enforcement: controller.EnforcementPreferred,
			},
		},
	}

	partClass := makePartitionDeviceClass("test-partition", partConfig)
	client := fake.NewSimpleClientset(partClass)

	// Create a topology model where NUMA alignment IS satisfiable:
	// GPUs and NIC on same NUMA 0
	model := controller.NewTopologyModel()
	model.UpdateFromResourceSlice(makeTopologyResourceSlice("gpu-slice", "gpu.nvidia.com", "gpu-pool", 0, "pcie-0", 2))
	model.UpdateFromResourceSlice(makeTopologyResourceSlice("nic-slice", "rdma.mellanox.com", "nic-pool", 0, "pcie-0", 1))

	expander := NewClaimExpander(client, model)

	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim", Namespace: "default"},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{
					{
						Name: "partition",
						Exactly: &resourcev1.ExactDeviceRequest{
							DeviceClassName: "test-partition",
							Count:           1,
						},
					},
				},
			},
		},
	}

	patches, err := expander.expandClaim(context.Background(), claim)
	require.NoError(t, err)
	require.NotEmpty(t, patches)

	var constraintsPatch *jsonPatch
	for i := range patches {
		if patches[i].Path == "/spec/devices/constraints" {
			constraintsPatch = &patches[i]
		}
	}

	require.NotNil(t, constraintsPatch)
	conBytes, _ := json.Marshal(constraintsPatch.Value)
	var constraints []resourcev1.DeviceConstraint
	require.NoError(t, json.Unmarshal(conBytes, &constraints))

	// NUMA constraint should be emitted because it's satisfiable
	assert.Len(t, constraints, 1, "preferred satisfiable constraint should be emitted")
	assert.Equal(t, resourcev1.FullyQualifiedName(controller.AttrNUMANode), *constraints[0].MatchAttribute)
}

func TestPreferredConstraintUsesSourceDriversForExplicitRequestClasses(t *testing.T) {
	partConfig := controller.PartitionConfig{
		Kind: "PartitionConfig",
		SubResources: []controller.SubResourceConfig{
			{SourceDriver: "gpu.nvidia.com", DeviceClass: "vfio.gpu.nvidia.com", Count: 1},
			{SourceDriver: "rdma.mellanox.com", DeviceClass: "vfio.rdma.mellanox.com", Count: 1},
		},
		Alignments: []controller.AlignmentConfig{{
			Attribute:   controller.AttrNUMANode,
			Requests:    []string{"vfio.gpu.nvidia.com", "vfio.rdma.mellanox.com"},
			Enforcement: controller.EnforcementPreferred,
		}},
	}
	partClass := makePartitionDeviceClass("explicit-request-classes", partConfig)
	model := controller.NewTopologyModel()
	model.UpdateFromResourceSlice(makeTopologyResourceSlice("gpu-slice", "gpu.nvidia.com", "gpu-pool", 0, "pcie-0", 1))
	model.UpdateFromResourceSlice(makeTopologyResourceSlice("nic-slice", "rdma.mellanox.com", "nic-pool", 0, "pcie-0", 1))
	expander := NewClaimExpander(fake.NewSimpleClientset(partClass), model)
	claim := &resourcev1.ResourceClaim{Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
		Name:    "partition",
		Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: partClass.Name, Count: 1},
	}}}}}

	patches, err := expander.expandClaim(context.Background(), claim)
	require.NoError(t, err)
	var constraintsPatch *jsonPatch
	for i := range patches {
		if patches[i].Path == "/spec/devices/constraints" {
			constraintsPatch = &patches[i]
		}
	}
	require.NotNil(t, constraintsPatch)
	var constraints []resourcev1.DeviceConstraint
	constraintBytes, err := json.Marshal(constraintsPatch.Value)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(constraintBytes, &constraints))
	require.Len(t, constraints, 1)
	assert.Equal(t, resourcev1.FullyQualifiedName(controller.AttrNUMANode), *constraints[0].MatchAttribute)
}

func TestPreferredConstraintSkippedWithoutModel(t *testing.T) {
	partConfig := controller.PartitionConfig{
		Kind: "PartitionConfig",
		SubResources: []controller.SubResourceConfig{
			{DeviceClass: "gpu.nvidia.com", Count: 2},
		},
		Alignments: []controller.AlignmentConfig{
			{
				Attribute:   controller.AttrNUMANode,
				Requests:    []string{"gpu.nvidia.com"},
				Enforcement: controller.EnforcementPreferred,
			},
		},
	}

	partClass := makePartitionDeviceClass("test-partition", partConfig)
	client := fake.NewSimpleClientset(partClass)

	// No model provided — preferred constraints should be skipped
	expander := NewClaimExpander(client)

	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim", Namespace: "default"},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{
					{
						Name: "partition",
						Exactly: &resourcev1.ExactDeviceRequest{
							DeviceClassName: "test-partition",
							Count:           1,
						},
					},
				},
			},
		},
	}

	patches, err := expander.expandClaim(context.Background(), claim)
	require.NoError(t, err)

	// With only preferred constraints and no model, there should be no constraints to add.
	// The requests patch should still exist but no constraints patch.
	var constraintsPatch *jsonPatch
	for i := range patches {
		if patches[i].Path == "/spec/devices/constraints" {
			constraintsPatch = &patches[i]
		}
	}

	// No constraints should be generated
	assert.Nil(t, constraintsPatch, "preferred constraints without model should produce no constraints patch")
}

// makeTopologyResourceSlice creates a ResourceSlice with devices on a specific NUMA node and PCIe root.
func makeTopologyResourceSlice(name, driver, poolName string, numaNode int64, pcieRoot string, count int) *resourcev1.ResourceSlice {
	nodeName := "node-1"
	var devices []resourcev1.Device
	for i := 0; i < count; i++ {
		devices = append(devices, resourcev1.Device{
			Name: fmt.Sprintf("dev-%d", i),
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				resourcev1.QualifiedName(controller.AttrNUMANode):           {IntValue: &numaNode},
				resourcev1.QualifiedName("resource.kubernetes.io/pcieRoot"): {StringValue: &pcieRoot},
			},
		})
	}

	return &resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: resourcev1.ResourceSliceSpec{
			Driver:   driver,
			NodeName: &nodeName,
			Pool: resourcev1.ResourcePool{
				Name:               poolName,
				Generation:         1,
				ResourceSliceCount: 1,
			},
			Devices: devices,
		},
	}
}

func TestExpandClaimCountGreaterThanOne(t *testing.T) {
	partConfig := controller.PartitionConfig{
		Kind: "PartitionConfig",
		SubResources: []controller.SubResourceConfig{
			{DeviceClass: "gpu.amd.com", Count: 1},
			{DeviceClass: "dra.cpu", Count: 1},
		},
	}

	partClass := makePartitionDeviceClass("eighth", partConfig)
	client := fake.NewSimpleClientset(partClass)
	expander := NewClaimExpander(client)

	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "default",
		},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{
					{
						Name: "partitions",
						Exactly: &resourcev1.ExactDeviceRequest{
							DeviceClassName: "eighth",
							Count:           3,
						},
					},
				},
			},
		},
	}

	patches, err := expander.expandClaim(context.Background(), claim)
	require.NoError(t, err)
	require.NotEmpty(t, patches)

	var requestsPatch *jsonPatch
	for i := range patches {
		if patches[i].Path == "/spec/devices/requests" {
			requestsPatch = &patches[i]
		}
	}
	require.NotNil(t, requestsPatch)

	reqBytes, _ := json.Marshal(requestsPatch.Value)
	var reqs []resourcev1.DeviceRequest
	require.NoError(t, json.Unmarshal(reqBytes, &reqs))

	// 3 instances × 2 sub-resources = 6 requests
	assert.Len(t, reqs, 6, "count=3 with 2 sub-resources should produce 6 requests")

	// Verify indexed naming
	expectedNames := []string{
		"partitions-0-gpu-amd-com", "partitions-0-dra-cpu",
		"partitions-1-gpu-amd-com", "partitions-1-dra-cpu",
		"partitions-2-gpu-amd-com", "partitions-2-dra-cpu",
	}
	actualNames := make([]string, len(reqs))
	for i, r := range reqs {
		actualNames[i] = r.Name
	}
	assert.ElementsMatch(t, expectedNames, actualNames)
}

func TestExpandClaimCountOneUnchanged(t *testing.T) {
	partConfig := controller.PartitionConfig{
		Kind: "PartitionConfig",
		SubResources: []controller.SubResourceConfig{
			{DeviceClass: "gpu.amd.com", Count: 1},
		},
	}

	partClass := makePartitionDeviceClass("eighth", partConfig)
	client := fake.NewSimpleClientset(partClass)
	expander := NewClaimExpander(client)

	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "default",
		},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{
					{
						Name: "partition",
						Exactly: &resourcev1.ExactDeviceRequest{
							DeviceClassName: "eighth",
							Count:           1,
						},
					},
				},
			},
		},
	}

	patches, err := expander.expandClaim(context.Background(), claim)
	require.NoError(t, err)
	require.NotEmpty(t, patches)

	var requestsPatch *jsonPatch
	for i := range patches {
		if patches[i].Path == "/spec/devices/requests" {
			requestsPatch = &patches[i]
		}
	}
	require.NotNil(t, requestsPatch)

	reqBytes, _ := json.Marshal(requestsPatch.Value)
	var reqs []resourcev1.DeviceRequest
	require.NoError(t, json.Unmarshal(reqBytes, &reqs))

	// count=1 should NOT add index to name
	assert.Len(t, reqs, 1)
	assert.Equal(t, "partition-gpu-amd-com", reqs[0].Name)
}

func TestSanitizeDeviceClassName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"gpu.nvidia.com", "gpu-nvidia-com"},
		{"rdma.mellanox.com", "rdma-mellanox-com"},
		{"simple", "simple"},
		{"UPPER.Case", "upper-case"},
		{"a..b", "a-b"},
		{"", "sub"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeDeviceClassName(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSanitizeForRequestName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"gpu.nvidia.com", "gpu-nvidia-com"},
		{"sriovnetwork.k8snetworkplumbingwg.io", "sriovnetwork-k8snetworkplumbingwg-io"},
		{"dra.cpu", "dra-cpu"},
		{"simple", "simple"},
		{"UPPER/Case", "upper-case"},
		{"a..b", "a-b"},
		{"", "sub"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeForRequestName(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestHandleVMIAdmission_RewritesDistinctHostDeviceRequests(t *testing.T) {
	claimName := "expanded-claim"
	mapping, err := json.Marshal(map[string][]string{
		"partition": {
			"partition-rdma-mellanox-com-0",
			"partition-rdma-mellanox-com-1",
		},
	})
	require.NoError(t, err)
	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: "default",
			Annotations: map[string]string{
				driverName + "/expanded-requests": string(mapping),
			},
		},
	}
	client := fake.NewSimpleClientset(claim)
	expander := NewClaimExpander(client)
	vmi := map[string]interface{}{
		"metadata": map[string]interface{}{"name": "mapped-vmi"},
		"spec": map[string]interface{}{
			"resourceClaims": []interface{}{map[string]interface{}{
				"name": "partition-claim", "resourceClaimName": claimName,
			}},
			"domain": map[string]interface{}{
				"devices": map[string]interface{}{
					"hostDevices": []interface{}{
						map[string]interface{}{"name": "nic0", "claimName": "partition-claim", "requestName": "partition"},
						map[string]interface{}{"name": "nic1", "claimName": "partition-claim", "requestName": "partition"},
					},
				},
			},
		},
	}
	vmiJSON, err := json.Marshal(vmi)
	require.NoError(t, err)
	resp := expander.handleVMIAdmission(context.Background(), &admissionv1.AdmissionRequest{
		UID:       "mapped-vmi-uid",
		Namespace: "default",
		Resource:  metav1.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances"},
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: vmiJSON},
	})
	require.True(t, resp.Allowed)
	var patches []jsonPatch
	require.NoError(t, json.Unmarshal(resp.Patch, &patches))
	var requestNames []string
	for _, patch := range patches {
		if strings.Contains(patch.Path, "hostDevices/") && strings.HasSuffix(patch.Path, "/requestName") {
			requestNames = append(requestNames, patch.Value.(string))
		}
	}
	require.Len(t, requestNames, 2)
	assert.ElementsMatch(t, []string{
		"partition-rdma-mellanox-com-0",
		"partition-rdma-mellanox-com-1",
	}, requestNames)
}

func TestHandlePodAdmission_NoClaimsPassesThrough(t *testing.T) {
	client := fake.NewSimpleClientset()
	expander := NewClaimExpander(client)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main"}},
		},
	}
	podJSON, _ := json.Marshal(pod)

	req := &admissionv1.AdmissionRequest{
		UID: "test-uid",
		Resource: metav1.GroupVersionResource{
			Group: "", Version: "v1", Resource: "pods",
		},
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: podJSON},
		Namespace: "default",
	}

	resp := expander.handlePodAdmission(context.Background(), req)
	assert.True(t, resp.Allowed)
	assert.Nil(t, resp.PatchType, "pod with no claims should not be mutated")
}

func TestHandlePodAdmission_RewritesClaimReferences(t *testing.T) {
	// Create a claim with expansion annotation
	claimName := "test-claim"
	expansionMapping := map[string][]string{
		"partition": {"partition-gpu-amd-com", "partition-sriovnetwork-k8snetworkplumbingwg-io"},
	}
	mappingJSON, _ := json.Marshal(expansionMapping)

	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      claimName,
			Namespace: "default",
			Annotations: map[string]string{
				driverName + "/expanded-requests": string(mappingJSON),
			},
		},
	}

	client := fake.NewSimpleClientset(claim)
	expander := NewClaimExpander(client)

	rcName := claimName
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			ResourceClaims: []corev1.PodResourceClaim{
				{Name: "my-partition", ResourceClaimName: &rcName},
			},
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Claims: []corev1.ResourceClaim{
							{Name: "my-partition", Request: "partition"},
						},
					},
				},
			},
		},
	}
	podJSON, _ := json.Marshal(pod)

	req := &admissionv1.AdmissionRequest{
		UID: "test-uid",
		Resource: metav1.GroupVersionResource{
			Group: "", Version: "v1", Resource: "pods",
		},
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: podJSON},
		Namespace: "default",
	}

	resp := expander.handlePodAdmission(context.Background(), req)
	assert.True(t, resp.Allowed)

	if resp.PatchType != nil {
		assert.Equal(t, admissionv1.PatchTypeJSONPatch, *resp.PatchType)
		assert.NotEmpty(t, resp.Patch)

		var patches []jsonPatch
		err := json.Unmarshal(resp.Patch, &patches)
		require.NoError(t, err)
		assert.NotEmpty(t, patches, "should have patches to rewrite claim references")
	}
}

func TestHandlePodAdmission_RewritesTemplateClaimReferences(t *testing.T) {
	partConfig := controller.PartitionConfig{
		Kind:         "PartitionConfig",
		Mode:         controller.OfferingModeContainer,
		SubResources: []controller.SubResourceConfig{{DeviceClass: "gpu.amd.com", Count: 1}},
	}
	partClass := makePartitionDeviceClass("template-partition", partConfig)
	template := &resourcev1.ResourceClaimTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "template-partition-tpl", Namespace: "default"},
		Spec: resourcev1.ResourceClaimTemplateSpec{Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
				Name:    "partition",
				Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: "template-partition", Count: 1},
			}}},
		}},
	}
	client := fake.NewSimpleClientset(partClass, template)
	expander := NewClaimExpander(client)
	templateName := "template-partition-tpl"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "template-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			ResourceClaims: []corev1.PodResourceClaim{{
				Name:                      "partition-claim",
				ResourceClaimTemplateName: &templateName,
			}},
			Containers: []corev1.Container{{
				Name:      "main",
				Resources: corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "partition-claim", Request: "partition"}}},
			}},
		},
	}
	podJSON, err := json.Marshal(pod)
	require.NoError(t, err)
	resp := expander.handlePodAdmission(context.Background(), &admissionv1.AdmissionRequest{
		UID:       "template-pod-uid",
		Namespace: "default",
		Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: podJSON},
	})
	require.True(t, resp.Allowed)
	require.NotNil(t, resp.PatchType)
	var patches []jsonPatch
	require.NoError(t, json.Unmarshal(resp.Patch, &patches))
	assert.NotEmpty(t, patches)
	assert.Contains(t, string(resp.Patch), "partition-gpu-amd-com")
}

func TestHandlePodAdmission_RejectsManagedTemplateLookupError(t *testing.T) {
	templateName := "managed-template"
	template := &resourcev1.ResourceClaimTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: templateName, Namespace: "default"},
		Spec: resourcev1.ResourceClaimTemplateSpec{Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
				Name:    "partition",
				Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: "managed-offering", Count: 1},
			}}},
		}},
	}
	client := fake.NewSimpleClientset(template)
	client.PrependReactor("get", "deviceclasses", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unavailable")
	})
	expander := NewClaimExpander(client)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "managed-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			ResourceClaims: []corev1.PodResourceClaim{{Name: "partition-claim", ResourceClaimTemplateName: &templateName}},
			Containers: []corev1.Container{{
				Name:      "main",
				Resources: corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "partition-claim", Request: "partition"}}},
			}},
		},
	}
	podJSON, err := json.Marshal(pod)
	require.NoError(t, err)
	resp := expander.handlePodAdmission(context.Background(), &admissionv1.AdmissionRequest{
		UID:       "managed-pod-uid",
		Namespace: "default",
		Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: podJSON},
	})
	assert.False(t, resp.Allowed)
}

func TestHandlePodAdmission_RejectsMissingTemplate(t *testing.T) {
	templateName := "missing-template"
	client := fake.NewSimpleClientset()
	expander := NewClaimExpander(client)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "missing-template-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			ResourceClaims: []corev1.PodResourceClaim{{Name: "partition-claim", ResourceClaimTemplateName: &templateName}},
		},
	}
	podJSON, err := json.Marshal(pod)
	require.NoError(t, err)
	resp := expander.handlePodAdmission(context.Background(), &admissionv1.AdmissionRequest{
		UID:       "missing-template-pod-uid",
		Namespace: "default",
		Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: podJSON},
	})
	assert.False(t, resp.Allowed)
}

func TestHandleVMIAdmission_NoTemplates(t *testing.T) {
	client := fake.NewSimpleClientset()
	expander := NewClaimExpander(client)

	vmi := map[string]interface{}{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachineInstance",
		"metadata": map[string]interface{}{
			"name":      "test-vmi",
			"namespace": "default",
		},
		"spec": map[string]interface{}{
			"domain": map[string]interface{}{
				"devices": map[string]interface{}{},
			},
		},
	}
	vmiJSON, _ := json.Marshal(vmi)

	req := &admissionv1.AdmissionRequest{
		UID: "test-uid",
		Resource: metav1.GroupVersionResource{
			Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances",
		},
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: vmiJSON},
		Namespace: "default",
	}

	resp := expander.handleVMIAdmission(context.Background(), req)
	assert.True(t, resp.Allowed)
	assert.Nil(t, resp.PatchType, "VMI without templates should not be mutated")
}

func TestHandleVMIAdmission_RejectsMissingTemplate(t *testing.T) {
	client := fake.NewSimpleClientset()
	expander := NewClaimExpander(client)
	vmi := map[string]interface{}{
		"metadata": map[string]interface{}{"name": "missing-template-vmi"},
		"spec": map[string]interface{}{
			"resourceClaims": []interface{}{map[string]interface{}{
				"name": "partition", "resourceClaimTemplateName": "missing-template",
			}},
			"domain": map[string]interface{}{"devices": map[string]interface{}{
				"hostDevices": []interface{}{map[string]interface{}{
					"name": "nic0", "claimName": "partition", "requestName": "partition",
				}},
			}},
		},
	}
	vmiJSON, err := json.Marshal(vmi)
	require.NoError(t, err)
	resp := expander.handleVMIAdmission(context.Background(), &admissionv1.AdmissionRequest{
		UID:       "missing-template-vmi-uid",
		Namespace: "default",
		Resource:  metav1.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances"},
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: vmiJSON},
	})
	assert.False(t, resp.Allowed)
}

func TestHandleVMIAdmission_RewritesExistingTemplateHostDevice(t *testing.T) {
	partConfig := controller.PartitionConfig{
		Kind:         "PartitionConfig",
		Mode:         controller.OfferingModeContainer,
		SubResources: []controller.SubResourceConfig{{DeviceClass: "rdma.mellanox.com", Count: 1}},
	}
	partClass := makePartitionDeviceClass("template-offering", partConfig)
	templateName := "template-offering-tpl"
	template := &resourcev1.ResourceClaimTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: templateName, Namespace: "default"},
		Spec: resourcev1.ResourceClaimTemplateSpec{Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
				Name:    "partition",
				Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: "template-offering", Count: 1},
			}}},
		}},
	}
	client := fake.NewSimpleClientset(partClass, template)
	expander := NewClaimExpander(client)
	vmi := map[string]interface{}{
		"metadata": map[string]interface{}{"name": "template-vmi"},
		"spec": map[string]interface{}{
			"resourceClaims": []interface{}{map[string]interface{}{
				"name": "partition-claim", "resourceClaimTemplateName": templateName,
			}},
			"domain": map[string]interface{}{"devices": map[string]interface{}{
				"hostDevices": []interface{}{map[string]interface{}{
					"name": "nic0", "claimName": "partition-claim", "requestName": "partition",
				}},
			}},
		},
	}
	vmiJSON, err := json.Marshal(vmi)
	require.NoError(t, err)
	resp := expander.handleVMIAdmission(context.Background(), &admissionv1.AdmissionRequest{
		UID:       "template-vmi-uid",
		Namespace: "default",
		Resource:  metav1.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances"},
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: vmiJSON},
	})
	require.True(t, resp.Allowed)
	var patches []jsonPatch
	require.NoError(t, json.Unmarshal(resp.Patch, &patches))
	var rewritten bool
	for _, patch := range patches {
		if strings.HasSuffix(patch.Path, "/hostDevices/0/requestName") {
			rewritten = true
			assert.Equal(t, "partition-rdma-mellanox-com", patch.Value)
		}
	}
	assert.True(t, rewritten)
}

func TestHandleVMIAdmission_GeneratesHostDevices(t *testing.T) {
	partConfig := controller.PartitionConfig{
		Kind: "PartitionConfig",
		Mode: controller.OfferingModePassthrough,
		SubResources: []controller.SubResourceConfig{
			{DeviceClass: "gpu.amd.com", Count: 1, Passthrough: true},
			{DeviceClass: "sriovnetwork.k8snetworkplumbingwg.io", Count: 1, Passthrough: true},
			{DeviceClass: "dra.cpu", Count: 1},
		},
	}
	partClass := makePartitionDeviceClass("eighth", partConfig)

	configJSON, _ := json.Marshal(partConfig)
	claimTemplate := &resourcev1.ResourceClaimTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "vm-eighth-tpl",
			Namespace: "default",
		},
		Spec: resourcev1.ResourceClaimTemplateSpec{
			Spec: resourcev1.ResourceClaimSpec{
				Devices: resourcev1.DeviceClaim{
					Requests: []resourcev1.DeviceRequest{
						{
							Name: "partition",
							Exactly: &resourcev1.ExactDeviceRequest{
								DeviceClassName: "eighth",
								Count:           1,
							},
						},
					},
				},
			},
		},
	}
	_ = configJSON

	client := fake.NewSimpleClientset(partClass, claimTemplate)
	expander := NewClaimExpander(client)

	vmi := map[string]interface{}{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachineInstance",
		"metadata": map[string]interface{}{
			"name":      "test-vmi",
			"namespace": "default",
		},
		"spec": map[string]interface{}{
			"resourceClaims": []interface{}{
				map[string]interface{}{
					"name":                      "partition",
					"resourceClaimTemplateName": "vm-eighth-tpl",
				},
			},
			"domain": map[string]interface{}{
				"devices": map[string]interface{}{},
			},
		},
	}
	vmiJSON, _ := json.Marshal(vmi)

	req := &admissionv1.AdmissionRequest{
		UID: "test-uid",
		Resource: metav1.GroupVersionResource{
			Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances",
		},
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: vmiJSON},
		Namespace: "default",
	}

	resp := expander.handleVMIAdmission(context.Background(), req)
	assert.True(t, resp.Allowed)

	if resp.PatchType != nil {
		assert.Equal(t, admissionv1.PatchTypeJSONPatch, *resp.PatchType)

		var patches []jsonPatch
		err := json.Unmarshal(resp.Patch, &patches)
		require.NoError(t, err)

		// Should have patches adding hostDevices for GPU and NIC (not CPU)
		hasHostDevicePatch := false
		for _, p := range patches {
			if strings.Contains(p.Path, "hostDevices") {
				hasHostDevicePatch = true
			}
		}
		assert.True(t, hasHostDevicePatch, "should generate hostDevices patches for passthrough devices")
	}
}

func TestHandleVMIAdmission_UsesUniqueRequestsForPassthroughCount(t *testing.T) {
	partConfig := controller.PartitionConfig{
		Kind: "PartitionConfig",
		Mode: controller.OfferingModePassthrough,
		SubResources: []controller.SubResourceConfig{
			{DeviceClass: "gpu.amd.com", Count: 2, Passthrough: true},
		},
	}
	partClass := makePartitionDeviceClass("vfio-count", partConfig)
	claimTemplate := &resourcev1.ResourceClaimTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "vfio-count-tpl", Namespace: "default"},
		Spec: resourcev1.ResourceClaimTemplateSpec{Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
				Name:    "partition",
				Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: "vfio-count", Count: 1},
			}}},
		}},
	}
	client := fake.NewSimpleClientset(partClass, claimTemplate)
	expander := NewClaimExpander(client)
	vmi := map[string]interface{}{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachineInstance",
		"metadata":   map[string]interface{}{"name": "count-vmi", "namespace": "default"},
		"spec": map[string]interface{}{
			"resourceClaims": []interface{}{map[string]interface{}{
				"name": "partition", "resourceClaimTemplateName": "vfio-count-tpl",
			}},
			"domain": map[string]interface{}{"devices": map[string]interface{}{}},
		},
	}
	vmiJSON, err := json.Marshal(vmi)
	require.NoError(t, err)
	resp := expander.handleVMIAdmission(context.Background(), &admissionv1.AdmissionRequest{
		UID:       "count-vmi-uid",
		Namespace: "default",
		Resource:  metav1.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances"},
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: vmiJSON},
	})
	require.True(t, resp.Allowed)
	var patches []jsonPatch
	require.NoError(t, json.Unmarshal(resp.Patch, &patches))
	var requestNames []string
	for _, patch := range patches {
		if !strings.Contains(patch.Path, "hostDevices") {
			continue
		}
		if devices, ok := patch.Value.([]interface{}); ok {
			for _, device := range devices {
				if value, ok := device.(map[string]interface{}); ok {
					if name, ok := value["requestName"].(string); ok {
						requestNames = append(requestNames, name)
					}
				}
			}
			continue
		}
		value, ok := patch.Value.(map[string]interface{})
		if ok {
			if name, ok := value["requestName"].(string); ok {
				requestNames = append(requestNames, name)
			}
		}
	}
	require.Len(t, requestNames, 2)
	assert.NotEqual(t, requestNames[0], requestNames[1])
}

func TestHandleVMIAdmission_DoesNotSplitCapacityBackedPassthrough(t *testing.T) {
	partConfig := controller.PartitionConfig{
		Kind: "PartitionConfig",
		Mode: controller.OfferingModePassthrough,
		SubResources: []controller.SubResourceConfig{{
			DeviceClass: "sriovnetwork.k8snetworkplumbingwg.io",
			Count:       2,
			Capacity:    map[string]string{"ports": "2"},
			Passthrough: true,
		}},
	}
	partClass := makePartitionDeviceClass("vfio-capacity", partConfig)
	claimTemplate := &resourcev1.ResourceClaimTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "vfio-capacity-tpl", Namespace: "default"},
		Spec: resourcev1.ResourceClaimTemplateSpec{Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
				Name:    "partition",
				Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: "vfio-capacity", Count: 1},
			}}},
		}},
	}
	client := fake.NewSimpleClientset(partClass, claimTemplate)
	expander := NewClaimExpander(client)
	vmi := map[string]interface{}{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachineInstance",
		"metadata":   map[string]interface{}{"name": "capacity-vmi", "namespace": "default"},
		"spec": map[string]interface{}{
			"resourceClaims": []interface{}{map[string]interface{}{
				"name": "partition", "resourceClaimTemplateName": "vfio-capacity-tpl",
			}},
			"domain": map[string]interface{}{"devices": map[string]interface{}{}},
		},
	}
	vmiJSON, err := json.Marshal(vmi)
	require.NoError(t, err)
	resp := expander.handleVMIAdmission(context.Background(), &admissionv1.AdmissionRequest{
		UID:       "capacity-vmi-uid",
		Namespace: "default",
		Resource:  metav1.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances"},
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: vmiJSON},
	})
	require.True(t, resp.Allowed)
	var patches []jsonPatch
	require.NoError(t, json.Unmarshal(resp.Patch, &patches))
	var requestNames []string
	for _, patch := range patches {
		if !strings.Contains(patch.Path, "hostDevices") {
			continue
		}
		devices := []interface{}{patch.Value}
		if value, ok := patch.Value.([]interface{}); ok {
			devices = value
		}
		for _, value := range devices {
			if device, ok := value.(map[string]interface{}); ok {
				if name, ok := device["requestName"].(string); ok {
					requestNames = append(requestNames, name)
				}
			}
		}
	}
	require.Equal(t, []string{"partition-sriovnetwork-k8snetworkplumbingwg-io"}, requestNames)
}

func TestEscapeJSONPointer(t *testing.T) {
	assert.Equal(t, "a~1b", escapeJSONPointer("a/b"))
	assert.Equal(t, "a~0b", escapeJSONPointer("a~b"))
	assert.Equal(t, "simple", escapeJSONPointer("simple"))
	assert.Equal(t, "a~0~1b", escapeJSONPointer("a~/b"))
}
