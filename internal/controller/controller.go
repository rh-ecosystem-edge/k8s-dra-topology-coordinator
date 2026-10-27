package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	klog "k8s.io/klog/v2"

	"github.com/rh-ecosystem-edge/k8s-dra-topology-coordinator/internal/metrics"
)

const (
	defaultResyncPeriod    = 5 * time.Minute
	reconcileDebounceDelay = 2 * time.Second
)

// PartitionMode determines which partitioning strategy the controller uses.
type PartitionMode string

const (
	PartitionModeAuto       PartitionMode = "auto"
	PartitionModePartitions PartitionMode = "partitions"
	PartitionModeGroupings  PartitionMode = "groupings"
)

// ValidPartitionMode returns true if the given mode string is recognized.
func ValidPartitionMode(mode string) bool {
	switch PartitionMode(mode) {
	case PartitionModeAuto, PartitionModePartitions, PartitionModeGroupings:
		return true
	}
	return false
}

// Controller is the main topology coordinator controller.
// It watches ResourceSlices and ConfigMaps, builds a cross-driver topology model,
// computes aligned partitions, and publishes DeviceClasses describing partition shapes.
type Controller struct {
	client        kubernetes.Interface
	driverName    string
	partitionMode PartitionMode

	model            *TopologyModel
	ruleStore        *TopologyRuleStore
	groupingStore    *GroupingStore
	partitionBuilder *PartitionBuilder
	groupingBuilder  *GroupingBuilder
	classManager     *DeviceClassManager

	// synced is set after informer caches are synchronized. It is atomic because
	// informer callbacks can run while StartCache is finishing.
	// Event handlers skip enqueueing reconciles until this is true.
	synced atomic.Bool

	// workqueue triggers a full reconciliation when topology changes
	workqueue  workqueue.TypedRateLimitingInterface[string]
	publishing atomic.Bool
}

// NewController creates a new topology coordinator controller.
func NewController(client kubernetes.Interface, driverName string, partitionMode PartitionMode) *Controller {
	if driverName == "" {
		driverName = CoordinatorDriverName
	}
	if partitionMode == "" {
		partitionMode = PartitionModeAuto
	}

	model := NewTopologyModel()
	ruleStore := NewTopologyRuleStore()
	groupingStore := NewGroupingStore()

	klog.Infof("Partition mode: %s", partitionMode)

	return &Controller{
		client:           client,
		driverName:       driverName,
		partitionMode:    partitionMode,
		model:            model,
		ruleStore:        ruleStore,
		groupingStore:    groupingStore,
		partitionBuilder: NewPartitionBuilder(model, ruleStore),
		groupingBuilder:  NewGroupingBuilder(model, ruleStore),
		classManager:     NewDeviceClassManager(client, driverName, ruleStore),
		workqueue:        workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
	}
}

// Model returns the topology model for use by the webhook.
func (c *Controller) Model() *TopologyModel {
	return c.model
}

// StartCache starts the informer caches and populates the topology model. It
// returns after the local cache is synchronized; callers may then serve
// admission while a leader independently controls publication.
func (c *Controller) StartCache(ctx context.Context) error {
	klog.Info("Starting topology coordinator cache")
	c.workqueue = workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())
	// Set up informers
	factory := informers.NewSharedInformerFactory(c.client, defaultResyncPeriod)

	// Filtered factory for ConfigMaps with the topology-rule label only
	cmFactory := informers.NewSharedInformerFactoryWithOptions(c.client, defaultResyncPeriod,
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = TopologyRuleLabel + "=true"
		}),
	)

	// Filtered factory for ConfigMaps with the device-grouping label
	groupingCMFactory := informers.NewSharedInformerFactoryWithOptions(c.client, defaultResyncPeriod,
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = DeviceGroupingLabel + "=true"
		}),
	)

	// Watch ResourceSlices from ALL drivers
	sliceInformer := factory.Resource().V1().ResourceSlices().Informer()
	if _, err := sliceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onSliceAdd,
		UpdateFunc: c.onSliceUpdate,
		DeleteFunc: c.onSliceDelete,
	}); err != nil {
		return fmt.Errorf("failed to add ResourceSlice event handler: %w", err)
	}

	// Watch only ConfigMaps with the topology-rule label
	cmInformer := cmFactory.Core().V1().ConfigMaps().Informer()
	if _, err := cmInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onConfigMapAdd,
		UpdateFunc: c.onConfigMapUpdate,
		DeleteFunc: c.onConfigMapDelete,
	}); err != nil {
		return fmt.Errorf("failed to add ConfigMap event handler: %w", err)
	}

	// Watch ConfigMaps with the device-grouping label
	groupingCMInformer := groupingCMFactory.Core().V1().ConfigMaps().Informer()
	if _, err := groupingCMInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onGroupingConfigMapAdd,
		UpdateFunc: c.onGroupingConfigMapUpdate,
		DeleteFunc: c.onGroupingConfigMapDelete,
	}); err != nil {
		return fmt.Errorf("failed to add grouping ConfigMap event handler: %w", err)
	}

	// Start informers
	factory.Start(ctx.Done())
	cmFactory.Start(ctx.Done())
	groupingCMFactory.Start(ctx.Done())

	// Wait for cache sync
	if !cache.WaitForCacheSync(ctx.Done(), sliceInformer.HasSynced, cmInformer.HasSynced, groupingCMInformer.HasSynced) {
		return fmt.Errorf("failed to sync informer caches")
	}
	klog.Info("Informer caches synced")

	// Build the model from the complete informer store to avoid
	// partial state from event handlers during startup.
	for _, obj := range sliceInformer.GetStore().List() {
		if slice, ok := obj.(*resourcev1.ResourceSlice); ok {
			c.model.UpdateFromResourceSlice(slice)
		}
	}
	for _, obj := range cmInformer.GetStore().List() {
		if cm, ok := obj.(*corev1.ConfigMap); ok {
			if err := c.ruleStore.LoadFromConfigMap(cm); err != nil {
				klog.Warningf("Failed to load topology rule from %s: %v", cm.Name, err)
			}
		}
	}
	c.refreshModelRules()

	// Load device grouping ConfigMaps
	for _, obj := range groupingCMInformer.GetStore().List() {
		if cm, ok := obj.(*corev1.ConfigMap); ok {
			if err := c.groupingStore.LoadFromConfigMap(cm); err != nil {
				klog.Warningf("Failed to load device grouping from %s: %v", cm.Name, err)
			}
		}
	}

	c.synced.Store(true)
	return nil
}

// RunWorker processes reconciliation events. Non-leaders keep their caches
// current but skip publication until SetPublishing(true) is called.
func (c *Controller) RunWorker(ctx context.Context) error {
	klog.Info("Starting topology coordinator worker")
	go c.runWorker(ctx)
	<-ctx.Done()
	c.workqueue.ShutDown()
	klog.Info("Topology coordinator worker stopped")
	return nil
}

// SetPublishing enables or disables DeviceClass publication for this replica.
func (c *Controller) SetPublishing(enabled bool) {
	c.publishing.Store(enabled)
	if enabled {
		c.EnqueueReconcile()
	}
}

// EnqueueReconcile requests a full reconciliation.
func (c *Controller) EnqueueReconcile() {
	if c.synced.Load() {
		c.workqueue.Add("reconcile")
	}
}

// Run starts the controller as a publishing leader. It is retained for tests
// and single-replica callers; multi-replica deployments should start caches on
// every replica and toggle publication from leader-election callbacks.
func (c *Controller) Run(ctx context.Context) error {
	if err := c.StartCache(ctx); err != nil {
		return err
	}
	c.SetPublishing(true)
	return c.RunWorker(ctx)
}

// runWorker processes items from the workqueue.
func (c *Controller) runWorker(ctx context.Context) {
	for {
		key, shutdown := c.workqueue.Get()
		if shutdown {
			return
		}
		if !c.publishing.Load() {
			c.workqueue.Forget(key)
			c.workqueue.Done(key)
			continue
		}

		if err := c.reconcile(ctx); err != nil {
			klog.Errorf("Reconciliation failed: %v", err)
			c.workqueue.AddRateLimited(key)
		} else {
			c.workqueue.Forget(key)
		}
		c.workqueue.Done(key)
	}
}

// reconcile performs a full reconciliation: recompute partitions and sync DeviceClasses.
func (c *Controller) reconcile(ctx context.Context) error {
	klog.V(4).Info("Running reconciliation")
	start := time.Now()

	// Update rules in the model
	c.refreshModelRules()

	groupings := c.groupingStore.GetGroupings()

	skipPartitions := c.partitionMode == PartitionModeGroupings
	skipGroupings := c.partitionMode == PartitionModePartitions

	partitionCount := 0
	groupingCount := 0
	nodeCount := 0

	if !skipPartitions {
		results := c.partitionBuilder.BuildPartitions()

		if err := c.classManager.SyncDeviceClasses(ctx, results); err != nil {
			metrics.ReconciliationErrors.Inc()
			return fmt.Errorf("failed to sync DeviceClasses: %w", err)
		}

		nodeCount = len(results)
		metrics.NodesTotal.Set(float64(nodeCount))
		partitionCount = countDeviceClasses(results)
	}

	if !skipGroupings {
		duplicateNames := detectDuplicateGroupingNames(groupings)
		names := make([]string, 0, len(duplicateNames))
		for name := range duplicateNames {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			sources := duplicateNames[name]
			klog.ErrorS(fmt.Errorf("multiple ConfigMaps define grouping %q", name),
				"Duplicate user-defined device grouping", "field", "data.name",
				"groupingName", name, "configMaps", sources)
		}

		// Auto-detect PCIe root pairings (GPU+NIC, GPU+NVMe, etc.)
		autoGroupings := c.partitionBuilder.DetectPCIeRootPairings()

		// Merge: ConfigMap-defined groupings override auto-detected ones with the same name
		mergedGroupings := mergeGroupings(autoGroupings, groupings)
		collisions := detectSanitizedNameCollisions(mergedGroupings)
		if len(collisions) > 0 {
			keys := make([]string, 0, len(collisions))
			for key := range collisions {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			details := make([]string, 0, len(keys))
			for _, key := range keys {
				groupings := collisions[key]
				rawNames := make([]string, 0, len(groupings))
				sources := make([]string, 0, len(groupings))
				for _, grouping := range groupings {
					rawNames = append(rawNames, grouping.Name)
					if grouping.sourceConfigMap == "" {
						sources = append(sources, "auto-detected")
					} else {
						sources = append(sources, grouping.sourceConfigMap)
					}
				}
				sort.Strings(rawNames)
				sort.Strings(sources)
				detail := fmt.Sprintf("field=data.name sanitized=%q rawNames=%v configMaps=%v", key, rawNames, sources)
				klog.ErrorS(fmt.Errorf("sanitized grouping name collision: %s", detail),
					"Sanitized grouping name collision", "field", "data.name", "sanitizedName", key,
					"groupingNames", rawNames, "configMaps", sources)
				details = append(details, detail)
			}
			metrics.ReconciliationErrors.Inc()
			return fmt.Errorf("sanitized grouping name collision: %s", strings.Join(details, "; "))
		}

		// Always sync, even with zero groupings: SyncGroupingDeviceClasses still
		// runs cleanup, so deleting the last grouping ConfigMap removes its
		// DeviceClasses instead of leaving them orphaned indefinitely.
		groupingResults := c.groupingBuilder.BuildGroupings(mergedGroupings)
		deviceClassCollisions := detectGroupingDeviceClassNameCollisions(groupingResults)
		if len(deviceClassCollisions) > 0 {
			classNames := make([]string, 0, len(deviceClassCollisions))
			for className := range deviceClassCollisions {
				classNames = append(classNames, className)
			}
			sort.Strings(classNames)

			sourcesByGrouping := make(map[string]string, len(mergedGroupings))
			for _, grouping := range mergedGroupings {
				source := grouping.sourceConfigMap
				if source == "" {
					source = "auto-detected"
				}
				sourcesByGrouping[grouping.Name] = source
			}

			details := make([]string, 0, len(classNames))
			for _, className := range classNames {
				groupingNames := deviceClassCollisions[className]
				sources := make([]string, 0, len(groupingNames))
				for _, groupingName := range groupingNames {
					sources = append(sources, sourcesByGrouping[groupingName])
				}
				sort.Strings(sources)
				detail := fmt.Sprintf("DeviceClass %q would be emitted by data.name values %v from ConfigMaps %v", className, groupingNames, sources)
				klog.ErrorS(fmt.Errorf("generated grouping DeviceClass name collision: %s", detail),
					"Generated grouping DeviceClass name collision", "field", "data.name",
					"deviceClassName", className, "groupingNames", groupingNames, "configMaps", sources)
				details = append(details, detail)
			}
			metrics.ReconciliationErrors.Inc()
			return fmt.Errorf("generated grouping DeviceClass name collision: %s", strings.Join(details, "; "))
		}

		if err := c.classManager.SyncGroupingDeviceClasses(ctx, groupingResults); err != nil {
			metrics.ReconciliationErrors.Inc()
			return fmt.Errorf("failed to sync grouping DeviceClasses: %w", err)
		}

		groupingCount = countGroupingDeviceClasses(groupingResults)
	}

	metrics.ReconciliationDuration.Observe(time.Since(start).Seconds())
	metrics.DeviceClassesTotal.Set(float64(partitionCount + groupingCount))

	klog.Infof("Reconciliation complete (partitions): %d nodes, %d DeviceClasses",
		nodeCount, partitionCount)
	if groupingCount > 0 {
		klog.Infof("Reconciliation complete (groupings): %d grouping DeviceClasses", groupingCount)
	}

	return nil
}

func (c *Controller) refreshModelRules() {
	rules := c.ruleStore.GetRules()
	c.model.SetRules(rules)
	metrics.TopologyRulesTotal.Set(float64(len(rules)))
}

// onSliceAdd handles a new ResourceSlice.
func (c *Controller) onSliceAdd(obj interface{}) {
	slice, ok := obj.(*resourcev1.ResourceSlice)
	if !ok {
		return
	}
	c.model.UpdateFromResourceSlice(slice)
	if c.synced.Load() {
		c.workqueue.AddAfter("reconcile", reconcileDebounceDelay)
	}
}

// onSliceUpdate handles a ResourceSlice update.
func (c *Controller) onSliceUpdate(_, newObj interface{}) {
	slice, ok := newObj.(*resourcev1.ResourceSlice)
	if !ok {
		return
	}
	c.model.UpdateFromResourceSlice(slice)
	if c.synced.Load() {
		c.workqueue.AddAfter("reconcile", reconcileDebounceDelay)
	}
}

// onSliceDelete handles a ResourceSlice deletion.
func (c *Controller) onSliceDelete(obj interface{}) {
	slice, ok := obj.(*resourcev1.ResourceSlice)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		slice, ok = tombstone.Obj.(*resourcev1.ResourceSlice)
		if !ok {
			return
		}
	}
	c.model.RemoveResourceSlice(slice)
	c.workqueue.AddAfter("reconcile", reconcileDebounceDelay)
}

// onConfigMapAdd handles a new ConfigMap.
func (c *Controller) onConfigMapAdd(obj interface{}) {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok {
		return
	}
	if cm.Labels[TopologyRuleLabel] != "true" {
		c.ruleStore.RemoveConfigMap(cm.Namespace, cm.Name)
		c.refreshModelRules()
		return
	}
	if err := c.ruleStore.LoadFromConfigMap(cm); err != nil {
		klog.Errorf("Failed to load topology rule from %s/%s: %v", cm.Namespace, cm.Name, err)
		return
	}
	c.refreshModelRules()
	c.workqueue.AddAfter("reconcile", reconcileDebounceDelay)
}

// onConfigMapUpdate handles a ConfigMap update.
func (c *Controller) onConfigMapUpdate(_, newObj interface{}) {
	cm, ok := newObj.(*corev1.ConfigMap)
	if !ok {
		return
	}
	if cm.Labels[TopologyRuleLabel] != "true" {
		c.ruleStore.RemoveConfigMap(cm.Namespace, cm.Name)
		c.refreshModelRules()
		return
	}
	if err := c.ruleStore.LoadFromConfigMap(cm); err != nil {
		klog.Errorf("Failed to update topology rule from %s/%s: %v", cm.Namespace, cm.Name, err)
		return
	}
	c.refreshModelRules()
	c.workqueue.AddAfter("reconcile", reconcileDebounceDelay)
}

// onConfigMapDelete handles a ConfigMap deletion.
func (c *Controller) onConfigMapDelete(obj interface{}) {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		cm, ok = tombstone.Obj.(*corev1.ConfigMap)
		if !ok {
			return
		}
	}
	c.ruleStore.RemoveConfigMap(cm.Namespace, cm.Name)
	c.refreshModelRules()
	c.workqueue.AddAfter("reconcile", reconcileDebounceDelay)
}

// onGroupingConfigMapAdd handles a new grouping ConfigMap.
func (c *Controller) onGroupingConfigMapAdd(obj interface{}) {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok {
		return
	}
	if cm.Labels[DeviceGroupingLabel] != "true" {
		return
	}
	if err := c.groupingStore.LoadFromConfigMap(cm); err != nil {
		klog.Errorf("Failed to load device grouping from %s/%s: %v", cm.Namespace, cm.Name, err)
		return
	}
	if c.synced.Load() {
		c.workqueue.AddAfter("reconcile", reconcileDebounceDelay)
	}
}

// onGroupingConfigMapUpdate handles a grouping ConfigMap update.
func (c *Controller) onGroupingConfigMapUpdate(_, newObj interface{}) {
	cm, ok := newObj.(*corev1.ConfigMap)
	if !ok {
		return
	}
	if cm.Labels[DeviceGroupingLabel] != "true" {
		return
	}
	if err := c.groupingStore.LoadFromConfigMap(cm); err != nil {
		klog.Errorf("Failed to update device grouping from %s/%s; retaining stale cached grouping: %v",
			cm.Namespace, cm.Name, err)
		return
	}
	if c.synced.Load() {
		c.workqueue.AddAfter("reconcile", reconcileDebounceDelay)
	}
}

// onGroupingConfigMapDelete handles a grouping ConfigMap deletion.
func (c *Controller) onGroupingConfigMapDelete(obj interface{}) {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		cm, ok = tombstone.Obj.(*corev1.ConfigMap)
		if !ok {
			return
		}
	}
	if cm.Labels[DeviceGroupingLabel] != "true" {
		return
	}
	c.groupingStore.RemoveConfigMap(cm.Namespace, cm.Name)
	c.workqueue.AddAfter("reconcile", reconcileDebounceDelay)
}

// mergeGroupings combines auto-detected and user-defined groupings.
// User-defined groupings (from ConfigMaps) override auto-detected ones
// with the same name.
func mergeGroupings(autoDetected, userDefined []DeviceGrouping) []DeviceGrouping {
	byName := make(map[string]DeviceGrouping)
	for _, g := range autoDetected {
		byName[g.Name] = g
	}
	for _, g := range userDefined {
		byName[g.Name] = g
	}
	merged := make([]DeviceGrouping, 0, len(byName))
	for _, g := range byName {
		merged = append(merged, g)
	}
	return merged
}

func detectDuplicateGroupingNames(userDefined []DeviceGrouping) map[string][]string {
	sourcesByName := make(map[string][]string)
	for _, grouping := range userDefined {
		source := grouping.sourceConfigMap
		if source == "" {
			source = "unknown source"
		}
		sourcesByName[grouping.Name] = append(sourcesByName[grouping.Name], source)
	}

	duplicates := make(map[string][]string)
	for name, sources := range sourcesByName {
		if len(sources) > 1 {
			sort.Strings(sources)
			duplicates[name] = sources
		}
	}
	return duplicates
}

func detectSanitizedNameCollisions(groupings []DeviceGrouping) map[string][]DeviceGrouping {
	bySanitizedName := make(map[string]map[string]DeviceGrouping)
	for _, grouping := range groupings {
		sanitizedName := sanitizeForName(grouping.Name)
		if bySanitizedName[sanitizedName] == nil {
			bySanitizedName[sanitizedName] = make(map[string]DeviceGrouping)
		}
		bySanitizedName[sanitizedName][grouping.Name] = grouping
	}

	collisions := make(map[string][]DeviceGrouping)
	for sanitizedName, byRawName := range bySanitizedName {
		if len(byRawName) < 2 {
			continue
		}
		groupings := make([]DeviceGrouping, 0, len(byRawName))
		for _, grouping := range byRawName {
			groupings = append(groupings, grouping)
		}
		sort.Slice(groupings, func(i, j int) bool {
			return groupings[i].Name < groupings[j].Name
		})
		collisions[sanitizedName] = groupings
	}
	return collisions
}

func detectGroupingDeviceClassNameCollisions(results []GroupingResult) map[string][]string {
	seen := make(map[string]bool)
	var representatives []GroupingInstance
	for _, result := range results {
		for _, instance := range result.Instances {
			key := groupingInstanceKey(instance)
			if seen[key] {
				continue
			}
			seen[key] = true
			representatives = append(representatives, instance)
		}
	}

	ownersByName := make(map[string]map[string]struct{})
	for _, instance := range representatives {
		groupingName := instance.GroupingName
		classNames := []string{
			sanitizeForName(groupingName),
			fmt.Sprintf("%s-rail%d", sanitizeForName(groupingName), instance.RailIndex),
		}
		for _, className := range classNames {
			if ownersByName[className] == nil {
				ownersByName[className] = make(map[string]struct{})
			}
			ownersByName[className][groupingName] = struct{}{}
		}
	}

	collisions := make(map[string][]string)
	for className, owners := range ownersByName {
		if len(owners) < 2 {
			continue
		}
		groupingNames := make([]string, 0, len(owners))
		for groupingName := range owners {
			groupingNames = append(groupingNames, groupingName)
		}
		sort.Strings(groupingNames)
		collisions[className] = groupingNames
	}
	return collisions
}

func countGroupingDeviceClasses(results []GroupingResult) int {
	seen := make(map[string]bool)
	for _, r := range results {
		for _, inst := range r.Instances {
			seen[inst.GroupingName+"-"+inst.Alignment] = true
		}
	}
	return len(seen)
}

func countDeviceClasses(results []PartitionResult) int {
	seen := make(map[string]bool)
	for _, r := range results {
		for _, p := range r.Partitions {
			seen[r.Profile+"-"+string(p.Type)] = true
		}
	}
	return len(seen)
}
