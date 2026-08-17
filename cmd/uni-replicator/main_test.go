package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kuoss/uni-replicator/internal/config"
	"github.com/kuoss/uni-replicator/internal/controller"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

type fakeSelfSubjectAccessReviews struct {
	create func(*authorizationv1.SelfSubjectAccessReview) (*authorizationv1.SelfSubjectAccessReview, error)
}

func (f fakeSelfSubjectAccessReviews) Create(_ context.Context, review *authorizationv1.SelfSubjectAccessReview, _ metav1.CreateOptions) (*authorizationv1.SelfSubjectAccessReview, error) {
	return f.create(review)
}

func writeKubeconfig(t *testing.T, server string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	data := []byte(`apiVersion: v1
kind: Config
clusters:
  - name: test
    cluster:
      server: ` + server + `
contexts:
  - name: test
    context:
      cluster: test
      user: test
current-context: test
users:
  - name: test
    user: {}
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeConfigFile(t *testing.T, directory, name string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("watches:\n  - apiVersion: v1\n    resources:\n      - secrets\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFindConfigPathPrecedence(t *testing.T) {
	directory := t.TempDir()
	explicit := writeConfigFile(t, directory, "explicit.yaml")
	environment := writeConfigFile(t, directory, "environment.yaml")
	candidate := writeConfigFile(t, directory, "candidate.yaml")

	path, err := findConfigPath(explicit, environment, []string{candidate})
	if err != nil {
		t.Fatalf("findConfigPath() error = %v", err)
	}
	if path != explicit {
		t.Fatalf("path = %q, want explicit path %q", path, explicit)
	}

	path, err = findConfigPath("", environment, []string{candidate})
	if err != nil {
		t.Fatalf("findConfigPath() error = %v", err)
	}
	if path != environment {
		t.Fatalf("path = %q, want environment path %q", path, environment)
	}
}

func TestFindConfigPathUsesFirstExistingCandidate(t *testing.T) {
	directory := t.TempDir()
	missing := filepath.Join(directory, "missing.yaml")
	first := writeConfigFile(t, directory, "config.yaml")
	second := writeConfigFile(t, directory, "etc/config.yaml")

	path, err := findConfigPath("", "", []string{missing, first, second})
	if err != nil {
		t.Fatalf("findConfigPath() error = %v", err)
	}
	if path != first {
		t.Fatalf("path = %q, want first existing candidate %q", path, first)
	}
}

func TestFindConfigPathDoesNotFallbackFromExplicitError(t *testing.T) {
	directory := t.TempDir()
	missing := filepath.Join(directory, "missing.yaml")
	fallback := writeConfigFile(t, directory, "config.yaml")

	_, err := findConfigPath(missing, "", []string{fallback})
	if err == nil || !strings.Contains(err.Error(), "--config") {
		t.Fatalf("findConfigPath() error = %v", err)
	}
}

func TestFindConfigPathReportsSearchedCandidates(t *testing.T) {
	directory := t.TempDir()
	candidates := []string{filepath.Join(directory, "config.yaml"), filepath.Join(directory, "etc/config.yaml")}

	_, err := findConfigPath("", "", candidates)
	if err == nil {
		t.Fatal("findConfigPath() succeeded without a configuration file")
	}
	for _, candidate := range candidates {
		if !strings.Contains(err.Error(), candidate) {
			t.Errorf("error %q does not contain %q", err, candidate)
		}
	}
}

func TestBuildRESTConfigPrefersExplicitKubeconfig(t *testing.T) {
	explicit := writeKubeconfig(t, "https://explicit.example")
	t.Setenv(clientcmd.RecommendedConfigPathEnvVar, filepath.Join(t.TempDir(), "missing"))

	cfg, err := buildRESTConfig(explicit, filepath.Join(t.TempDir(), "missing-default"))
	if err != nil {
		t.Fatalf("buildRESTConfig() error = %v", err)
	}
	if cfg.Host != "https://explicit.example" {
		t.Fatalf("host = %q, want explicit kubeconfig host", cfg.Host)
	}
}

func TestBuildRESTConfigUsesKubeconfigEnvironment(t *testing.T) {
	environment := writeKubeconfig(t, "https://environment.example")
	t.Setenv(clientcmd.RecommendedConfigPathEnvVar, environment)

	cfg, err := buildRESTConfig("", filepath.Join(t.TempDir(), "missing-default"))
	if err != nil {
		t.Fatalf("buildRESTConfig() error = %v", err)
	}
	if cfg.Host != "https://environment.example" {
		t.Fatalf("host = %q, want KUBECONFIG host", cfg.Host)
	}
}

func TestBuildRESTConfigDoesNotFallbackFromInvalidKubeconfigEnvironment(t *testing.T) {
	defaultKubeconfig := writeKubeconfig(t, "https://default.example")
	missing := filepath.Join(t.TempDir(), "missing")
	t.Setenv(clientcmd.RecommendedConfigPathEnvVar, missing)

	_, err := buildRESTConfig("", defaultKubeconfig)
	if err == nil || !strings.Contains(err.Error(), "load KUBECONFIG") {
		t.Fatalf("buildRESTConfig() error = %v", err)
	}
}

func TestBuildRESTConfigUsesDefaultAfterInClusterConfig(t *testing.T) {
	defaultKubeconfig := writeKubeconfig(t, "https://default.example")
	t.Setenv(clientcmd.RecommendedConfigPathEnvVar, "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	cfg, err := buildRESTConfig("", defaultKubeconfig)
	if err != nil {
		t.Fatalf("buildRESTConfig() error = %v", err)
	}
	if cfg.Host != "https://default.example" {
		t.Fatalf("host = %q, want default kubeconfig host", cfg.Host)
	}
}

func TestResolveArbitraryNamespacedCRD(t *testing.T) {
	discovery := &fake.FakeDiscovery{Fake: &clienttesting.Fake{}}
	discovery.Resources = []*metav1.APIResourceList{{
		GroupVersion: "k8s.nginx.org/v1",
		APIResources: []metav1.APIResource{{Name: "policies", SingularName: "policy", Namespaced: true, Kind: "Policy", Verbs: metav1.Verbs{"get", "list", "watch"}}},
	}}

	resources, err := resolveResources(discovery, []config.Watch{{APIVersion: "k8s.nginx.org/v1", Resources: []string{"policies"}}})
	if err != nil {
		t.Fatalf("resolveResources() error = %v", err)
	}
	if got := resources[0].GVR.String(); got != "k8s.nginx.org/v1, Resource=policies" {
		t.Fatalf("resolved GVR = %q", got)
	}
	if got := resources[0].GVK.String(); got != "k8s.nginx.org/v1, Kind=Policy" {
		t.Fatalf("resolved GVK = %q", got)
	}
}

func TestResolveRejectsClusterScopedResource(t *testing.T) {
	discovery := &fake.FakeDiscovery{Fake: &clienttesting.Fake{}}
	discovery.Resources = []*metav1.APIResourceList{{
		GroupVersion: "example.io/v1",
		APIResources: []metav1.APIResource{{Name: "clusters", Namespaced: false, Kind: "Cluster"}},
	}}
	_, err := resolveResources(discovery, []config.Watch{{APIVersion: "example.io/v1", Resources: []string{"clusters"}}})
	if err == nil {
		t.Fatal("resolveResources() accepted a cluster-scoped resource")
	}
}

func TestResolveRejectsMissingResource(t *testing.T) {
	discovery := &fake.FakeDiscovery{Fake: &clienttesting.Fake{}}
	discovery.Resources = []*metav1.APIResourceList{{
		GroupVersion: "v1",
		APIResources: []metav1.APIResource{{Name: "secrets", Namespaced: true, Kind: "Secret"}},
	}}

	_, err := resolveResources(discovery, []config.Watch{{APIVersion: "v1", Resources: []string{"configmaps"}}})
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("resolveResources() error = %v", err)
	}
}

func TestResolveDiscoveryErrorIncludesConfiguredResources(t *testing.T) {
	discovery := &fake.FakeDiscovery{Fake: &clienttesting.Fake{}}
	watches := []config.Watch{{
		APIVersion: "example.io/v1",
		Resources:  []string{"widgets", "gadgets"},
	}}

	_, err := resolveResources(discovery, watches)
	if err == nil {
		t.Fatal("resolveResources() succeeded for an unavailable apiVersion")
	}
	for _, want := range []string{"example.io/v1", "widgets", "gadgets"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestResolveRejectsDuplicateResource(t *testing.T) {
	discovery := &fake.FakeDiscovery{Fake: &clienttesting.Fake{}}
	discovery.Resources = []*metav1.APIResourceList{{
		GroupVersion: "v1",
		APIResources: []metav1.APIResource{{Name: "secrets", Namespaced: true, Kind: "Secret"}},
	}}
	watches := []config.Watch{
		{APIVersion: "v1", Resources: []string{"secrets"}},
		{APIVersion: "v1", Resources: []string{"secrets"}},
	}

	_, err := resolveResources(discovery, watches)
	if err == nil || !strings.Contains(err.Error(), "configured more than once") {
		t.Fatalf("resolveResources() error = %v", err)
	}
}

func TestResolveRejectsSubresource(t *testing.T) {
	discovery := &fake.FakeDiscovery{Fake: &clienttesting.Fake{}}
	discovery.Resources = []*metav1.APIResourceList{{
		GroupVersion: "apps/v1",
		APIResources: []metav1.APIResource{{Name: "deployments/status", Namespaced: true, Kind: "Deployment"}},
	}}

	_, err := resolveResources(discovery, []config.Watch{{APIVersion: "apps/v1", Resources: []string{"deployments/status"}}})
	if err == nil || !strings.Contains(err.Error(), "subresource") {
		t.Fatalf("resolveResources() error = %v", err)
	}
}

func TestValidateResourcePermissions(t *testing.T) {
	var checked []string
	client := fakeSelfSubjectAccessReviews{create: func(review *authorizationv1.SelfSubjectAccessReview) (*authorizationv1.SelfSubjectAccessReview, error) {
		attributes := review.Spec.ResourceAttributes
		checked = append(checked, attributes.Verb)
		return &authorizationv1.SelfSubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}}, nil
	}}
	resources := []controller.Resource{{GVR: schema.GroupVersionResource{Group: "example.io", Version: "v1", Resource: "widgets"}}}

	if err := validateResourcePermissions(context.Background(), client, resources); err != nil {
		t.Fatalf("validateResourcePermissions() error = %v", err)
	}
	if got, want := strings.Join(checked, ","), strings.Join(requiredResourceVerbs[:], ","); got != want {
		t.Fatalf("checked verbs = %q, want %q", got, want)
	}
}

func TestValidateResourcePermissionsReportsAllDeniedVerbs(t *testing.T) {
	client := fakeSelfSubjectAccessReviews{create: func(review *authorizationv1.SelfSubjectAccessReview) (*authorizationv1.SelfSubjectAccessReview, error) {
		verb := review.Spec.ResourceAttributes.Verb
		return &authorizationv1.SelfSubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{
			Allowed: verb != "watch" && verb != "patch",
			Reason:  "RBAC denied",
		}}, nil
	}}
	resources := []controller.Resource{{GVR: schema.GroupVersionResource{Group: "example.io", Version: "v1", Resource: "widgets"}}}

	err := validateResourcePermissions(context.Background(), client, resources)
	if err == nil {
		t.Fatal("validateResourcePermissions() succeeded with denied permissions")
	}
	for _, want := range []string{"watch example.io/v1, Resource=widgets", "patch example.io/v1, Resource=widgets", "RBAC denied"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestValidateResourcePermissionsReturnsReviewError(t *testing.T) {
	client := fakeSelfSubjectAccessReviews{create: func(*authorizationv1.SelfSubjectAccessReview) (*authorizationv1.SelfSubjectAccessReview, error) {
		return nil, errors.New("API unavailable")
	}}
	resources := []controller.Resource{{GVR: schema.GroupVersionResource{Version: "v1", Resource: "secrets"}}}

	err := validateResourcePermissions(context.Background(), client, resources)
	if err == nil || !strings.Contains(err.Error(), "check get permission for /v1, Resource=secrets") {
		t.Fatalf("validateResourcePermissions() error = %v", err)
	}
}

func TestWildcardResourcePermissions(t *testing.T) {
	client := fakeSelfSubjectAccessReviews{create: func(review *authorizationv1.SelfSubjectAccessReview) (*authorizationv1.SelfSubjectAccessReview, error) {
		attributes := review.Spec.ResourceAttributes
		if attributes.Group != "*" || attributes.Resource != "*" {
			t.Fatalf("wildcard attributes = group %q, resource %q", attributes.Group, attributes.Resource)
		}
		allowed := attributes.Verb == "create" || attributes.Verb == "deletecollection"
		return &authorizationv1.SelfSubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: allowed}}, nil
	}}

	verbs, err := wildcardResourcePermissions(context.Background(), client)
	if err != nil {
		t.Fatalf("wildcardResourcePermissions() error = %v", err)
	}
	if want := []string{"create", "deletecollection"}; !reflect.DeepEqual(verbs, want) {
		t.Fatalf("verbs = %v, want %v", verbs, want)
	}
}

func TestRecommendedClusterRole(t *testing.T) {
	resources := []controller.Resource{
		{GVR: schema.GroupVersionResource{Group: "example.io", Version: "v1", Resource: "widgets"}},
		{GVR: schema.GroupVersionResource{Version: "v1", Resource: "secrets"}},
		{GVR: schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}},
		{GVR: schema.GroupVersionResource{Group: "example.io", Version: "v2", Resource: "widgets"}},
	}

	data, err := recommendedClusterRole(resources)
	if err != nil {
		t.Fatalf("recommendedClusterRole() error = %v", err)
	}
	for _, want := range []string{
		`- apiGroups: [""]`,
		`  resources: ["configmaps", "secrets"]`,
		`  verbs: ["get", "list", "watch", "patch", "delete"]`,
		`- apiGroups: ["example.io"]`,
		`  resources: ["widgets"]`,
	} {
		if !strings.Contains(data, want) {
			t.Errorf("generated ClusterRole does not contain %q:\n%s", want, data)
		}
	}
	var manifest clusterRoleManifest
	if err := yaml.Unmarshal([]byte(data), &manifest); err != nil {
		t.Fatalf("unmarshal generated ClusterRole: %v", err)
	}
	want := clusterRoleManifest{
		APIVersion: "rbac.authorization.k8s.io/v1",
		Kind:       "ClusterRole",
		Metadata:   clusterRoleMetadata{Name: "uni-replicator"},
		Rules: []clusterRoleManifestRule{
			{APIGroups: []string{""}, Resources: []string{"configmaps", "secrets"}, Verbs: requiredResourceVerbs[:]},
			{APIGroups: []string{"example.io"}, Resources: []string{"widgets"}, Verbs: requiredResourceVerbs[:]},
		},
	}
	if !reflect.DeepEqual(manifest, want) {
		t.Fatalf("manifest = %#v, want %#v", manifest, want)
	}
}

func TestWriteRecommendedClusterRolePreservesYAMLFormatting(t *testing.T) {
	manifest := "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\n"
	var output bytes.Buffer

	if err := writeRecommendedClusterRole(&output, manifest); err != nil {
		t.Fatalf("writeRecommendedClusterRole() error = %v", err)
	}
	want := "\n# Recommended least-privilege ClusterRole\n" + manifest
	if output.String() != want {
		t.Fatalf("output = %q, want %q", output.String(), want)
	}
}
