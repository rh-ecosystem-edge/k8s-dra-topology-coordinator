//go:build e2e

package e2e

import (
	"context"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stretchr/testify/assert"
)

func TestOwnedLabels(t *testing.T) {
	labels := ownedLabels("run-123")

	assert.Equal(t, "run-123", labels[runLabelKey])
	assert.Equal(t, "true", labels[ownerLabelKey])
}

func TestNewManagedDeviceClassNamesExcludesBaseline(t *testing.T) {
	h := &harness{baselineDeviceClasses: map[string]struct{}{"existing": {}}}
	classes := &resourcev1.DeviceClassList{Items: []resourcev1.DeviceClass{
		{ObjectMeta: metav1.ObjectMeta{Name: "existing"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "created"}},
	}}

	assert.Equal(t, []string{"created"}, h.newManagedDeviceClassNames(classes))
}

func TestNewManagedDeviceClassNamesIsSorted(t *testing.T) {
	h := &harness{}
	classes := &resourcev1.DeviceClassList{Items: []resourcev1.DeviceClass{
		{ObjectMeta: metav1.ObjectMeta{Name: "z-created"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "a-created"}},
	}}

	assert.Equal(t, []string{"a-created", "z-created"}, h.newManagedDeviceClassNames(classes))
}

func TestRemoveStaleWebhookConfigurations(t *testing.T) {
	client := fake.NewSimpleClientset(
		&admissionregistrationv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{
			Name: "old-release",
			Labels: map[string]string{
				"app.kubernetes.io/name":      "nodepartition",
				"app.kubernetes.io/component": "controller",
				"app.kubernetes.io/instance":  "old-release",
			},
		}},
		&admissionregistrationv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{
			Name: "current-release-nodepartition",
			Labels: map[string]string{
				"app.kubernetes.io/name":      "nodepartition",
				"app.kubernetes.io/component": "controller",
				"app.kubernetes.io/instance":  "current-release",
			},
		}},
		&admissionregistrationv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{
			Name:   "unrelated",
			Labels: map[string]string{"app.kubernetes.io/name": "other"},
		}},
	)
	h := &harness{client: client, cfg: Config{Release: "current-release"}}

	assert.NoError(t, h.removeStaleWebhookConfigurations(context.Background()))
	_, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(context.Background(), "old-release", metav1.GetOptions{})
	assert.Error(t, err)
	_, err = client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(context.Background(), "current-release-nodepartition", metav1.GetOptions{})
	assert.NoError(t, err)
	_, err = client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(context.Background(), "unrelated", metav1.GetOptions{})
	assert.NoError(t, err)
}

func TestDeleteReleaseWebhookConfigurations(t *testing.T) {
	client := fake.NewSimpleClientset(
		&admissionregistrationv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{
			Name: "current-release-nodepartition",
			Labels: map[string]string{
				"app.kubernetes.io/name":      "nodepartition",
				"app.kubernetes.io/component": "controller",
				"app.kubernetes.io/instance":  "current-release",
			},
		}},
		&admissionregistrationv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{
			Name: "other-release-nodepartition",
			Labels: map[string]string{
				"app.kubernetes.io/name":      "nodepartition",
				"app.kubernetes.io/component": "controller",
				"app.kubernetes.io/instance":  "other-release",
			},
		}},
	)
	h := &harness{client: client, cfg: Config{Release: "current-release"}}

	assert.NoError(t, h.deleteReleaseWebhookConfigurations(context.Background()))
	_, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(context.Background(), "current-release-nodepartition", metav1.GetOptions{})
	assert.Error(t, err)
	_, err = client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(context.Background(), "other-release-nodepartition", metav1.GetOptions{})
	assert.NoError(t, err)
}
