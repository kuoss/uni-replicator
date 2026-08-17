package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kuoss/uni-replicator/internal/config"
	"github.com/kuoss/uni-replicator/internal/controller"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	authorizationclient "k8s.io/client-go/kubernetes/typed/authorization/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

func main() {
	defaultKubeconfig := ""
	if home := homedir.HomeDir(); home != "" {
		defaultKubeconfig = home + "/.kube/config"
	}

	var (
		configPath string
		kubeconfig string
		resync     time.Duration
		workers    int
	)
	flag.StringVar(&configPath, "config", "", "path to the resource configuration (then UNI_REPLICATOR_CONFIG or standard locations)")
	flag.StringVar(&kubeconfig, "kubeconfig", "", "path to kubeconfig (then KUBECONFIG, in-cluster config, or ~/.kube/config)")
	flag.DurationVar(&resync, "resync-period", 10*time.Minute, "informer resync period")
	flag.IntVar(&workers, "workers", 2, "number of reconciliation workers")
	flag.Parse()

	if err := run(configPath, kubeconfig, defaultKubeconfig, resync, workers); err != nil {
		slog.Error("uni-replicator stopped", "error", err)
		os.Exit(1)
	}
}

func run(configPath, kubeconfig, defaultKubeconfig string, resync time.Duration, workers int) error {
	resolvedConfigPath, err := resolveConfigPath(configPath)
	if err != nil {
		return err
	}
	cfg, err := config.Load(resolvedConfigPath)
	if err != nil {
		return err
	}
	slog.Info("loaded configuration", "path", resolvedConfigPath)
	if workers < 1 {
		return fmt.Errorf("workers must be at least 1")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	restConfig, err := buildRESTConfig(kubeconfig, defaultKubeconfig)
	if err != nil {
		return err
	}
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create dynamic client: %w", err)
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create discovery client: %w", err)
	}
	authorizationClient, err := authorizationclient.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create authorization client: %w", err)
	}

	mappings, err := resolveResources(discoveryClient, cfg.Watches)
	if err != nil {
		return err
	}
	for i := range mappings {
		mappings[i].CascadeDeletionPolicy = controller.CascadeDeletionPolicy(cfg.Policy.CascadeDeletion)
		mappings[i].ExistingTargetPolicy = controller.ExistingTargetPolicy(cfg.Policy.ExistingTarget)
	}
	if err := validateResourcePermissions(ctx, authorizationClient.SelfSubjectAccessReviews(), mappings); err != nil {
		return err
	}
	warnIfWildcardPermissions(ctx, authorizationClient.SelfSubjectAccessReviews(), mappings)
	c, err := controller.New(dynamicClient, mappings, resync)
	if err != nil {
		return err
	}

	slog.Info("starting uni-replicator", "resources", len(mappings), "workers", workers)
	return c.Run(ctx, workers)
}

const configPathEnv = "UNI_REPLICATOR_CONFIG"

var defaultConfigPaths = []string{
	"./config.yaml",
	"./etc/config.yaml",
	"/etc/uni-replicator/config.yaml",
}

func resolveConfigPath(explicit string) (string, error) {
	return findConfigPath(explicit, os.Getenv(configPathEnv), defaultConfigPaths)
}

func findConfigPath(explicit, environment string, candidates []string) (string, error) {
	if explicit != "" {
		return requireConfigFile(explicit, "--config")
	}
	if environment != "" {
		return requireConfigFile(environment, configPathEnv)
	}

	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil {
			if !info.Mode().IsRegular() {
				return "", fmt.Errorf("configuration path %q is not a regular file", candidate)
			}
			return filepath.Abs(candidate)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("check configuration path %q: %w", candidate, err)
		}
	}
	return "", fmt.Errorf("no configuration file found (searched: %s); use --config or %s", strings.Join(candidates, ", "), configPathEnv)
}

func requireConfigFile(path, source string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("load configuration from %s path %q: %w", source, path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("load configuration from %s path %q: not a regular file", source, path)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve configuration path %q: %w", path, err)
	}
	return absPath, nil
}

var requiredResourceVerbs = [...]string{"get", "list", "watch", "patch", "delete"}

var auditedResourceVerbs = [...]string{
	"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection",
}

func validateResourcePermissions(ctx context.Context, client authorizationclient.SelfSubjectAccessReviewInterface, resources []controller.Resource) error {
	var denied []string
	for _, resource := range resources {
		for _, verb := range requiredResourceVerbs {
			review, err := client.Create(ctx, &authorizationv1.SelfSubjectAccessReview{
				Spec: authorizationv1.SelfSubjectAccessReviewSpec{
					ResourceAttributes: &authorizationv1.ResourceAttributes{
						Group:    resource.GVR.Group,
						Version:  resource.GVR.Version,
						Resource: resource.GVR.Resource,
						Verb:     verb,
					},
				},
			}, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("check %s permission for %s: %w", verb, resource.GVR, err)
			}
			if review.Status.Allowed {
				continue
			}
			permission := fmt.Sprintf("%s %s", verb, resource.GVR)
			if review.Status.Reason != "" {
				permission += fmt.Sprintf(" (%s)", review.Status.Reason)
			}
			if review.Status.EvaluationError != "" {
				permission += fmt.Sprintf(" (%s)", review.Status.EvaluationError)
			}
			denied = append(denied, permission)
		}
	}
	if len(denied) > 0 {
		return fmt.Errorf("missing required Kubernetes permissions: %s", strings.Join(denied, ", "))
	}
	return nil
}

func warnIfWildcardPermissions(ctx context.Context, client authorizationclient.SelfSubjectAccessReviewInterface, resources []controller.Resource) {
	verbs, err := wildcardResourcePermissions(ctx, client)
	if err != nil {
		slog.WarnContext(ctx, "could not check for wildcard Kubernetes permissions", "error", err)
		return
	}
	if len(verbs) == 0 {
		return
	}

	manifest, err := recommendedClusterRole(resources)
	if err != nil {
		slog.WarnContext(ctx, "wildcard Kubernetes permissions detected", "verbs", verbs, "renderError", err)
		return
	}
	slog.WarnContext(ctx, "wildcard Kubernetes permissions detected; consider using the recommended ClusterRole below", "verbs", verbs)
	if err := writeRecommendedClusterRole(os.Stderr, manifest); err != nil {
		slog.WarnContext(ctx, "could not print the recommended ClusterRole", "error", err)
	}
}

func writeRecommendedClusterRole(output io.Writer, manifest string) error {
	_, err := fmt.Fprintf(output, "\n# Recommended least-privilege ClusterRole\n%s", manifest)
	return err
}

func wildcardResourcePermissions(ctx context.Context, client authorizationclient.SelfSubjectAccessReviewInterface) ([]string, error) {
	var allowed []string
	for _, verb := range auditedResourceVerbs {
		review, err := client.Create(ctx, &authorizationv1.SelfSubjectAccessReview{
			Spec: authorizationv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Group:    "*",
					Resource: "*",
					Verb:     verb,
				},
			},
		}, metav1.CreateOptions{})
		if err != nil {
			return nil, fmt.Errorf("check wildcard %s permission: %w", verb, err)
		}
		if review.Status.EvaluationError != "" {
			return nil, fmt.Errorf("check wildcard %s permission: %s", verb, review.Status.EvaluationError)
		}
		if review.Status.Allowed {
			allowed = append(allowed, verb)
		}
	}
	return allowed, nil
}

type clusterRoleManifest struct {
	APIVersion string                    `json:"apiVersion" yaml:"apiVersion"`
	Kind       string                    `json:"kind" yaml:"kind"`
	Metadata   clusterRoleMetadata       `json:"metadata" yaml:"metadata"`
	Rules      []clusterRoleManifestRule `json:"rules" yaml:"rules"`
}

type clusterRoleMetadata struct {
	Name string `json:"name" yaml:"name"`
}

type clusterRoleManifestRule struct {
	APIGroups []string `json:"apiGroups" yaml:"apiGroups"`
	Resources []string `json:"resources" yaml:"resources"`
	Verbs     []string `json:"verbs" yaml:"verbs"`
}

func recommendedClusterRole(resources []controller.Resource) (string, error) {
	resourcesByGroup := make(map[string]map[string]struct{})
	for _, resource := range resources {
		if resourcesByGroup[resource.GVR.Group] == nil {
			resourcesByGroup[resource.GVR.Group] = make(map[string]struct{})
		}
		resourcesByGroup[resource.GVR.Group][resource.GVR.Resource] = struct{}{}
	}

	groups := make([]string, 0, len(resourcesByGroup))
	for group := range resourcesByGroup {
		groups = append(groups, group)
	}
	sort.Strings(groups)

	rules := make([]clusterRoleManifestRule, 0, len(groups))
	for _, group := range groups {
		resourceNames := make([]string, 0, len(resourcesByGroup[group]))
		for resource := range resourcesByGroup[group] {
			resourceNames = append(resourceNames, resource)
		}
		sort.Strings(resourceNames)
		rules = append(rules, clusterRoleManifestRule{
			APIGroups: []string{group},
			Resources: resourceNames,
			Verbs:     requiredResourceVerbs[:],
		})
	}

	var manifest strings.Builder
	manifest.WriteString("apiVersion: rbac.authorization.k8s.io/v1\n")
	manifest.WriteString("kind: ClusterRole\n")
	manifest.WriteString("metadata:\n")
	manifest.WriteString("  name: uni-replicator\n")
	manifest.WriteString("rules:\n")
	for _, rule := range rules {
		fmt.Fprintf(&manifest, "- apiGroups: %s\n", yamlFlowStringList(rule.APIGroups))
		fmt.Fprintf(&manifest, "  resources: %s\n", yamlFlowStringList(rule.Resources))
		fmt.Fprintf(&manifest, "  verbs: %s\n", yamlFlowStringList(rule.Verbs))
	}
	return manifest.String(), nil
}

func yamlFlowStringList(values []string) string {
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = strconv.Quote(value)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func buildRESTConfig(kubeconfig, defaultKubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("load kubeconfig %q: %w", kubeconfig, err)
		}
		return cfg, nil
	}
	if kubeconfigEnv := os.Getenv(clientcmd.RecommendedConfigPathEnvVar); kubeconfigEnv != "" {
		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{}).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("load KUBECONFIG %q: %w", kubeconfigEnv, err)
		}
		return cfg, nil
	}
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	if defaultKubeconfig == "" {
		return nil, fmt.Errorf("not running in a cluster and no kubeconfig was supplied")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", defaultKubeconfig)
	if err != nil {
		return nil, fmt.Errorf("load default kubeconfig %q: %w", defaultKubeconfig, err)
	}
	return cfg, nil
}

func resolveResources(client discovery.DiscoveryInterface, watches []config.Watch) ([]controller.Resource, error) {
	seen := make(map[schema.GroupVersionResource]struct{})
	discovered := make(map[string]*metav1.APIResourceList)
	var result []controller.Resource

	for _, watch := range watches {
		gv, err := schema.ParseGroupVersion(watch.APIVersion)
		if err != nil {
			return nil, fmt.Errorf("invalid apiVersion %q: %w", watch.APIVersion, err)
		}
		apiResources := discovered[watch.APIVersion]
		if apiResources == nil {
			apiResources, err = client.ServerResourcesForGroupVersion(watch.APIVersion)
			if err != nil {
				return nil, fmt.Errorf("discover resources %v in apiVersion %q: %w", watch.Resources, watch.APIVersion, err)
			}
			discovered[watch.APIVersion] = apiResources
		}

		for _, resourceName := range watch.Resources {
			if strings.Contains(resourceName, "/") {
				return nil, fmt.Errorf("subresource %q in apiVersion %q is not supported", resourceName, watch.APIVersion)
			}
			var apiResource *metav1.APIResource
			for i := range apiResources.APIResources {
				if apiResources.APIResources[i].Name == resourceName {
					apiResource = &apiResources.APIResources[i]
					break
				}
			}
			if apiResource == nil {
				return nil, fmt.Errorf("resource %q does not exist in apiVersion %q", resourceName, watch.APIVersion)
			}
			if !apiResource.Namespaced {
				return nil, fmt.Errorf("%s resource %q is cluster-scoped; only namespaced resources are supported", watch.APIVersion, resourceName)
			}
			if apiResource.Kind == "" {
				return nil, fmt.Errorf("%s resource %q has no kind in discovery", watch.APIVersion, resourceName)
			}
			gvr := gv.WithResource(resourceName)
			if _, exists := seen[gvr]; exists {
				return nil, fmt.Errorf("resource %s is configured more than once", gvr)
			}
			seen[gvr] = struct{}{}
			result = append(result, controller.Resource{
				GVR: gvr,
				GVK: gv.WithKind(apiResource.Kind),
			})
		}
	}
	return result, nil
}
