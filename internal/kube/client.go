// Package kube loads one kubeconfig context and lists arbitrary API resources
// through the dynamic client, resolving kubectl-style resource arguments
// (plural, singular, Kind, shortname, resource.group) via discovery.
package kube

import (
	"context"
	"fmt"
	"os"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/pager"
)

// Client talks to a single kubeconfig context.
type Client struct {
	Context string
	dyn     dynamic.Interface
	mapper  meta.RESTMapper
}

// Resource is a resolved API resource.
type Resource struct {
	GVR        schema.GroupVersionResource
	Kind       string
	Namespaced bool
}

var namespacesGVR = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}

// NewClient builds a client for contextName. kubeconfig is an explicit file
// path; when empty the default loading rules apply ($KUBECONFIG, which may
// list several files, then ~/.kube/config).
func NewClient(kubeconfig, contextName string, timeout time.Duration) (*Client, error) {
	cfg, err := restConfig(kubeconfig, contextName)
	if err != nil {
		return nil, err
	}
	cfg.Timeout = timeout
	// Discovery on CRD-heavy clusters issues many requests; lift the client-side
	// limiter so it does not throttle a read-only tool.
	cfg.QPS = 50
	cfg.Burst = 100
	// Deprecation warnings would interleave with the per-context error lines.
	cfg.WarningHandler = rest.NoWarnings{}

	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create dynamic client: %w", err)
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create discovery client: %w", err)
	}
	base := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc))
	// The shortcut expander is what turns "deploy"/"svc" into their long forms;
	// the plain discovery mapper only knows plural, singular and Kind.
	mapper := restmapper.NewShortcutExpander(base, dc, func(msg string) {
		fmt.Fprintf(os.Stderr, "kquery: %s: %s\n", contextName, msg)
	})
	return &Client{Context: contextName, dyn: dyn, mapper: mapper}, nil
}

func restConfig(kubeconfig, contextName string) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)

	// client-go silently falls back to current-context when the override names
	// a context that does not exist; refuse instead of querying the wrong cluster.
	raw, err := cc.RawConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	if _, ok := raw.Contexts[contextName]; !ok {
		return nil, fmt.Errorf("context %q not found in kubeconfig (see: kubectl config get-contexts)", contextName)
	}
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("build client config: %w", err)
	}
	return cfg, nil
}

// Resolve maps a kubectl-style resource argument to an API resource.
func (c *Client) Resolve(arg string) (Resource, error) {
	mapping, err := c.mappingFor(arg)
	if err != nil {
		return Resource{}, fmt.Errorf("cannot resolve %q to an API resource (see: kubectl api-resources): %w", arg, err)
	}
	return Resource{
		GVR:        mapping.Resource,
		Kind:       mapping.GroupVersionKind.Kind,
		Namespaced: mapping.Scope.Name() == meta.RESTScopeNameNamespace,
	}, nil
}

// mappingFor mirrors kubectl's resource.Builder lookup: try the argument as a
// resource name first (plural, singular, shortname, resource.version.group),
// then as a Kind.
func (c *Client) mappingFor(arg string) (*meta.RESTMapping, error) {
	fullGVR, groupResource := schema.ParseResourceArg(arg)
	gvk := schema.GroupVersionKind{}
	if fullGVR != nil {
		gvk, _ = c.mapper.KindFor(*fullGVR)
	}
	if gvk.Empty() {
		gvk, _ = c.mapper.KindFor(groupResource.WithVersion(""))
	}
	if !gvk.Empty() {
		return c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	}

	fullGVK, groupKind := schema.ParseKindArg(arg)
	if fullGVK == nil {
		g := groupKind.WithVersion("")
		fullGVK = &g
	}
	if !fullGVK.Empty() {
		if m, err := c.mapper.RESTMapping(fullGVK.GroupKind(), fullGVK.Version); err == nil {
			return m, nil
		}
	}
	return c.mapper.RESTMapping(groupKind, gvk.Version)
}

// List returns every object of res in namespace (ignored for cluster-scoped
// resources), following pagination.
func (c *Client) List(ctx context.Context, res Resource, namespace string) ([]*unstructured.Unstructured, error) {
	var ri dynamic.ResourceInterface = c.dyn.Resource(res.GVR)
	if res.Namespaced {
		ri = c.dyn.Resource(res.GVR).Namespace(namespace)
	}
	p := pager.New(func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		return ri.List(ctx, opts)
	})
	var items []*unstructured.Unstructured
	err := p.EachListItem(ctx, metav1.ListOptions{}, func(obj runtime.Object) error {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return fmt.Errorf("unexpected list item type %T", obj)
		}
		items = append(items, u)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return items, nil
}

// NamespaceMissing reports whether namespace is known not to exist. Listing a
// namespaced resource in a nonexistent namespace returns an empty list, not an
// error, so callers check this when a list comes back empty. When the
// namespace itself cannot be read (RBAC) it returns false: the empty list stands.
func (c *Client) NamespaceMissing(ctx context.Context, namespace string) bool {
	_, err := c.dyn.Resource(namespacesGVR).Get(ctx, namespace, metav1.GetOptions{})
	return apierrors.IsNotFound(err)
}
