package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"runtime/debug"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const (
	ReplicateToAnnotation     = "replicator.kuoss.github.io/replicate-to"
	ManagedLabel              = "replicator.kuoss.github.io/managed"
	SourceNamespaceAnnotation = "replicator.kuoss.github.io/source-namespace"
	SourceHashAnnotation      = "replicator.kuoss.github.io/source-hash"
	FieldManager              = "uni-replicator"
	sourceIndexName           = "uni-replicator-source"
	targetIndexName           = "uni-replicator-target"
	maxRetries                = 10
)

type Resource struct {
	GVR                   schema.GroupVersionResource
	GVK                   schema.GroupVersionKind
	CascadeDeletionPolicy CascadeDeletionPolicy
	ExistingTargetPolicy  ExistingTargetPolicy
}

type CascadeDeletionPolicy string

const (
	CascadeDeletionDelete CascadeDeletionPolicy = "Delete"
	CascadeDeletionRetain CascadeDeletionPolicy = "Retain"
)

type ExistingTargetPolicy string

const (
	ExistingTargetPreserve  ExistingTargetPolicy = "Preserve"
	ExistingTargetOverwrite ExistingTargetPolicy = "Overwrite"
)

type key struct {
	GVR       schema.GroupVersionResource
	Namespace string
	Name      string
}

type watchedResource struct {
	Resource
	informer cache.SharedIndexInformer
}

type Controller struct {
	client    dynamic.Interface
	factory   dynamicinformer.DynamicSharedInformerFactory
	resources map[schema.GroupVersionResource]*watchedResource
	queue     workqueue.RateLimitingInterface
}

func New(client dynamic.Interface, resources []Resource, resync time.Duration) (*Controller, error) {
	if client == nil {
		return nil, fmt.Errorf("dynamic client is required")
	}
	if len(resources) == 0 {
		return nil, fmt.Errorf("at least one resource is required")
	}

	factory := dynamicinformer.NewDynamicSharedInformerFactory(client, resync)
	c := &Controller{
		client:    client,
		factory:   factory,
		resources: make(map[schema.GroupVersionResource]*watchedResource, len(resources)),
		queue:     workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "uni-replicator"),
	}

	for _, resource := range resources {
		if resource.GVR.Empty() || resource.GVK.Empty() {
			return nil, fmt.Errorf("resource must have non-empty GVR and GVK")
		}
		if resource.CascadeDeletionPolicy == "" {
			resource.CascadeDeletionPolicy = CascadeDeletionDelete
		}
		if resource.CascadeDeletionPolicy != CascadeDeletionDelete && resource.CascadeDeletionPolicy != CascadeDeletionRetain {
			return nil, fmt.Errorf("resource %s has invalid cascade deletion policy %q", resource.GVR, resource.CascadeDeletionPolicy)
		}
		if resource.ExistingTargetPolicy == "" {
			resource.ExistingTargetPolicy = ExistingTargetPreserve
		}
		if resource.ExistingTargetPolicy != ExistingTargetPreserve && resource.ExistingTargetPolicy != ExistingTargetOverwrite {
			return nil, fmt.Errorf("resource %s has invalid existing target policy %q", resource.GVR, resource.ExistingTargetPolicy)
		}
		if _, exists := c.resources[resource.GVR]; exists {
			return nil, fmt.Errorf("resource %s is configured more than once", resource.GVR)
		}
		informer := factory.ForResource(resource.GVR).Informer()
		if err := informer.SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
			slog.Error("resource watch failed; retrying", "error", err, "resource", resource.GVR)
		}); err != nil {
			return nil, fmt.Errorf("set watch error handler for %s: %w", resource.GVR, err)
		}
		if err := informer.AddIndexers(cache.Indexers{
			sourceIndexName: sourceIndex,
			targetIndexName: targetIndex,
		}); err != nil {
			return nil, fmt.Errorf("add informer indexes for %s: %w", resource.GVR, err)
		}
		watched := &watchedResource{Resource: resource, informer: informer}
		c.resources[resource.GVR] = watched
		if _, err := informer.AddEventHandler(c.eventHandler(resource.GVR)); err != nil {
			return nil, fmt.Errorf("add event handler for %s: %w", resource.GVR, err)
		}
	}
	return c, nil
}

func (c *Controller) Run(ctx context.Context, workers int) error {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.ErrorContext(ctx, "controller panic", "panic", recovered, "stack", string(debug.Stack()))
		}
	}()
	defer c.queue.ShutDown()

	c.factory.Start(ctx.Done())
	for gvr, resource := range c.resources {
		if !cache.WaitForCacheSync(ctx.Done(), resource.informer.HasSynced) {
			return fmt.Errorf("cache sync failed for %s", gvr)
		}
	}

	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}
	<-ctx.Done()
	return nil
}

func (c *Controller) runWorker(ctx context.Context) {
	for c.processNext(ctx) {
	}
}

func (c *Controller) processNext(ctx context.Context) bool {
	item, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(item)

	k, ok := item.(key)
	if !ok {
		c.queue.Forget(item)
		slog.ErrorContext(ctx, "unexpected queue item", "type", fmt.Sprintf("%T", item))
		return true
	}
	if err := c.reconcile(ctx, k); err != nil {
		if c.queue.NumRequeues(item) < maxRetries {
			slog.ErrorContext(ctx, "reconciliation failed; retrying", "error", err, "resource", k.GVR, "namespace", k.Namespace, "name", k.Name)
			c.queue.AddRateLimited(item)
			return true
		}
		c.queue.Forget(item)
		slog.ErrorContext(ctx, "giving up reconciliation", "error", err, "resource", k.GVR, "namespace", k.Namespace, "name", k.Name, "retries", maxRetries)
		return true
	}

	c.queue.Forget(item)
	return true
}

func (c *Controller) eventHandler(gvr schema.GroupVersionResource) cache.ResourceEventHandler {
	enqueue := func(value interface{}, deleted bool) {
		obj, err := objectFromEvent(value)
		if err != nil {
			slog.Error("could not handle resource event", "error", err, "resource", gvr)
			return
		}
		c.enqueueObject(gvr, obj)
		if deleted && !isManaged(obj) {
			c.enqueueSourcesForTarget(gvr, obj.GetNamespace(), obj.GetName())
		}
	}
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(value interface{}) {
			enqueue(value, false)
		},
		UpdateFunc: func(oldValue, newValue interface{}) {
			enqueue(oldValue, false)
			enqueue(newValue, false)
		},
		DeleteFunc: func(value interface{}) {
			enqueue(value, true)
		},
	}
}

func (c *Controller) enqueueObject(gvr schema.GroupVersionResource, obj *unstructured.Unstructured) {
	if isManaged(obj) {
		annotations := obj.GetAnnotations()
		namespace := annotations[SourceNamespaceAnnotation]
		if namespace == "" {
			slog.Error("managed object has no source identity", "resource", gvr, "namespace", obj.GetNamespace(), "name", obj.GetName())
			return
		}
		c.queue.Add(key{GVR: gvr, Namespace: namespace, Name: obj.GetName()})
		return
	}
	c.queue.Add(key{GVR: gvr, Namespace: obj.GetNamespace(), Name: obj.GetName()})
}

func (c *Controller) enqueueSourcesForTarget(gvr schema.GroupVersionResource, namespace, name string) {
	resource, ok := c.resources[gvr]
	if !ok {
		slog.Error("could not find sources for unconfigured target", "resource", gvr, "namespace", namespace, "name", name)
		return
	}
	values, err := resource.informer.GetIndexer().ByIndex(targetIndexName, sourceIdentity(namespace, name))
	if err != nil {
		slog.Error("could not find sources for target", "error", err, "resource", gvr, "namespace", namespace, "name", name)
		return
	}
	for _, value := range values {
		source, ok := value.(*unstructured.Unstructured)
		if !ok || isManaged(source) {
			continue
		}
		c.queue.Add(key{GVR: gvr, Namespace: source.GetNamespace(), Name: source.GetName()})
	}
}

func objectFromEvent(value interface{}) (*unstructured.Unstructured, error) {
	if tombstone, ok := value.(cache.DeletedFinalStateUnknown); ok {
		value = tombstone.Obj
	}
	obj, ok := value.(*unstructured.Unstructured)
	if !ok {
		return nil, fmt.Errorf("expected unstructured object, got %T", value)
	}
	return obj, nil
}

func (c *Controller) reconcile(ctx context.Context, k key) error {
	resource, ok := c.resources[k.GVR]
	if !ok {
		return fmt.Errorf("resource %s is not configured", k.GVR)
	}
	value, exists, err := resource.informer.GetIndexer().GetByKey(k.Namespace + "/" + k.Name)
	if err != nil {
		return fmt.Errorf("read source from cache: %w", err)
	}
	if !exists {
		if resource.CascadeDeletionPolicy == CascadeDeletionRetain {
			return c.retainReplicas(ctx, resource, k)
		}
		return c.deleteReplicas(ctx, resource, k, nil)
	}
	source, ok := value.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("cached source has unexpected type %T", value)
	}
	if isManaged(source) {
		return nil
	}

	targets := parseTargets(source.GetAnnotations()[ReplicateToAnnotation], source.GetNamespace())
	var reconcileErrors []error
	if err := c.deleteReplicas(ctx, resource, k, targets); err != nil {
		reconcileErrors = append(reconcileErrors, err)
	}
	for namespace := range targets {
		if err := c.applyReplica(ctx, resource, source, namespace); err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("sync replica %s/%s: %w", namespace, source.GetName(), err))
		}
	}
	return errors.Join(reconcileErrors...)
}

func (c *Controller) deleteReplicas(ctx context.Context, resource *watchedResource, source key, keep map[string]struct{}) error {
	values, err := resource.informer.GetIndexer().ByIndex(sourceIndexName, sourceIdentity(source.Namespace, source.Name))
	if err != nil {
		return fmt.Errorf("list replicas from cache: %w", err)
	}
	var deleteErrors []error
	for _, value := range values {
		replica, ok := value.(*unstructured.Unstructured)
		if !ok || !isManaged(replica) {
			continue
		}
		if _, wanted := keep[replica.GetNamespace()]; wanted {
			continue
		}
		uid := replica.GetUID()
		options := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}
		err := c.client.Resource(resource.GVR).Namespace(replica.GetNamespace()).Delete(ctx, replica.GetName(), options)
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			deleteErrors = append(deleteErrors, fmt.Errorf("delete replica %s/%s: %w", replica.GetNamespace(), replica.GetName(), err))
			continue
		}
		if err == nil {
			slog.InfoContext(ctx, "deleted replica", "resource", resource.GVR, "namespace", replica.GetNamespace(), "name", replica.GetName())
		}
	}
	return errors.Join(deleteErrors...)
}

func (c *Controller) retainReplicas(ctx context.Context, resource *watchedResource, source key) error {
	values, err := resource.informer.GetIndexer().ByIndex(sourceIndexName, sourceIdentity(source.Namespace, source.Name))
	if err != nil {
		return fmt.Errorf("list replicas from cache: %w", err)
	}
	var retainErrors []error
	for _, value := range values {
		replica, ok := value.(*unstructured.Unstructured)
		if !ok || !isManaged(replica) {
			continue
		}
		metadata := map[string]interface{}{
			"labels": map[string]interface{}{
				ManagedLabel: nil,
			},
			"annotations": map[string]interface{}{
				ReplicateToAnnotation:     nil,
				SourceNamespaceAnnotation: nil,
				SourceHashAnnotation:      nil,
			},
		}
		if resourceVersion := replica.GetResourceVersion(); resourceVersion != "" {
			metadata["resourceVersion"] = resourceVersion
		}
		patch, err := json.Marshal(map[string]interface{}{"metadata": metadata})
		if err != nil {
			retainErrors = append(retainErrors, fmt.Errorf("marshal retain patch for %s/%s: %w", replica.GetNamespace(), replica.GetName(), err))
			continue
		}
		_, err = c.client.Resource(resource.GVR).Namespace(replica.GetNamespace()).Patch(ctx, replica.GetName(), types.MergePatchType, patch, metav1.PatchOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			retainErrors = append(retainErrors, fmt.Errorf("retain replica %s/%s: %w", replica.GetNamespace(), replica.GetName(), err))
			continue
		}
		if err == nil {
			slog.InfoContext(ctx, "retained replica", "resource", resource.GVR, "namespace", replica.GetNamespace(), "name", replica.GetName())
		}
	}
	return errors.Join(retainErrors...)
}

func (c *Controller) applyReplica(ctx context.Context, resource *watchedResource, source *unstructured.Unstructured, namespace string) error {
	client := c.client.Resource(resource.GVR).Namespace(namespace)
	desired, err := buildReplica(source, resource.GVK, namespace)
	if err != nil {
		return err
	}
	existing, err := client.Get(ctx, source.GetName(), metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get target object: %w", err)
	}
	if err == nil {
		if !isManaged(existing) && resource.ExistingTargetPolicy == ExistingTargetPreserve {
			slog.ErrorContext(ctx, "target object exists and is not managed; refusing takeover", "resource", resource.GVR, "namespace", namespace, "name", source.GetName())
			return nil
		}
		annotations := existing.GetAnnotations()
		if isManaged(existing) && annotations[SourceNamespaceAnnotation] != source.GetNamespace() && resource.ExistingTargetPolicy == ExistingTargetPreserve {
			slog.ErrorContext(ctx, "target object is managed by a different source; refusing takeover", "resource", resource.GVR, "namespace", namespace, "name", source.GetName())
			return nil
		}
		if annotations[SourceHashAnnotation] == desired.GetAnnotations()[SourceHashAnnotation] && objectContains(existing.Object, desired.Object) {
			return nil
		}
	}

	data, err := desired.MarshalJSON()
	if err != nil {
		return fmt.Errorf("marshal apply object: %w", err)
	}
	force := resource.ExistingTargetPolicy == ExistingTargetOverwrite
	_, err = client.Patch(ctx, source.GetName(), types.ApplyPatchType, data, metav1.PatchOptions{FieldManager: FieldManager, Force: &force})
	if err != nil {
		return fmt.Errorf("server-side apply: %w", err)
	}
	slog.InfoContext(ctx, "synchronized replica", "resource", resource.GVR, "sourceNamespace", source.GetNamespace(), "namespace", namespace, "name", source.GetName())
	return nil
}

func buildReplica(source *unstructured.Unstructured, gvk schema.GroupVersionKind, namespace string) (*unstructured.Unstructured, error) {
	replica := source.DeepCopy()
	replica.SetGroupVersionKind(gvk)
	replica.SetNamespace(namespace)

	for _, field := range []string{
		"uid", "resourceVersion", "generation", "creationTimestamp", "deletionTimestamp",
		"deletionGracePeriodSeconds", "managedFields", "ownerReferences", "finalizers", "selfLink",
	} {
		unstructured.RemoveNestedField(replica.Object, "metadata", field)
	}
	unstructured.RemoveNestedField(replica.Object, "status")

	labels := copyStringMap(replica.GetLabels())
	labels[ManagedLabel] = "true"
	replica.SetLabels(labels)
	annotations := copyStringMap(replica.GetAnnotations())
	annotations[SourceNamespaceAnnotation] = source.GetNamespace()
	replica.SetAnnotations(annotations)
	replicaAnnotations := replica.GetAnnotations()
	delete(replicaAnnotations, SourceHashAnnotation)
	replica.SetAnnotations(replicaAnnotations)
	hash, err := objectHash(replica)
	if err != nil {
		return nil, err
	}
	replicaAnnotations[SourceHashAnnotation] = hash
	replica.SetAnnotations(replicaAnnotations)
	return replica, nil
}

func objectHash(object *unstructured.Unstructured) (string, error) {
	data, err := object.MarshalJSON()
	if err != nil {
		return "", fmt.Errorf("marshal object for source hash: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func objectContains(actual, desired interface{}) bool {
	switch desiredValue := desired.(type) {
	case map[string]interface{}:
		actualValue, ok := actual.(map[string]interface{})
		if !ok {
			return false
		}
		for key, desiredField := range desiredValue {
			actualField, exists := actualValue[key]
			if !exists || !objectContains(actualField, desiredField) {
				return false
			}
		}
		return true
	case []interface{}:
		actualValue, ok := actual.([]interface{})
		if !ok || len(actualValue) != len(desiredValue) {
			return false
		}
		for i := range desiredValue {
			if !objectContains(actualValue[i], desiredValue[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(actual, desired)
	}
}

func parseTargets(value, sourceNamespace string) map[string]struct{} {
	result := make(map[string]struct{})
	for _, item := range strings.Split(value, ",") {
		namespace := strings.TrimSpace(item)
		if namespace != "" && namespace != sourceNamespace {
			result[namespace] = struct{}{}
		}
	}
	return result
}

func isManaged(obj metav1.Object) bool {
	return obj.GetLabels()[ManagedLabel] == "true"
}

func sourceIndex(value interface{}) ([]string, error) {
	obj, ok := value.(*unstructured.Unstructured)
	if !ok {
		return nil, nil
	}
	if !isManaged(obj) {
		return nil, nil
	}
	annotations := obj.GetAnnotations()
	namespace := annotations[SourceNamespaceAnnotation]
	if namespace == "" {
		return nil, nil
	}
	return []string{sourceIdentity(namespace, obj.GetName())}, nil
}

func targetIndex(value interface{}) ([]string, error) {
	obj, ok := value.(*unstructured.Unstructured)
	if !ok || isManaged(obj) {
		return nil, nil
	}
	targets := parseTargets(obj.GetAnnotations()[ReplicateToAnnotation], obj.GetNamespace())
	result := make([]string, 0, len(targets))
	for namespace := range targets {
		result = append(result, sourceIdentity(namespace, obj.GetName()))
	}
	return result, nil
}

func sourceIdentity(namespace, name string) string {
	return namespace + "\x00" + name
}

func copyStringMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source)+2)
	for key, value := range source {
		result[key] = value
	}
	return result
}
