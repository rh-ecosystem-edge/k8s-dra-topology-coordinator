// Package webhook implements a mutating admission webhook that expands
// partition ResourceClaims into multi-request claims with topology alignment.
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	klog "k8s.io/klog/v2"

	"github.com/rh-ecosystem-edge/k8s-dra-topology-coordinator/internal/controller"
)

const (
	driverName = "nodepartition.dra.k8s.io"
)

// ClaimExpander is a mutating admission webhook that expands partition
// ResourceClaims into multi-request claims with alignment constraints.
type ClaimExpander struct {
	client     kubernetes.Interface
	decoder    runtime.Decoder
	model      *controller.TopologyModel
	driverName string
}

// jsonPatch represents a single JSON Patch operation.
type jsonPatch struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

type managedExpansionError struct {
	err error
}

func (e *managedExpansionError) Error() string { return e.err.Error() }
func (e *managedExpansionError) Unwrap() error { return e.err }

// NewClaimExpander creates a new ClaimExpander webhook handler.
// The model parameter is optional; when provided, it enables satisfiability
// checks for "preferred" enforcement constraints.
func NewClaimExpander(client kubernetes.Interface, model ...*controller.TopologyModel) *ClaimExpander {
	return NewClaimExpanderWithDriver(client, driverName, model...)
}

// NewClaimExpanderWithDriver creates a ClaimExpander for the configured DRA driver.
func NewClaimExpanderWithDriver(client kubernetes.Interface, configuredDriverName string, model ...*controller.TopologyModel) *ClaimExpander {
	if configuredDriverName == "" {
		configuredDriverName = driverName
	}
	scheme := runtime.NewScheme()
	codecs := serializer.NewCodecFactory(scheme)
	ce := &ClaimExpander{
		client:     client,
		decoder:    codecs.UniversalDeserializer(),
		driverName: configuredDriverName,
	}
	if len(model) > 0 {
		ce.model = model[0]
	}
	return ce
}

// Handler returns the HTTP handler for the webhook.
func (ce *ClaimExpander) Handler() http.Handler {
	return ce
}

// ServeHTTP handles admission review requests.
func (ce *ClaimExpander) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		klog.Errorf("Failed to read request body: %v", err)
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	var review admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &review); err != nil {
		klog.Errorf("Failed to unmarshal admission review: %v", err)
		http.Error(w, "failed to unmarshal admission review", http.StatusBadRequest)
		return
	}

	if review.Request == nil {
		klog.Error("Admission review has no request")
		http.Error(w, "admission review has no request", http.StatusBadRequest)
		return
	}

	response := ce.handleAdmission(r.Context(), review.Request)
	review.Response = response
	review.Response.UID = review.Request.UID

	respBytes, err := json.Marshal(review)
	if err != nil {
		klog.Errorf("Failed to marshal admission response: %v", err)
		http.Error(w, "failed to marshal response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(respBytes); err != nil {
		klog.Errorf("Failed to write admission response: %v", err)
	}
}

// handleAdmission processes a single admission request and returns the response.
func (ce *ClaimExpander) handleAdmission(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	// Route by resource type
	if req.Resource.Group == "" && req.Resource.Resource == "pods" {
		if req.Operation == admissionv1.Create {
			return ce.handlePodAdmission(ctx, req)
		}
		return allowResponse()
	}

	if req.Resource.Group == "kubevirt.io" && req.Resource.Resource == "virtualmachineinstances" {
		if req.Operation == admissionv1.Create {
			return ce.handleVMIAdmission(ctx, req)
		}
		return allowResponse()
	}

	// Only handle ResourceClaims
	if req.Resource.Group != "resource.k8s.io" || req.Resource.Resource != "resourceclaims" {
		return allowResponse()
	}

	// Only handle CREATE and UPDATE
	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update {
		return allowResponse()
	}

	var claim resourcev1.ResourceClaim
	if err := json.Unmarshal(req.Object.Raw, &claim); err != nil {
		klog.Errorf("Failed to unmarshal ResourceClaim: %v", err)
		return allowResponse()
	}

	patches, err := ce.expandClaim(ctx, &claim)
	if err != nil {
		klog.Errorf("Failed to expand claim %s/%s: %v", claim.Namespace, claim.Name, err)
		var managedErr *managedExpansionError
		if errors.As(err, &managedErr) {
			return denyResponse(err.Error())
		}
		return allowResponse()
	}

	if len(patches) == 0 {
		return allowResponse()
	}

	patchBytes, err := json.Marshal(patches)
	if err != nil {
		klog.Errorf("Failed to marshal patches: %v", err)
		return allowResponse()
	}

	patchType := admissionv1.PatchTypeJSONPatch
	return &admissionv1.AdmissionResponse{
		Allowed:   true,
		PatchType: &patchType,
		Patch:     patchBytes,
	}
}

// expandClaim examines a ResourceClaim and generates JSON patches to expand
// partition requests into sub-resource requests with alignment constraints.
func (ce *ClaimExpander) expandClaim(ctx context.Context, claim *resourcev1.ResourceClaim) ([]jsonPatch, error) { //nolint:unparam // error return reserved for future use
	requests := claim.Spec.Devices.Requests
	if len(requests) == 0 {
		return nil, nil
	}

	var expandedRequests []resourcev1.DeviceRequest
	var constraints []resourcev1.DeviceConstraint
	var explicitConfigs []resourcev1.DeviceClaimConfiguration
	anyExpanded := false
	expansionMap := make(map[string][]string) // original request name → expanded request names

	for _, req := range requests {
		if req.Exactly == nil {
			expandedRequests = append(expandedRequests, req)
			continue
		}

		partitionConfig, managed, err := ce.lookupPartitionConfig(ctx, req.Exactly.DeviceClassName)
		if err != nil {
			klog.Warningf("Failed to get partition config for DeviceClass %q: %v", req.Exactly.DeviceClassName, err)
			if managed {
				return nil, err
			}
			expandedRequests = append(expandedRequests, req)
			continue
		}

		if partitionConfig == nil {
			expandedRequests = append(expandedRequests, req)
			continue
		}
		// Expand this request into sub-resource requests
		subRequests, subConstraints, subConfigs := ce.expandRequest(req, partitionConfig)
		expandedRequests = append(expandedRequests, subRequests...)
		constraints = append(constraints, subConstraints...)
		explicitConfigs = append(explicitConfigs, subConfigs...)
		anyExpanded = true

		// Track the expansion mapping for Pod rewriting
		var expandedNames []string
		for _, sr := range subRequests {
			expandedNames = append(expandedNames, sr.Name)
		}
		expansionMap[req.Name] = expandedNames
	}

	if !anyExpanded {
		return nil, nil
	}
	if err := validateExpandedRequestNames(expandedRequests); err != nil {
		return nil, &managedExpansionError{err: err}
	}

	var patches []jsonPatch
	remappedConstraints, constraintsChanged := remapConstraints(claim.Spec.Devices.Constraints, expansionMap)
	remappedConfigs, configsChanged := remapConfigs(claim.Spec.Devices.Config, expansionMap)

	// Replace requests
	patches = append(patches, jsonPatch{
		Op:    "replace",
		Path:  "/spec/devices/requests",
		Value: expandedRequests,
	})

	// Add or replace constraints
	allConstraints := append(remappedConstraints, constraints...)
	if len(allConstraints) > 0 && (constraintsChanged || len(constraints) > 0) {
		if len(claim.Spec.Devices.Constraints) == 0 {
			patches = append(patches, jsonPatch{
				Op:    "add",
				Path:  "/spec/devices/constraints",
				Value: allConstraints,
			})
		} else {
			patches = append(patches, jsonPatch{
				Op:    "replace",
				Path:  "/spec/devices/constraints",
				Value: allConstraints,
			})
		}
	}

	// Copy only explicitly declared driver configurations. The coordinator
	// does not infer VFIO behavior from a class or driver name.
	configsToAdd := make([]resourcev1.DeviceClaimConfiguration, 0, len(explicitConfigs))
	if len(explicitConfigs) > 0 {
		sort.SliceStable(explicitConfigs, func(i, j int) bool {
			return deviceConfigKey(explicitConfigs[i]) < deviceConfigKey(explicitConfigs[j])
		})
		for _, cfg := range explicitConfigs {
			existingConfigs := append(append([]resourcev1.DeviceClaimConfiguration(nil), remappedConfigs...), configsToAdd...)
			if deviceConfigCovered(existingConfigs, cfg) {
				continue
			}
			configsToAdd = append(configsToAdd, cfg)
		}
		if len(configsToAdd) > 0 {
			// Generated configs go first: later entries win in DRA, and a
			// user's own config for the same request must take precedence.
			allConfigs := append(append([]resourcev1.DeviceClaimConfiguration(nil), configsToAdd...), remappedConfigs...)
			if len(claim.Spec.Devices.Config) == 0 {
				patches = append(patches, jsonPatch{
					Op:    "add",
					Path:  "/spec/devices/config",
					Value: allConfigs,
				})
			} else {
				patches = append(patches, jsonPatch{
					Op:    "replace",
					Path:  "/spec/devices/config",
					Value: allConfigs,
				})
			}
		}
	}
	if configsChanged && len(configsToAdd) == 0 {
		patches = append(patches, jsonPatch{
			Op:    "replace",
			Path:  "/spec/devices/config",
			Value: remappedConfigs,
		})
	}

	// Annotate the claim with the expansion mapping so the Pod webhook
	// can rewrite container request references without re-fetching DeviceClasses.
	if len(expansionMap) > 0 {
		mapJSON, err := json.Marshal(expansionMap)
		if err != nil {
			klog.Warningf("Failed to marshal expansion map: %v", err)
		} else {
			annotations := claim.Annotations
			if annotations == nil {
				annotations = make(map[string]string)
			}
			annotations[controller.CoordinatorDriverName+"/expanded-requests"] = string(mapJSON)
			if claim.Annotations == nil {
				patches = append(patches, jsonPatch{
					Op:    "add",
					Path:  "/metadata/annotations",
					Value: annotations,
				})
			} else {
				patches = append(patches, jsonPatch{
					Op:    "add",
					Path:  "/metadata/annotations/" + escapeJSONPointer(controller.CoordinatorDriverName+"/expanded-requests"),
					Value: string(mapJSON),
				})
			}
		}
	}

	return patches, nil
}

// expandRequest expands a partition DeviceRequest into sub-resource requests.
// When req.Exactly.Count > 1, creates N independent partition instances, each
// with its own set of sub-requests and alignment constraints.
func (ce *ClaimExpander) expandRequest(req resourcev1.DeviceRequest, config *controller.PartitionConfig) ([]resourcev1.DeviceRequest, []resourcev1.DeviceConstraint, []resourcev1.DeviceClaimConfiguration) {
	count := int64(1)
	if req.Exactly != nil && req.Exactly.Count > 1 {
		count = req.Exactly.Count
	}

	var allSubRequests []resourcev1.DeviceRequest
	var allConstraints []resourcev1.DeviceConstraint
	var allConfigs []resourcev1.DeviceClaimConfiguration

	for i := int64(0); i < count; i++ {
		prefix := req.Name
		if count > 1 {
			prefix = fmt.Sprintf("%s-%d", req.Name, i)
		}
		subRequests, constraints, configs := ce.expandSinglePartition(prefix, req, config)
		allSubRequests = append(allSubRequests, subRequests...)
		allConstraints = append(allConstraints, constraints...)
		allConfigs = append(allConfigs, configs...)
	}

	return allSubRequests, allConstraints, allConfigs
}

// expandSinglePartition expands one partition instance into sub-resource requests
// and alignment constraints. The prefix determines the naming of generated requests.
//
// Passthrough device types (GPU, NIC) with count>1 are split into individual
// count=1 requests so each gets a unique name. This enables KubeVirt to create
// one hostDevice per physical device. Non-passthrough devices (CPU, memory)
// keep their original count since they use consumable capacity, not PCI passthrough.
// The alignment constraints tie all split requests together so the scheduler
// places them on the same NUMA/pcieRoot.
func (ce *ClaimExpander) expandSinglePartition(prefix string, req resourcev1.DeviceRequest, config *controller.PartitionConfig) ([]resourcev1.DeviceRequest, []resourcev1.DeviceConstraint, []resourcev1.DeviceClaimConfiguration) {
	var subRequests []resourcev1.DeviceRequest
	var constraints []resourcev1.DeviceConstraint
	var explicitConfigs []resourcev1.DeviceClaimConfiguration

	requestNameMap := make(map[string][]string)

	for _, sr := range config.SubResources {
		sanitized := sanitizeDeviceClassName(sr.DeviceClass)
		var requestNames []string

		splitCount := 1
		deviceCount := sr.Count
		if config.Mode == controller.OfferingModePassthrough && sr.Passthrough && len(sr.Capacity) == 0 && sr.Count > 1 {
			splitCount = sr.Count
			deviceCount = 1
		}

		for si := 0; si < splitCount; si++ {
			var name string
			if splitCount > 1 {
				name = fmt.Sprintf("%s-%s-%d", prefix, sanitized, si)
			} else {
				name = prefix + "-" + sanitized
			}
			requestNameMap[sr.DeviceClass] = append(requestNameMap[sr.DeviceClass], name)
			requestNames = append(requestNames, name)

			exact := &resourcev1.ExactDeviceRequest{
				DeviceClassName: sr.DeviceClass,
				Count:           int64(deviceCount),
			}

			// Add capacity requests for shared devices (DRAConsumableCapacity)
			if len(sr.Capacity) > 0 {
				exact.Capacity = &resourcev1.CapacityRequirements{
					Requests: make(map[resourcev1.QualifiedName]resource.Quantity),
				}
				for capName, capVal := range sr.Capacity {
					qty, err := resource.ParseQuantity(capVal)
					if err == nil {
						exact.Capacity.Requests[resourcev1.QualifiedName(capName)] = qty
					}
				}
			}

			// Apply per-driver CEL selectors from the PartitionConfig.
			for _, cel := range sr.Selectors {
				exact.Selectors = append(exact.Selectors, resourcev1.DeviceSelector{
					CEL: &resourcev1.CELDeviceSelector{
						Expression: cel,
					},
				})
			}

			// Forward any user-specified selectors from the original partition request
			if req.Exactly != nil && len(req.Exactly.Selectors) > 0 {
				exact.Selectors = append(exact.Selectors, req.Exactly.Selectors...)
			}

			subRequests = append(subRequests, resourcev1.DeviceRequest{
				Name:    name,
				Exactly: exact,
			})
		}

		if config.Mode == controller.OfferingModePassthrough && sr.Passthrough && sr.Opaque != nil {
			explicitConfigs = append(explicitConfigs, resourcev1.DeviceClaimConfiguration{
				Requests: requestNames,
				DeviceConfiguration: resourcev1.DeviceConfiguration{
					Opaque: &resourcev1.OpaqueDeviceConfiguration{
						Driver:     sr.Opaque.Driver,
						Parameters: runtime.RawExtension{Raw: append([]byte(nil), sr.Opaque.Parameters...)},
					},
				},
			})
		}
	}

	// Build driver counts map for satisfiability checks
	driverCounts := make(map[string]int, len(config.SubResources))
	for _, sr := range config.SubResources {
		driver := sr.SourceDriver
		if driver == "" {
			driver = sr.DeviceClass
		}
		driverCounts[driver] = sr.Count
	}

	// Build constraints from alignments.
	// When sub-resources are split (count>1 → individual requests), constraints
	// need special handling:
	//
	// For pcieRoot-type constraints (device-level pairing): create per-index
	// constraints that pair corresponding devices (gpu-0+nic-0, gpu-1+nic-1).
	// Each pair must share the same pcieRoot, but different pairs can be on
	// different roots.
	//
	// For numaNode-type constraints (partition-level grouping): create one
	// global constraint covering all requests. All devices in the partition
	// must share the same NUMA node.
	for _, alignment := range config.Alignments {
		if alignment.Enforcement == controller.EnforcementPreferred {
			if ce.model == nil || !ce.model.IsConstraintSatisfiable(alignment.Attribute, driverCounts) {
				klog.V(2).Infof("Skipping preferred constraint %s: not satisfiable (or no topology model)", alignment.Attribute)
				continue
			}
		}

		// Collect resolved request names per driver, preserving order
		type driverRequests struct {
			driver string
			names  []string
		}
		var perDriver []driverRequests
		for _, reqName := range alignment.Requests {
			if mapped, ok := requestNameMap[reqName]; ok {
				perDriver = append(perDriver, driverRequests{reqName, mapped})
			} else {
				found := false
				for _, sr := range config.SubResources {
					sanitized := sanitizeDeviceClassName(sr.DeviceClass)
					if reqName == sanitized {
						perDriver = append(perDriver, driverRequests{reqName, []string{prefix + "-" + sanitized}})
						found = true
						break
					}
				}
				if !found {
					continue
				}
			}
		}

		// Check if any driver has split requests (more than 1 name)
		maxSplit := 0
		for _, dr := range perDriver {
			if len(dr.names) > maxSplit {
				maxSplit = len(dr.names)
			}
		}

		attr := resourcev1.FullyQualifiedName(alignment.Attribute)

		if maxSplit <= 1 {
			// No splits — single constraint covering all requests
			var allRequests []string
			for _, dr := range perDriver {
				allRequests = append(allRequests, dr.names...)
			}
			if len(allRequests) >= 2 {
				constraints = append(constraints, resourcev1.DeviceConstraint{
					Requests:       allRequests,
					MatchAttribute: &attr,
				})
			}
		} else if strings.Contains(string(attr), "numaNode") {
			// NUMA constraint: global — all devices must share the same NUMA
			var allRequests []string
			for _, dr := range perDriver {
				allRequests = append(allRequests, dr.names...)
			}
			if len(allRequests) >= 2 {
				constraints = append(constraints, resourcev1.DeviceConstraint{
					Requests:       allRequests,
					MatchAttribute: &attr,
				})
			}
		} else {
			// Per-pair constraints (pcieRoot, etc.): pair by index.
			// gpu-0+nic-0 on same root, gpu-1+nic-1 on same root, etc.
			for idx := 0; idx < maxSplit; idx++ {
				var pairRequests []string
				for _, dr := range perDriver {
					if idx < len(dr.names) {
						pairRequests = append(pairRequests, dr.names[idx])
					} else if len(dr.names) == 1 {
						// Non-split driver (e.g., CPU) — include in every pair
						pairRequests = append(pairRequests, dr.names[0])
					}
				}
				if len(pairRequests) >= 2 {
					constraints = append(constraints, resourcev1.DeviceConstraint{
						Requests:       pairRequests,
						MatchAttribute: &attr,
					})
				}
			}
		}
	}

	return subRequests, constraints, explicitConfigs
}

func validateExpandedRequestNames(requests []resourcev1.DeviceRequest) error {
	seen := make(map[string]struct{}, len(requests))
	for _, request := range requests {
		if _, exists := seen[request.Name]; exists {
			return fmt.Errorf("expanded request name %q is duplicated", request.Name)
		}
		if problems := validation.IsDNS1123Label(request.Name); len(problems) > 0 {
			return fmt.Errorf("expanded request name %q is invalid: %s", request.Name, strings.Join(problems, "; "))
		}
		seen[request.Name] = struct{}{}
	}
	return nil
}

func remapConstraints(constraints []resourcev1.DeviceConstraint, mapping map[string][]string) ([]resourcev1.DeviceConstraint, bool) {
	if len(constraints) == 0 {
		return nil, false
	}
	remapped := make([]resourcev1.DeviceConstraint, 0, len(constraints))
	changed := false
	for _, constraint := range constraints {
		copyConstraint := constraint
		copyConstraint.Requests = nil
		for _, request := range constraint.Requests {
			expanded, ok := mapping[request]
			if !ok {
				copyConstraint.Requests = append(copyConstraint.Requests, request)
				continue
			}
			copyConstraint.Requests = append(copyConstraint.Requests, expanded...)
			changed = true
		}
		remapped = append(remapped, copyConstraint)
	}
	return remapped, changed
}

func remapConfigs(configs []resourcev1.DeviceClaimConfiguration, mapping map[string][]string) ([]resourcev1.DeviceClaimConfiguration, bool) {
	if len(configs) == 0 {
		return nil, false
	}
	remapped := make([]resourcev1.DeviceClaimConfiguration, 0, len(configs))
	changed := false
	for _, config := range configs {
		copyConfig := config
		copyConfig.Requests = nil
		if len(config.Requests) == 0 {
			remapped = append(remapped, copyConfig)
			continue
		}
		for _, request := range config.Requests {
			expanded, ok := mapping[request]
			if !ok {
				copyConfig.Requests = append(copyConfig.Requests, request)
				continue
			}
			copyConfig.Requests = append(copyConfig.Requests, expanded...)
			changed = true
		}
		remapped = append(remapped, copyConfig)
	}
	return remapped, changed
}

// getPartitionConfig looks up a DeviceClass and extracts the PartitionConfig
// from its opaque configuration if present.
func (ce *ClaimExpander) getPartitionConfig(ctx context.Context, deviceClassName string) (*controller.PartitionConfig, error) {
	config, _, err := ce.lookupPartitionConfig(ctx, deviceClassName)
	return config, err
}

func (ce *ClaimExpander) lookupPartitionConfig(ctx context.Context, deviceClassName string) (*controller.PartitionConfig, bool, error) {
	dc, err := ce.client.ResourceV1().DeviceClasses().Get(ctx, deviceClassName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// A missing class is left to the normal DRA validation path. This
			// preserves pass-through behavior for native claims.
			return nil, false, nil
		}
		return nil, true, &managedExpansionError{err: fmt.Errorf("failed to get DeviceClass %q: %w", deviceClassName, err)}
	}
	managed := dc.Labels[controller.CoordinatorDriverName+"/managed"] == "true"

	for _, cfg := range dc.Spec.Config {
		if cfg.Opaque == nil {
			continue
		}
		if cfg.Opaque.Driver != ce.driverName {
			continue
		}

		var partConfig controller.PartitionConfig
		if err := json.Unmarshal(cfg.Opaque.Parameters.Raw, &partConfig); err != nil {
			klog.Warningf("Failed to unmarshal opaque config for DeviceClass %q: %v", deviceClassName, err)
			if managed {
				return nil, true, &managedExpansionError{err: fmt.Errorf("managed DeviceClass %q has invalid PartitionConfig: %w", deviceClassName, err)}
			}
			continue
		}

		if partConfig.Kind != "PartitionConfig" {
			if managed {
				return nil, true, &managedExpansionError{err: fmt.Errorf("managed DeviceClass %q does not contain a PartitionConfig", deviceClassName)}
			}
			continue
		}
		if err := partConfig.Validate(); err != nil {
			if managed {
				return nil, true, &managedExpansionError{err: fmt.Errorf("managed DeviceClass %q has invalid PartitionConfig: %w", deviceClassName, err)}
			}
			continue
		}

		return &partConfig, managed, nil
	}

	if managed {
		return nil, true, &managedExpansionError{err: fmt.Errorf("managed DeviceClass %q has no PartitionConfig", deviceClassName)}
	}
	return nil, false, nil
}

// sanitizeDeviceClassName converts a device class name into a DNS-label-safe suffix.
func sanitizeDeviceClassName(name string) string {
	sanitized := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return '-'
	}, name)

	// Collapse consecutive dashes
	for strings.Contains(sanitized, "--") {
		sanitized = strings.ReplaceAll(sanitized, "--", "-")
	}
	sanitized = strings.Trim(sanitized, "-")

	if sanitized == "" {
		sanitized = "sub"
	}

	return sanitized
}

// handlePodAdmission rewrites container resource claim request names to match
// expanded partition request names. This makes partition expansion transparent
// to consumers like KubeVirt that read KEP-5304 metadata by request name.
func (ce *ClaimExpander) handlePodAdmission(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		klog.Errorf("Failed to unmarshal Pod: %v", err)
		return allowResponse()
	}

	// Build a map of pod claim name → expansion mapping by looking up referenced claims
	type claimExpansion struct {
		mapping map[string][]string // original request → expanded requests
	}
	claimExpansions := make(map[string]*claimExpansion) // pod claim name → expansion

	for _, prc := range pod.Spec.ResourceClaims {
		if prc.ResourceClaimName != nil && *prc.ResourceClaimName != "" {
			claimName := *prc.ResourceClaimName
			claim, err := ce.client.ResourceV1().ResourceClaims(req.Namespace).Get(ctx, claimName, metav1.GetOptions{})
			if err != nil {
				klog.V(4).Infof("Could not look up claim %s/%s for pod rewriting: %v", req.Namespace, claimName, err)
				continue
			}
			annotation := claim.Annotations[controller.CoordinatorDriverName+"/expanded-requests"]
			if annotation == "" {
				continue
			}
			var mapping map[string][]string
			if err := json.Unmarshal([]byte(annotation), &mapping); err != nil {
				klog.Warningf("Failed to parse expansion annotation on claim %s: %v", claimName, err)
				continue
			}
			claimExpansions[prc.Name] = &claimExpansion{mapping: mapping}
			continue
		}

		if prc.ResourceClaimTemplateName != nil && *prc.ResourceClaimTemplateName != "" {
			templateName := *prc.ResourceClaimTemplateName
			template, err := ce.client.ResourceV1().ResourceClaimTemplates(req.Namespace).Get(ctx, templateName, metav1.GetOptions{})
			if err != nil {
				return denyResponse(fmt.Sprintf("failed to get ResourceClaimTemplate %s/%s: %v", req.Namespace, templateName, err))
			}
			mapping, err := ce.expansionMapping(ctx, template.Spec.Spec.Devices.Requests)
			if err != nil {
				klog.Warningf("Failed to expand claim template %s: %v", templateName, err)
				var managedErr *managedExpansionError
				if errors.As(err, &managedErr) {
					return denyResponse(err.Error())
				}
				continue
			}
			if len(mapping) > 0 {
				claimExpansions[prc.Name] = &claimExpansion{mapping: mapping}
			}
		}
	}

	if len(claimExpansions) == 0 {
		return allowResponse()
	}

	var patches []jsonPatch

	// Rewrite container claim references
	rewriteContainerClaims := func(containerPath string, containers []corev1.Container) {
		for i, ctr := range containers {
			var newClaims []corev1.ResourceClaim
			changed := false
			for _, rc := range ctr.Resources.Claims {
				exp, ok := claimExpansions[rc.Name]
				if !ok || rc.Request == "" {
					newClaims = append(newClaims, rc)
					continue
				}
				expandedNames, ok := exp.mapping[rc.Request]
				if !ok {
					newClaims = append(newClaims, rc)
					continue
				}
				// Expand into N entries, one per sub-request
				for _, expandedName := range expandedNames {
					newClaims = append(newClaims, corev1.ResourceClaim{
						Name:    rc.Name,
						Request: expandedName,
					})
				}
				changed = true
			}
			if changed {
				patches = append(patches, jsonPatch{
					Op:    "replace",
					Path:  fmt.Sprintf("%s/%d/resources/claims", containerPath, i),
					Value: newClaims,
				})
			}
		}
	}

	rewriteContainerClaims("/spec/containers", pod.Spec.Containers)
	rewriteContainerClaims("/spec/initContainers", pod.Spec.InitContainers)

	if len(patches) == 0 {
		return allowResponse()
	}

	podID := pod.Name
	if podID == "" {
		podID = pod.GenerateName + "*"
	}
	klog.Infof("Rewriting %d container claim references in pod %s/%s", len(patches), req.Namespace, podID)

	patchBytes, err := json.Marshal(patches)
	if err != nil {
		klog.Errorf("Failed to marshal pod patches: %v", err)
		return allowResponse()
	}

	patchType := admissionv1.PatchTypeJSONPatch
	return &admissionv1.AdmissionResponse{
		Allowed:   true,
		PatchType: &patchType,
		Patch:     patchBytes,
	}
}

func (ce *ClaimExpander) expansionMapping(ctx context.Context, requests []resourcev1.DeviceRequest) (map[string][]string, error) {
	mapping := make(map[string][]string)
	for _, req := range requests {
		if req.Exactly == nil {
			continue
		}
		config, err := ce.getPartitionConfig(ctx, req.Exactly.DeviceClassName)
		if err != nil {
			return nil, err
		}
		if config == nil {
			continue
		}
		subRequests, _, _ := ce.expandRequest(req, config)
		for _, subRequest := range subRequests {
			mapping[req.Name] = append(mapping[req.Name], subRequest.Name)
		}
	}
	return mapping, nil
}

// handleVMIAdmission auto-generates hostDevices for KubeVirt VMIs that
// reference partition ResourceClaimTemplates, and rewrites requestName fields
// on any existing hostDevices/gpus to match expanded partition request names.
func (ce *ClaimExpander) handleVMIAdmission(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	var vmi map[string]interface{}
	if err := json.Unmarshal(req.Object.Raw, &vmi); err != nil {
		klog.Errorf("Failed to unmarshal VMI: %v", err)
		return allowResponse()
	}

	var patches []jsonPatch

	resourceClaims := nestedSlice(vmi, "spec", "resourceClaims")
	existingHostDevices := nestedSlice(vmi, "spec", "domain", "devices", "hostDevices")
	hdIdx := len(existingHostDevices)
	globalDevIdx := 0
	templateMappings := make(map[string]map[string][]string)

	// For each resourceClaim referencing a template, look up the template's
	// DeviceClass config and auto-generate hostDevices if none exist for it.
	for _, rc := range resourceClaims {
		rcMap, ok := rc.(map[string]interface{})
		if !ok {
			continue
		}
		rcName, _ := rcMap["name"].(string)
		templateName, _ := rcMap["resourceClaimTemplateName"].(string)
		if templateName == "" {
			continue
		}

		// Resolve every template before deciding whether to generate devices.
		// Existing devices still need the template mapping for request rewrites.
		tpl, err := ce.client.ResourceV1().ResourceClaimTemplates(req.Namespace).Get(ctx, templateName, metav1.GetOptions{})
		if err != nil {
			return denyResponse(fmt.Sprintf("failed to get ResourceClaimTemplate %s/%s: %v", req.Namespace, templateName, err))
		}
		mapping, err := ce.expansionMapping(ctx, tpl.Spec.Spec.Devices.Requests)
		if err != nil {
			return denyResponse(fmt.Sprintf("failed to expand ResourceClaimTemplate %s/%s: %v", req.Namespace, templateName, err))
		}
		if len(mapping) > 0 {
			templateMappings[rcName] = mapping
		}

		// Check if the VMI already has hostDevices for this claim
		hasDevices := false
		for _, hd := range existingHostDevices {
			hdMap, _ := hd.(map[string]interface{})
			if hdMap["claimName"] == rcName {
				hasDevices = true
				break
			}
		}
		if hasDevices {
			continue
		}
		// Find the partition request and its DeviceClass
		for _, tplReq := range tpl.Spec.Spec.Devices.Requests {
			if tplReq.Exactly == nil || tplReq.Exactly.DeviceClassName == "" {
				continue
			}

			config, err := ce.getPartitionConfig(ctx, tplReq.Exactly.DeviceClassName)
			if err != nil {
				var managedErr *managedExpansionError
				if errors.As(err, &managedErr) {
					return denyResponse(err.Error())
				}
				continue
			}
			if config == nil {
				continue
			}

			count := int64(1)
			if tplReq.Exactly.Count > 1 {
				count = tplReq.Exactly.Count
			}

			// Use the explicit offering metadata to identify VM passthrough devices.
			var passthroughDevices []struct {
				class    string
				nameHint string
				srCount  int
			}
			if config.Mode == controller.OfferingModePassthrough {
				for _, sr := range config.SubResources {
					if !sr.Passthrough {
						continue
					}
					hint := "device"
					lc := strings.ToLower(sr.DeviceClass)
					if strings.Contains(lc, "gpu") {
						hint = "gpu"
					} else if strings.Contains(lc, "net") || strings.Contains(lc, "sriov") || strings.Contains(lc, "rdma") {
						hint = "nic"
					}
					splitCount := sr.Count
					if len(sr.Capacity) > 0 || splitCount <= 1 {
						splitCount = 1
					}
					passthroughDevices = append(passthroughDevices, struct {
						class    string
						nameHint string
						srCount  int
					}{sr.DeviceClass, hint, splitCount})
				}
			}

			if len(passthroughDevices) == 0 {
				continue
			}

			// Generate hostDevices for each partition instance
			for i := int64(0); i < count; i++ {
				for _, pd := range passthroughDevices {
					for si := 0; si < pd.srCount; si++ {
						// Build the expanded request name matching the split
						// pattern from claim expansion
						var requestName string
						sanitized := sanitizeDeviceClassName(pd.class)
						prefix := tplReq.Name
						if count > 1 {
							prefix = fmt.Sprintf("%s-%d", tplReq.Name, i)
						}
						requestName = fmt.Sprintf("%s-%s", prefix, sanitized)
						if pd.srCount > 1 {
							requestName = fmt.Sprintf("%s-%s-%d", prefix, sanitized, si)
						}

						deviceName := fmt.Sprintf("%s%d", pd.nameHint, globalDevIdx)
						globalDevIdx++

						hostDevice := map[string]interface{}{
							"name":        deviceName,
							"claimName":   rcName,
							"requestName": requestName,
						}

						if hdIdx == 0 && len(existingHostDevices) == 0 {
							patches = append(patches, jsonPatch{
								Op:    "add",
								Path:  "/spec/domain/devices/hostDevices",
								Value: []interface{}{hostDevice},
							})
							hdIdx++
						} else {
							patches = append(patches, jsonPatch{
								Op:    "add",
								Path:  "/spec/domain/devices/hostDevices/-",
								Value: hostDevice,
							})
							hdIdx++
						}
					}
				}
			}

			generatedHostDevices := 0
			for _, pd := range passthroughDevices {
				generatedHostDevices += pd.srCount
			}
			vmiName, _ := vmi["metadata"].(map[string]interface{})["name"].(string)
			klog.Infof("Auto-generated %d hostDevices for VMI %s/%s from template %s (count=%d)",
				int(count)*generatedHostDevices, req.Namespace, vmiName, templateName, count)
		}
	}

	// Also handle existing hostDevices/gpus that need requestName rewriting
	// (for manually specified devices referencing pre-expansion names)
	expansionMap := make(map[string]map[string][]string)
	for _, rc := range resourceClaims {
		rcMap, ok := rc.(map[string]interface{})
		if !ok {
			continue
		}
		claimName, _ := rcMap["resourceClaimName"].(string)
		if claimName == "" {
			claimName, _ = rcMap["name"].(string)
			if mapping := templateMappings[claimName]; len(mapping) > 0 {
				expansionMap[claimName] = mapping
			}
			continue
		}
		claim, err := ce.client.ResourceV1().ResourceClaims(req.Namespace).Get(ctx, claimName, metav1.GetOptions{})
		if err != nil {
			continue
		}
		annotation := claim.Annotations[controller.CoordinatorDriverName+"/expanded-requests"]
		if annotation == "" {
			continue
		}
		var mapping map[string][]string
		if err := json.Unmarshal([]byte(annotation), &mapping); err != nil {
			continue
		}
		rcName, _ := rcMap["name"].(string)
		expansionMap[rcName] = mapping
	}

	if len(expansionMap) > 0 {
		usedExpanded := make(map[string]map[string]bool)
		gpus := nestedSlice(vmi, "spec", "domain", "devices", "gpus")
		for i, g := range gpus {
			gMap, ok := g.(map[string]interface{})
			if !ok {
				continue
			}
			claimName, _ := gMap["claimName"].(string)
			requestName, _ := gMap["requestName"].(string)
			if claimName == "" || requestName == "" {
				continue
			}
			mapping, ok := expansionMap[claimName]
			if !ok {
				continue
			}
			expanded, ok := mapping[requestName]
			if !ok || len(expanded) == 0 {
				continue
			}
			key := claimName + "\x00" + requestName
			name, ok := pickExpandedRequest(expanded, "gpu", "", usedExpanded[key])
			if !ok {
				continue
			}
			if usedExpanded[key] == nil {
				usedExpanded[key] = make(map[string]bool)
			}
			usedExpanded[key][name] = true
			patches = append(patches, jsonPatch{
				Op:    "replace",
				Path:  fmt.Sprintf("/spec/domain/devices/gpus/%d/requestName", i),
				Value: name,
			})
		}

		hostDevices := nestedSlice(vmi, "spec", "domain", "devices", "hostDevices")
		for i, hd := range hostDevices {
			hdMap, ok := hd.(map[string]interface{})
			if !ok {
				continue
			}
			claimName, _ := hdMap["claimName"].(string)
			requestName, _ := hdMap["requestName"].(string)
			if claimName == "" || requestName == "" {
				continue
			}
			mapping, ok := expansionMap[claimName]
			if !ok {
				continue
			}
			expanded, ok := mapping[requestName]
			if !ok || len(expanded) == 0 {
				continue
			}
			deviceName, _ := hdMap["name"].(string)
			key := claimName + "\x00" + requestName
			name, ok := pickExpandedRequest(expanded, "host", deviceName, usedExpanded[key])
			if !ok {
				continue
			}
			if usedExpanded[key] == nil {
				usedExpanded[key] = make(map[string]bool)
			}
			usedExpanded[key][name] = true
			patches = append(patches, jsonPatch{Op: "replace", Path: fmt.Sprintf("/spec/domain/devices/hostDevices/%d/requestName", i), Value: name})
		}
	}

	if len(patches) == 0 {
		return allowResponse()
	}

	vmiName, _ := vmi["metadata"].(map[string]interface{})["name"].(string)
	klog.Infof("Patching %d fields in VMI %s/%s", len(patches), req.Namespace, vmiName)

	patchBytes, err := json.Marshal(patches)
	if err != nil {
		klog.Errorf("Failed to marshal VMI patches: %v", err)
		return allowResponse()
	}

	patchType := admissionv1.PatchTypeJSONPatch
	return &admissionv1.AdmissionResponse{
		Allowed:   true,
		PatchType: &patchType,
		Patch:     patchBytes,
	}
}

func pickExpandedRequest(names []string, kind, deviceName string, used map[string]bool) (string, bool) {
	deviceName = strings.ToLower(deviceName)
	for _, name := range names {
		if used != nil && used[name] {
			continue
		}
		lower := strings.ToLower(name)
		matches := true
		switch kind {
		case "gpu":
			matches = strings.Contains(lower, "gpu")
		case "host":
			switch {
			case strings.Contains(deviceName, "gpu"):
				matches = strings.Contains(lower, "gpu")
			case strings.Contains(deviceName, "nic"), strings.Contains(deviceName, "net"), strings.Contains(deviceName, "rdma"), strings.Contains(deviceName, "sriov"):
				matches = strings.Contains(lower, "net") || strings.Contains(lower, "rdma") || strings.Contains(lower, "sriov")
			case strings.Contains(deviceName, "nvme"):
				matches = strings.Contains(lower, "nvme")
			}
		}
		if matches {
			return name, true
		}
	}
	return "", false
}

func sanitizeForRequestName(s string) string {
	return sanitizeDeviceClassName(s)
}

// nestedSlice extracts a nested []interface{} from a map hierarchy.
func nestedSlice(obj map[string]interface{}, fields ...string) []interface{} {
	current := obj
	for i, field := range fields {
		if i == len(fields)-1 {
			val, _ := current[field].([]interface{})
			return val
		}
		next, ok := current[field].(map[string]interface{})
		if !ok {
			return nil
		}
		current = next
	}
	return nil
}

// escapeJSONPointer escapes special characters in JSON Pointer tokens (RFC 6901).
func escapeJSONPointer(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	s = strings.ReplaceAll(s, "/", "~1")
	return s
}

func deviceConfigKey(config resourcev1.DeviceClaimConfiguration) string {
	if config.Opaque == nil {
		return ""
	}
	requests := append([]string(nil), config.Requests...)
	sort.Strings(requests)
	return strings.Join([]string{
		config.Opaque.Driver,
		canonicalJSON(config.Opaque.Parameters.Raw),
		strings.Join(requests, ","),
	}, "|")
}

func deviceConfigCovered(existing []resourcev1.DeviceClaimConfiguration, candidate resourcev1.DeviceClaimConfiguration) bool {
	if candidate.Opaque == nil {
		return false
	}
	candidateRequests := requestSet(candidate.Requests)
	for _, current := range existing {
		if current.Opaque == nil || current.Opaque.Driver != candidate.Opaque.Driver {
			continue
		}
		if canonicalJSON(current.Opaque.Parameters.Raw) != canonicalJSON(candidate.Opaque.Parameters.Raw) {
			continue
		}
		if len(current.Requests) == 0 {
			return true
		}
		if len(candidateRequests) == 0 {
			continue
		}
		currentRequests := requestSet(current.Requests)
		covered := true
		for request := range candidateRequests {
			if !currentRequests[request] {
				covered = false
				break
			}
		}
		if covered {
			return true
		}
	}
	return false
}

func requestSet(requests []string) map[string]bool {
	set := make(map[string]bool, len(requests))
	for _, request := range requests {
		set[request] = true
	}
	return set
}

func canonicalJSON(raw []byte) string {
	var value interface{}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return string(raw)
	}
	data, err := json.Marshal(value)
	if err != nil {
		return string(raw)
	}
	return string(data)
}

// allowResponse returns an admission response that allows the request without mutation.
func allowResponse() *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{
		Allowed: true,
	}
}

func denyResponse(message string) *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{
		Allowed: false,
		Result:  &metav1.Status{Message: message},
	}
}
