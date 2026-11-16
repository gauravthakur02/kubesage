package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"k8s.io/client-go/tools/clientcmd"
)

// ContextInfo is one kubeconfig context.
type ContextInfo struct {
	Name, Cluster, Server, User, Namespace string
	Current                                bool
	AuthExec                               string // exec credential plugin (kubelogin, aws, gke-gcloud-auth-plugin…)
}

func pathOptions(kubeconfig string) *clientcmd.PathOptions {
	po := clientcmd.NewDefaultPathOptions()
	if kubeconfig != "" {
		po.LoadingRules.ExplicitPath = kubeconfig
	}
	return po
}

// KubeContexts lists the contexts of the kubeconfig (all files in $KUBECONFIG, merged).
func KubeContexts(kubeconfig string) ([]ContextInfo, error) {
	cfg, err := pathOptions(kubeconfig).GetStartingConfig()
	if err != nil {
		return nil, err
	}
	var out []ContextInfo
	for name, ctx := range cfg.Contexts {
		ci := ContextInfo{Name: name, Cluster: ctx.Cluster, User: ctx.AuthInfo, Namespace: ctx.Namespace, Current: name == cfg.CurrentContext}
		if cl, ok := cfg.Clusters[ctx.Cluster]; ok {
			ci.Server = cl.Server
		}
		if u, ok := cfg.AuthInfos[ctx.AuthInfo]; ok && u.Exec != nil {
			ci.AuthExec = u.Exec.Command
		}
		out = append(out, ci)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// backupKubeconfig copies every kubeconfig file that exists to <file>.kubesage-bak-<time>.
func backupKubeconfig(po *clientcmd.PathOptions) (string, error) {
	stamp := time.Now().Format("20060102-150405")
	var made []string
	for _, f := range po.GetLoadingPrecedence() {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		dst := f + ".kubesage-bak-" + stamp
		if err := os.WriteFile(dst, b, 0o600); err != nil {
			return "", err
		}
		made = append(made, dst)
	}
	return strings.Join(made, ", "), nil
}

// RenameContext renames a context (cluster and user entries keep their names). Returns the backup path.
func RenameContext(kubeconfig, from, to string) (string, error) {
	po := pathOptions(kubeconfig)
	cfg, err := po.GetStartingConfig()
	if err != nil {
		return "", err
	}
	ctx, ok := cfg.Contexts[from]
	if !ok {
		return "", fmt.Errorf("context %q not found", from)
	}
	if _, exists := cfg.Contexts[to]; exists {
		return "", fmt.Errorf("context %q already exists", to)
	}
	bak, err := backupKubeconfig(po)
	if err != nil {
		return "", err
	}
	cfg.Contexts[to] = ctx
	delete(cfg.Contexts, from)
	if cfg.CurrentContext == from {
		cfg.CurrentContext = to
	}
	return bak, clientcmd.ModifyConfig(po, *cfg, true)
}

// DeleteContext removes a context, and its cluster and user entries when no other context uses them.
func DeleteContext(kubeconfig, name string) (string, error) {
	po := pathOptions(kubeconfig)
	cfg, err := po.GetStartingConfig()
	if err != nil {
		return "", err
	}
	ctx, ok := cfg.Contexts[name]
	if !ok {
		return "", fmt.Errorf("context %q not found", name)
	}
	bak, err := backupKubeconfig(po)
	if err != nil {
		return "", err
	}
	delete(cfg.Contexts, name)
	clusterUsed, userUsed := false, false
	for _, c := range cfg.Contexts {
		clusterUsed = clusterUsed || c.Cluster == ctx.Cluster
		userUsed = userUsed || c.AuthInfo == ctx.AuthInfo
	}
	if !clusterUsed {
		delete(cfg.Clusters, ctx.Cluster)
	}
	if !userUsed {
		delete(cfg.AuthInfos, ctx.AuthInfo)
	}
	if cfg.CurrentContext == name {
		cfg.CurrentContext = ""
	}
	return bak, clientcmd.ModifyConfig(po, *cfg, true)
}

// ReloadContexts re-reads the kubeconfig so Contexts() reflects changes.
func (c *Client) ReloadContexts() error {
	raw, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules(c.Kubeconfig), &clientcmd.ConfigOverrides{}).RawConfig()
	if err != nil {
		return err
	}
	c.raw = raw
	return nil
}

// AzureSubscription is an enabled Azure subscription.
type AzureSubscription struct{ ID, Name string }

// AKSCluster is an AKS cluster found with az aks list.
type AKSCluster struct {
	Name, ResourceGroup, Location, Version, Subscription string
}

// AzureSubscriptions lists enabled subscriptions (az account list).
func AzureSubscriptions(ctx context.Context) ([]AzureSubscription, error) {
	out, err := exec.CommandContext(ctx, "az", "account", "list", "--query", "[?state=='Enabled'].{ID:id, Name:name}", "-o", "json").Output()
	if err != nil {
		return nil, azErr(err)
	}
	var subs []AzureSubscription
	return subs, json.Unmarshal(out, &subs)
}

// AKSClusters lists AKS clusters in a subscription (read-only).
func AKSClusters(ctx context.Context, subscription string) ([]AKSCluster, error) {
	out, err := exec.CommandContext(ctx, "az", "aks", "list", "--subscription", subscription,
		"--query", "[].{Name:name, ResourceGroup:resourceGroup, Location:location, Version:currentKubernetesVersion}", "-o", "json").Output()
	if err != nil {
		return nil, azErr(err)
	}
	var cl []AKSCluster
	if err := json.Unmarshal(out, &cl); err != nil {
		return nil, err
	}
	for i := range cl {
		cl[i].Subscription = subscription
	}
	sort.Slice(cl, func(i, j int) bool { return cl[i].Name < cl[j].Name })
	return cl, nil
}

// AKSGetCredentials merges an AKS cluster's user credentials into the kubeconfig (az aks get-credentials).
func AKSGetCredentials(ctx context.Context, kubeconfig string, cl AKSCluster) (string, error) {
	po := pathOptions(kubeconfig)
	bak, err := backupKubeconfig(po)
	if err != nil {
		return "", err
	}
	args := []string{"aks", "get-credentials", "--subscription", cl.Subscription, "-g", cl.ResourceGroup, "-n", cl.Name}
	if kubeconfig != "" {
		args = append(args, "--file", kubeconfig)
	}
	if out, err := exec.CommandContext(ctx, "az", args...).CombinedOutput(); err != nil {
		return bak, fmt.Errorf("az aks get-credentials: %s", strings.TrimSpace(string(out)))
	}
	return bak, nil
}

func azErr(err error) error {
	if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
		return fmt.Errorf("az: %s", firstLine(strings.TrimSpace(string(ee.Stderr))))
	}
	if _, lerr := exec.LookPath("az"); lerr != nil {
		return fmt.Errorf("the az CLI is not installed")
	}
	return err
}
