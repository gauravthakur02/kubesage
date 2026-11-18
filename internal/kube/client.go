// Package kube wraps client-go with the operations kubeweave needs.
package kube

import (
	"fmt"
	"sort"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// AllNamespaces is the sentinel namespace meaning "every namespace".
const AllNamespaces = ""

// Client is a connection to one kubeconfig context.
type Client struct {
	Clientset  kubernetes.Interface
	Dynamic    dynamic.Interface
	Context    string
	Namespace  string // "" means all namespaces
	Kubeconfig string
	Rest       *rest.Config
	warns      *warnCollector
	raw        clientcmdapi.Config
	nsc        nsCache
}

func loadingRules(kubeconfig string) *clientcmd.ClientConfigLoadingRules {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	return rules
}

// New connects to the given context (empty = current context).
func New(kubeconfig, context, namespace string) (*Client, error) {
	rules := loadingRules(kubeconfig)
	overrides := &clientcmd.ConfigOverrides{CurrentContext: context}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	raw, err := cc.RawConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	if context == "" {
		context = raw.CurrentContext
	}
	if context == "" {
		return nil, fmt.Errorf("no current kubeconfig context; pass --context")
	}
	if _, ok := raw.Contexts[context]; !ok {
		return nil, fmt.Errorf("context %q not found in kubeconfig", context)
	}
	rc, err := cc.ClientConfig()
	if err != nil {
		return nil, err
	}
	rc.QPS, rc.Burst = 50, 100
	warns := &warnCollector{}
	rc.WarningHandler = warns
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	if namespace == "" {
		if ns, _, err := cc.Namespace(); err == nil {
			namespace = ns
		}
	}
	return &Client{Clientset: cs, Dynamic: dyn, Rest: rc, warns: warns, Context: context, Namespace: namespace, Kubeconfig: kubeconfig, raw: raw}, nil
}

// Contexts lists kubeconfig context names, sorted.
func (c *Client) Contexts() []string {
	out := make([]string, 0, len(c.raw.Contexts))
	for name := range c.raw.Contexts {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ClusterName returns the cluster backing the current context.
func (c *Client) ClusterName() string {
	if ctx, ok := c.raw.Contexts[c.Context]; ok {
		return ctx.Cluster
	}
	return ""
}

// ServerVersion returns the API server git version (e.g. v1.31.2).
func (c *Client) ServerVersion() (string, error) {
	v, err := c.Clientset.Discovery().ServerVersion()
	if err != nil {
		return "", err
	}
	return v.GitVersion, nil
}

// NamespaceLabel is a human label for the active namespace.
func (c *Client) NamespaceLabel() string {
	if c.Namespace == AllNamespaces {
		return "all"
	}
	return c.Namespace
}

// WithNamespace returns a client sharing this connection but scoped to ns.
func (c *Client) WithNamespace(ns string) *Client {
	return &Client{Clientset: c.Clientset, Dynamic: c.Dynamic, Rest: c.Rest, warns: c.warns, Context: c.Context,
		Namespace: ns, Kubeconfig: c.Kubeconfig, raw: c.raw}
}

// ClusterOf returns the cluster name of any kubeconfig context.
func (c *Client) ClusterOf(ctx string) string {
	if x, ok := c.raw.Contexts[ctx]; ok {
		return x.Cluster
	}
	return ""
}

// NewForTesting builds a client on any clientset with a fake kubeconfig listing contexts (context → cluster).
func NewForTesting(cs kubernetes.Interface, context, namespace string, contexts map[string]string) *Client {
	raw := clientcmdapi.Config{Contexts: map[string]*clientcmdapi.Context{}}
	for ctx, cluster := range contexts {
		raw.Contexts[ctx] = &clientcmdapi.Context{Cluster: cluster}
	}
	return &Client{Clientset: cs, Context: context, Namespace: namespace, raw: raw}
}

// Impersonate returns a client that acts as another user (the API server checks that this client may
// impersonate). Used by kubeweave serve so each signed-in person works with their own permissions.
func (c *Client) Impersonate(user string, groups []string) (*Client, error) {
	if c.Rest == nil {
		return nil, fmt.Errorf("impersonation needs a live cluster connection")
	}
	rc := rest.CopyConfig(c.Rest)
	rc.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: groups}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	return &Client{Clientset: cs, Dynamic: dyn, Rest: rc, warns: c.warns, Context: c.Context, Namespace: c.Namespace,
		Kubeconfig: c.Kubeconfig, raw: c.raw}, nil
}
