package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

const claimYAML = `apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: requested
  namespace: team-a
  labels:
    purpose: preview
spec:
  devices:
    requests:
    - name: mig
      exactly:
        deviceClassName: nvidia-mig-1g-10gb
        count: 1
`

const templateYAML = `apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: template-name
  namespace: team-b
  labels:
    outer: ignored
spec:
  metadata:
    labels:
      copied: "yes"
    annotations:
      example.com/note: from-template
  spec:
    devices:
      requests:
      - name: mig
        exactly:
          deviceClassName: nvidia-mig-1g-10gb
          count: 2
`

func TestPreviewClaimParsesClaimAndTemplate(t *testing.T) {
	claim, err := previewClaim(strings.NewReader(claimYAML))
	require.NoError(t, err)
	assert.Equal(t, "requested", claim.Name)
	assert.Empty(t, claim.GenerateName)
	assert.Equal(t, "team-a", claim.Namespace)
	assert.Equal(t, "preview", claim.Labels["purpose"])

	input := "apiVersion: v1\nkind: Pod\nmetadata:\n  name: workload\n---\n" + templateYAML
	fromTemplate, err := previewClaim(strings.NewReader(input))
	require.NoError(t, err)
	assert.Empty(t, fromTemplate.Name)
	assert.Equal(t, previewGenerateName, fromTemplate.GenerateName)
	assert.Equal(t, "team-b", fromTemplate.Namespace)
	assert.Equal(t, map[string]string{"copied": "yes"}, fromTemplate.Labels)
	assert.Equal(t, map[string]string{"example.com/note": "from-template"}, fromTemplate.Annotations)
	assert.Equal(t, int64(2), fromTemplate.Spec.Devices.Requests[0].Exactly.Count)
	assert.Empty(t, fromTemplate.UID)
	assert.Empty(t, fromTemplate.ResourceVersion)
}

func TestPreviewClaimInputErrors(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
	}{
		{"no claim", "apiVersion: v1\nkind: Pod\n", "no ResourceClaim"},
		{"two claims", claimYAML + "---\n" + templateYAML, "more than one"},
		{"unsupported version", strings.Replace(claimYAML, "resource.k8s.io/v1", "resource.k8s.io/v1beta1", 1), "unsupported ResourceClaim apiVersion"},
		{"malformed", "kind: ResourceClaim\nmetadata: [\n", "decode input"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := previewClaim(strings.NewReader(tc.input))
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestRunExpandUsesDryRunAndReturnsAdmittedClaim(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "resourceclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		create := action.(ktesting.CreateAction)
		assert.Equal(t, "team-a", create.GetNamespace())
		assert.Equal(t, []string{metav1.DryRunAll}, action.(ktesting.CreateActionImpl).GetCreateOptions().DryRun)
		claim := create.GetObject().(*resourcev1.ResourceClaim)
		assert.Equal(t, "requested", claim.Name)
		assert.Equal(t, "preview", claim.Labels["purpose"])
		admitted := claim.DeepCopy()
		admitted.TypeMeta = metav1.TypeMeta{}
		admitted.Spec.Devices.Requests[0].Name = "mig-mig-nvidia-com"
		admitted.Annotations = map[string]string{"nodepartition.dra.k8s.io/expanded-requests": `{"mig":["mig-mig-nvidia-com"]}`}
		return true, admitted, nil
	})

	var output bytes.Buffer
	require.NoError(t, runExpand(context.Background(), client, &output, strings.NewReader(claimYAML), "", "default"))
	var got resourcev1.ResourceClaim
	require.NoError(t, json.Unmarshal(output.Bytes(), &got))
	assert.Equal(t, "resource.k8s.io/v1", got.APIVersion)
	assert.Equal(t, "ResourceClaim", got.Kind)
	assert.Equal(t, "mig-mig-nvidia-com", got.Spec.Devices.Requests[0].Name)
	assert.Contains(t, got.Annotations["nodepartition.dra.k8s.io/expanded-requests"], "mig-mig-nvidia-com")
	_, err := client.ResourceV1().ResourceClaims("team-a").Get(context.Background(), "requested", metav1.GetOptions{})
	require.Error(t, err)
}

func TestRunExpandTemplateNamespaceAndConflict(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "resourceclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		create := action.(ktesting.CreateAction)
		assert.Equal(t, "team-b", create.GetNamespace())
		claim := create.GetObject().(*resourcev1.ResourceClaim)
		assert.Empty(t, claim.Name)
		assert.Equal(t, previewGenerateName, claim.GenerateName)
		assert.Equal(t, "yes", claim.Labels["copied"])
		assert.Empty(t, claim.Labels["outer"])
		return true, claim, nil
	})
	var output bytes.Buffer
	require.NoError(t, runExpand(context.Background(), client, &output, strings.NewReader(templateYAML), "team-b", "context-ns"))
	var got resourcev1.ResourceClaim
	require.NoError(t, json.Unmarshal(output.Bytes(), &got))
	assert.Equal(t, "team-b", got.Namespace)

	client.ClearActions()
	output.Reset()
	err := runExpand(context.Background(), client, &output, strings.NewReader(templateYAML), "wrong-ns", "default")
	require.ErrorContains(t, err, "conflicts with --namespace")
	assert.Empty(t, client.Actions())
}

func TestRunExpandNamespaceFallbackAndSyntheticName(t *testing.T) {
	input := strings.ReplaceAll(claimYAML, "  name: requested\n  namespace: team-a\n", "")
	for _, tc := range []struct {
		flag, context, want string
	}{
		{"flag-ns", "context-ns", "flag-ns"},
		{"", "context-ns", "context-ns"},
		{"", "", "default"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			client.PrependReactor("create", "resourceclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
				create := action.(ktesting.CreateAction)
				assert.Equal(t, tc.want, create.GetNamespace())
				claim := create.GetObject().(*resourcev1.ResourceClaim)
				assert.Equal(t, previewGenerateName, claim.GenerateName)
				return true, claim, nil
			})
			var output bytes.Buffer
			require.NoError(t, runExpand(context.Background(), client, &output, strings.NewReader(input), tc.flag, tc.context))
		})
	}
}

func TestValidateExpandInputModes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		fileSet     bool
		file        string
		offeringSet bool
		offering    string
		countSet    bool
		count       int64
		wantErr     string
	}{
		{name: "missing input", count: 1, wantErr: "specify exactly one"},
		{name: "both inputs", fileSet: true, file: "-", offeringSet: true, offering: "gpu-inference", count: 1, wantErr: "mutually exclusive"},
		{name: "empty file", fileSet: true, file: "", count: 1, wantErr: "file path must not be empty"},
		{name: "empty offering", offeringSet: true, offering: " ", count: 1, wantErr: "offering name must not be empty"},
		{name: "count with file", fileSet: true, file: "claim.yaml", countSet: true, count: 1, wantErr: "--count requires --offering"},
		{name: "zero offering count", offeringSet: true, offering: "gpu-inference", count: 0, wantErr: "count must be positive"},
		{name: "negative offering count", offeringSet: true, offering: "gpu-inference", count: -1, wantErr: "count must be positive"},
		{name: "file input", fileSet: true, file: "claim.yaml", count: 1},
		{name: "stdin input", fileSet: true, file: "-", count: 1},
		{name: "offering default count", offeringSet: true, offering: "gpu-inference", count: 1},
		{name: "offering explicit count", offeringSet: true, offering: "gpu-inference", countSet: true, count: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateExpandInput(tc.fileSet, tc.file, tc.offeringSet, tc.offering, tc.countSet, tc.count)
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.wantErr)
			}
		})
	}
}

func TestOfferingPreviewClaim(t *testing.T) {
	claim, err := offeringPreviewClaim("gpu-inference", 3)
	require.NoError(t, err)
	assert.Equal(t, "resource.k8s.io/v1", claim.APIVersion)
	assert.Equal(t, "ResourceClaim", claim.Kind)
	assert.Equal(t, previewGenerateName, claim.GenerateName)
	require.Len(t, claim.Spec.Devices.Requests, 1)
	request := claim.Spec.Devices.Requests[0]
	assert.Equal(t, "offering", request.Name)
	require.NotNil(t, request.Exactly)
	assert.Equal(t, "gpu-inference", request.Exactly.DeviceClassName)
	assert.Equal(t, int64(3), request.Exactly.Count)

	_, err = offeringPreviewClaim("gpu-inference", 0)
	require.ErrorContains(t, err, "count must be positive")
}

func TestRunExpandOfferingRequiresCuratedOfferingAndDryRunsBundleCount(t *testing.T) {
	client := fake.NewSimpleClientset()
	for _, dc := range fixtureClasses() {
		dc := dc
		_, err := client.ResourceV1().DeviceClasses().Create(context.Background(), &dc, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	client.PrependReactor("create", "resourceclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		create := action.(ktesting.CreateAction)
		assert.Equal(t, []string{metav1.DryRunAll}, action.(ktesting.CreateActionImpl).GetCreateOptions().DryRun)
		claim := create.GetObject().(*resourcev1.ResourceClaim)
		require.Len(t, claim.Spec.Devices.Requests, 1)
		assert.Equal(t, int64(5), claim.Spec.Devices.Requests[0].Exactly.Count)
		assert.Equal(t, "gpu-inference", claim.Spec.Devices.Requests[0].Exactly.DeviceClassName)
		admitted := claim.DeepCopy()
		admitted.Spec.Devices.Requests[0].Name = "expanded-first-bundle"
		return true, admitted, nil
	})

	var output bytes.Buffer
	// The fixture catalog count is 4; count requests complete bundles and is not capped by it.
	require.NoError(t, runExpandOffering(context.Background(), client, &output, "gpu-inference", 5, "", "team-a"))
	var got resourcev1.ResourceClaim
	require.NoError(t, json.Unmarshal(output.Bytes(), &got))
	assert.Equal(t, "team-a", got.Namespace)
	assert.Equal(t, "expanded-first-bundle", got.Spec.Devices.Requests[0].Name)

	client.ClearActions()
	for _, name := range []string{"missing", "plain", "gpu-nic-pair", "gpu-full"} {
		output.Reset()
		err := runExpandOffering(context.Background(), client, &output, name, 1, "", "default")
		require.Error(t, err)
		assert.Empty(t, output.String())
		for _, action := range client.Actions() {
			assert.NotEqual(t, "create", action.GetVerb(), "invalid offering %q must fail before claim CREATE", name)
		}
		client.ClearActions()
	}
}

func TestClaimsExpandRegistersOfferingFlags(t *testing.T) {
	command, _, err := newRootCmd().Find([]string{"claims", "expand"})
	require.NoError(t, err)
	require.NotNil(t, command.Flags().Lookup("offering"))
	assert.Equal(t, "", command.Flags().Lookup("offering").DefValue)
	require.NotNil(t, command.Flags().Lookup("count"))
	assert.Equal(t, "1", command.Flags().Lookup("count").DefValue)
	assert.Empty(t, command.Flags().Lookup("file").Annotations[cobra.BashCompOneRequiredFlag])
	var help bytes.Buffer
	command.SetOut(&help)
	require.NoError(t, command.Help())
	assert.Contains(t, help.String(), "--offering")
	assert.Contains(t, help.String(), "--count")
}
