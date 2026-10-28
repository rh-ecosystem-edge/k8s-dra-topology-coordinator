package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/rh-ecosystem-edge/k8s-dra-topology-coordinator/internal/controller"
)

func passthroughClaim(class string, count int64) *resourcev1.ResourceClaim {
	return &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: "default"},
		Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
			Name:    "partition",
			Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: class, Count: count},
		}}}},
	}
}

func passthroughClass(name string, deviceCount int) *resourcev1.DeviceClass {
	return makePartitionDeviceClass(name, controller.PartitionConfig{
		Kind: "PartitionConfig",
		Mode: controller.OfferingModePassthrough,
		SubResources: []controller.SubResourceConfig{
			{DeviceClass: "gpu.amd.com", Count: deviceCount, Passthrough: true},
		},
	})
}

func TestExpansionRejectsOversizedExpansion(t *testing.T) {
	tests := []struct {
		name        string
		deviceCount int
		count       int64
	}{
		{"huge count", 1, 2_000_000_000},
		{"count over limit", 1, 33},
		{"split class over limit", 33, 1},
		{"product over limit", 5, 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewSimpleClientset(passthroughClass("big", tt.deviceCount))
			expander := NewClaimExpander(client)

			_, err := expander.expandClaim(context.Background(), passthroughClaim("big", tt.count))
			var managedErr *managedExpansionError
			require.ErrorAs(t, err, &managedErr)

			review := makeAdmissionReview(passthroughClaim("big", tt.count), admissionv1.Create)
			resp := sendAdmissionReview(t, expander.Handler(), review)
			assert.False(t, resp.Response.Allowed)
		})
	}
}

func TestExpansionAllowsExactlyMaxRequests(t *testing.T) {
	client := fake.NewSimpleClientset(passthroughClass("max", 32))
	expander := NewClaimExpander(client)

	patches, err := expander.expandClaim(context.Background(), passthroughClaim("max", 1))
	require.NoError(t, err)
	require.NotEmpty(t, patches)
	require.Equal(t, "/spec/devices/requests", patches[0].Path)
	requests, ok := patches[0].Value.([]resourcev1.DeviceRequest)
	require.True(t, ok)
	assert.Len(t, requests, resourcev1.DeviceRequestsMaxSize)
}

func TestPodAdmissionDeniesTemplateWithHugeCount(t *testing.T) {
	templateName := "huge-template"
	template := &resourcev1.ResourceClaimTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: templateName, Namespace: "default"},
		Spec:       resourcev1.ResourceClaimTemplateSpec{Spec: passthroughClaim("big", 2_000_000_000).Spec},
	}
	client := fake.NewSimpleClientset(passthroughClass("big", 1), template)
	expander := NewClaimExpander(client)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "huge-pod", Namespace: "default"},
		Spec: corev1.PodSpec{
			ResourceClaims: []corev1.PodResourceClaim{{Name: "c", ResourceClaimTemplateName: &templateName}},
			Containers: []corev1.Container{{
				Name:      "main",
				Resources: corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "c", Request: "partition"}}},
			}},
		},
	}
	podJSON, err := json.Marshal(pod)
	require.NoError(t, err)

	resp := expander.handlePodAdmission(context.Background(), &admissionv1.AdmissionRequest{
		UID:       "huge-pod-uid",
		Namespace: "default",
		Resource:  metav1.GroupVersionResource{Version: "v1", Resource: "pods"},
		Operation: admissionv1.Create,
		Object:    runtime.RawExtension{Raw: podJSON},
	})
	assert.False(t, resp.Allowed)
}

func TestServeHTTPRejectsNonPost(t *testing.T) {
	expander := NewClaimExpander(fake.NewSimpleClientset())
	rec := httptest.NewRecorder()
	expander.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mutate", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, http.MethodPost, rec.Header().Get("Allow"))
}

func TestServeHTTPRejectsOversizedBody(t *testing.T) {
	expander := NewClaimExpander(fake.NewSimpleClientset())
	body := bytes.Repeat([]byte("a"), maxAdmissionReviewBytes+1)
	rec := httptest.NewRecorder()
	expander.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader(body)))
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

func TestPatchResponse(t *testing.T) {
	resp := patchResponse([]jsonPatch{{Op: "add", Path: "/a", Value: "b"}})
	assert.True(t, resp.Allowed)
	require.NotNil(t, resp.PatchType)
	assert.Equal(t, admissionv1.PatchTypeJSONPatch, *resp.PatchType)
	assert.JSONEq(t, `[{"op":"add","path":"/a","value":"b"}]`, string(resp.Patch))
}

func TestExpansionRejectsSplitCountOverflow(t *testing.T) {
	class := makePartitionDeviceClass("overflow", controller.PartitionConfig{
		Kind: "PartitionConfig",
		Mode: controller.OfferingModePassthrough,
		SubResources: []controller.SubResourceConfig{
			{DeviceClass: "gpu.amd.com", Count: 1, Passthrough: true},
			{DeviceClass: "nic.amd.com", Count: math.MaxInt, Passthrough: true},
		},
	})
	expander := NewClaimExpander(fake.NewSimpleClientset(class))

	_, err := expander.expandClaim(context.Background(), passthroughClaim("overflow", 1))
	var managedErr *managedExpansionError
	require.ErrorAs(t, err, &managedErr)

	_, err = expander.expansionMapping(context.Background(), passthroughClaim("overflow", 1).Spec.Devices.Requests)
	require.ErrorAs(t, err, &managedErr)
}
