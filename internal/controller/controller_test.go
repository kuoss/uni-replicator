package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

var (
	testGVR = schema.GroupVersionResource{Group: "example.io", Version: "v1", Resource: "widgets"}
	testGVK = schema.GroupVersionKind{Group: "example.io", Version: "v1", Kind: "Widget"}
)

func TestControllerReplicatesArbitraryCRDAndMaintainsLifecycle(t *testing.T) {
	source := widget("platform", "shared", "one")
	source.SetUID(types.UID("source-123"))
	source.SetAnnotations(map[string]string{ReplicateToAnnotation: "app-a, app-b, platform, app-a"})
	source.Object["status"] = map[string]interface{}{"state": "Ready"}
	if err := unstructured.SetNestedField(source.Object, "remove-me", "spec", "obsolete"); err != nil {
		t.Fatal(err)
	}
	source.SetFinalizers([]string{"example.io/protect"})
	unmanaged := widget("app-b", "shared", "do-not-touch")

	client := newTestClient(source, unmanaged)
	c, err := New(client, []Resource{{GVR: testGVR, GVK: testGVK}}, 0)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx, 2) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run() error = %v", err)
		}
	})

	replica := eventuallyGet(t, client, "app-a", "shared")
	if !isManaged(replica) || replica.GetAnnotations()[SourceNamespaceAnnotation] != "platform" {
		t.Fatalf("replica has incorrect management metadata: labels=%v annotations=%v", replica.GetLabels(), replica.GetAnnotations())
	}
	if _, found := replica.Object["status"]; found {
		t.Fatal("replica retained status")
	}
	if len(replica.GetFinalizers()) != 0 || replica.GetUID() != "" || replica.GetResourceVersion() != "" {
		t.Fatalf("replica retained lifecycle metadata: %#v", replica.Object["metadata"])
	}
	if got := nestedValue(t, replica); got != "one" {
		t.Fatalf("replica value = %q, want one", got)
	}
	if got := nestedValue(t, eventuallyGet(t, client, "app-b", "shared")); got != "do-not-touch" {
		t.Fatalf("unmanaged destination was overwritten: %q", got)
	}

	assertServerSideApply(t, client)

	updated := eventuallyGet(t, client, "platform", "shared").DeepCopy()
	if err := unstructured.SetNestedField(updated.Object, "two", "spec", "value"); err != nil {
		t.Fatal(err)
	}
	unstructured.RemoveNestedField(updated.Object, "spec", "obsolete")
	updated.SetAnnotations(map[string]string{ReplicateToAnnotation: "app-a"})
	if _, err := client.Resource(testGVR).Namespace("platform").Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update source: %v", err)
	}
	eventually(t, func() bool {
		obj, err := client.Resource(testGVR).Namespace("app-a").Get(ctx, "shared", metav1.GetOptions{})
		if err != nil {
			return false
		}
		_, obsolete, nestedErr := unstructured.NestedString(obj.Object, "spec", "obsolete")
		return nestedErr == nil && !obsolete && nestedValue(t, obj) == "two"
	}, "updated replica")

	if err := client.Resource(testGVR).Namespace("app-a").Delete(ctx, "shared", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete replica: %v", err)
	}
	eventually(t, func() bool {
		obj, err := client.Resource(testGVR).Namespace("app-a").Get(ctx, "shared", metav1.GetOptions{})
		return err == nil && nestedValue(t, obj) == "two"
	}, "deleted replica to be recreated")

	updated = eventuallyGet(t, client, "platform", "shared").DeepCopy()
	updated.SetAnnotations(map[string]string{})
	if _, err := client.Resource(testGVR).Namespace("platform").Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("remove destination: %v", err)
	}
	eventuallyNotFound(t, client, "app-a", "shared")

	updated = eventuallyGet(t, client, "platform", "shared").DeepCopy()
	updated.SetAnnotations(map[string]string{ReplicateToAnnotation: "app-a"})
	if _, err := client.Resource(testGVR).Namespace("platform").Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("restore destination: %v", err)
	}
	eventuallyGet(t, client, "app-a", "shared")
	if err := client.Resource(testGVR).Namespace("platform").Delete(ctx, "shared", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete source: %v", err)
	}
	eventuallyNotFound(t, client, "app-a", "shared")
}

func TestBlockingDestinationDeletionImmediatelyReconcilesSource(t *testing.T) {
	ctx := context.Background()
	source := widget("platform", "shared", "source-value")
	source.SetAnnotations(map[string]string{ReplicateToAnnotation: "app-b"})
	blocker := widget("app-b", "shared", "unmanaged-value")
	client := newTestClient(blocker)
	c, err := New(client, []Resource{{GVR: testGVR, GVK: testGVK}}, 0)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer c.queue.ShutDown()
	if err := c.resources[testGVR].informer.GetIndexer().Add(source); err != nil {
		t.Fatalf("add source to informer index: %v", err)
	}

	if err := c.reconcile(ctx, key{GVR: testGVR, Namespace: source.GetNamespace(), Name: source.GetName()}); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	if got := nestedValue(t, eventuallyGet(t, client, "app-b", "shared")); got != "unmanaged-value" {
		t.Fatalf("blocking destination was overwritten: %q", got)
	}

	if err := client.Resource(testGVR).Namespace("app-b").Delete(ctx, "shared", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete blocking destination: %v", err)
	}
	c.eventHandler(testGVR).OnDelete(blocker)
	if got := c.queue.Len(); got != 2 {
		t.Fatalf("queued items after blocker deletion = %d, want blocker and source", got)
	}
	if !c.processNext(ctx) || !c.processNext(ctx) {
		t.Fatal("worker stopped while processing blocker deletion")
	}

	replica := eventuallyGet(t, client, "app-b", "shared")
	if got := nestedValue(t, replica); got != "source-value" {
		t.Fatalf("replica value = %q, want source value", got)
	}
}

func TestRetainPolicyOrphansReplicasWhenSourceIsDeleted(t *testing.T) {
	source := widget("platform", "shared", "source-value")
	source.SetAnnotations(map[string]string{ReplicateToAnnotation: "app-a"})
	client := newTestClient(source)
	c, err := New(client, []Resource{{
		GVR:                   testGVR,
		GVK:                   testGVK,
		CascadeDeletionPolicy: CascadeDeletionRetain,
	}}, 0)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx, 1) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run() error = %v", err)
		}
	})

	eventuallyGet(t, client, "app-a", "shared")
	if err := client.Resource(testGVR).Namespace("platform").Delete(ctx, "shared", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete source: %v", err)
	}
	eventually(t, func() bool {
		replica, err := client.Resource(testGVR).Namespace("app-a").Get(ctx, "shared", metav1.GetOptions{})
		if err != nil || isManaged(replica) {
			return false
		}
		annotations := replica.GetAnnotations()
		return annotations[ReplicateToAnnotation] == "" &&
			annotations[SourceNamespaceAnnotation] == "" &&
			annotations[SourceHashAnnotation] == "" &&
			nestedValue(t, replica) == "source-value"
	}, "replica to be retained as an unmanaged object")
}

func TestBuildReplicaStripsLifecycleFields(t *testing.T) {
	source := widget("source", "sample", "value")
	source.SetUID("uid")
	source.SetResourceVersion("42")
	source.SetGeneration(3)
	source.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "test"}})
	source.SetOwnerReferences([]metav1.OwnerReference{{Name: "owner"}})
	source.SetFinalizers([]string{"finalizer"})
	source.Object["status"] = map[string]interface{}{"ok": true}

	replica, err := buildReplica(source, testGVK, "destination")
	if err != nil {
		t.Fatalf("buildReplica() error = %v", err)
	}
	metadata := replica.Object["metadata"].(map[string]interface{})
	for _, field := range []string{"uid", "resourceVersion", "generation", "managedFields", "ownerReferences", "finalizers"} {
		if _, exists := metadata[field]; exists {
			t.Errorf("metadata.%s was not removed", field)
		}
	}
	if _, exists := replica.Object["status"]; exists {
		t.Error("status was not removed")
	}
	annotations := replica.GetAnnotations()
	if annotations[SourceNamespaceAnnotation] != "source" || annotations[SourceHashAnnotation] == "" {
		t.Errorf("replica management annotations = %v", annotations)
	}
}

func newTestClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{testGVR: "WidgetList"}, objects...)
	// The generic fake tracker's SSA implementation only updates existing objects.
	// Emulate apiserver create-or-update apply semantics while retaining watch events.
	client.PrependReactor("patch", "widgets", func(action clienttesting.Action) (bool, runtime.Object, error) {
		patch := action.(clienttesting.PatchAction)
		if patch.GetPatchType() != types.ApplyPatchType {
			return false, nil, nil
		}
		obj := &unstructured.Unstructured{}
		if err := json.Unmarshal(patch.GetPatch(), &obj.Object); err != nil {
			return true, nil, err
		}
		namespace := action.GetNamespace()
		_, err := client.Tracker().Get(action.GetResource(), namespace, patch.GetName(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			err = client.Tracker().Create(action.GetResource(), obj, namespace)
		} else if err == nil {
			err = client.Tracker().Update(action.GetResource(), obj, namespace)
		}
		if err != nil {
			return true, nil, err
		}
		result, err := client.Tracker().Get(action.GetResource(), namespace, patch.GetName(), metav1.GetOptions{})
		return true, result, err
	})
	return client
}

func widget(namespace, name, value string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": testGVK.GroupVersion().String(),
		"kind":       testGVK.Kind,
		"metadata": map[string]interface{}{
			"namespace": namespace,
			"name":      name,
		},
		"spec": map[string]interface{}{"value": value},
	}}
}

func nestedValue(t *testing.T, obj *unstructured.Unstructured) string {
	t.Helper()
	value, found, err := unstructured.NestedString(obj.Object, "spec", "value")
	if err != nil || !found {
		t.Fatalf("read spec.value: found=%v err=%v object=%#v", found, err, obj.Object)
	}
	return value
}

func eventuallyGet(t *testing.T, client dynamic.Interface, namespace, name string) *unstructured.Unstructured {
	t.Helper()
	var result *unstructured.Unstructured
	eventually(t, func() bool {
		obj, err := client.Resource(testGVR).Namespace(namespace).Get(context.Background(), name, metav1.GetOptions{})
		if err == nil {
			result = obj
		}
		return err == nil
	}, namespace+"/"+name+" to exist")
	return result
}

func eventuallyNotFound(t *testing.T, client dynamic.Interface, namespace, name string) {
	t.Helper()
	eventually(t, func() bool {
		_, err := client.Resource(testGVR).Namespace(namespace).Get(context.Background(), name, metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	}, namespace+"/"+name+" to be deleted")
}

func eventually(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func assertServerSideApply(t *testing.T, client *dynamicfake.FakeDynamicClient) {
	t.Helper()
	for _, action := range client.Actions() {
		patch, ok := action.(clienttesting.PatchActionImpl)
		if !ok || patch.GetPatchType() != types.ApplyPatchType {
			continue
		}
		return
	}
	t.Fatal("no server-side apply action found")
}
