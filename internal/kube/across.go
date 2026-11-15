package kube

import (
	"context"
	"sort"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Many enterprise clusters (e.g. AKS with Entra ID RBAC) grant namespace-scoped
// access only: listing pods works cluster-wide but deployments do not. For
// "all namespaces" views we fall back to listing each namespace in parallel
// and silently skip the ones the user cannot read.

type nsCache struct {
	mu    sync.Mutex
	names []string
	at    time.Time
}

func (c *Client) namespaceNames(ctx context.Context) ([]string, error) {
	c.nsc.mu.Lock()
	defer c.nsc.mu.Unlock()
	if c.nsc.names != nil && time.Since(c.nsc.at) < time.Minute {
		return c.nsc.names, nil
	}
	list, err := c.Clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for _, n := range list.Items {
		names = append(names, n.Name)
	}
	sort.Strings(names)
	c.nsc.names, c.nsc.at = names, time.Now()
	return names, nil
}

// listAcross runs list for ns; when ns is "all" and the cluster-wide call is
// forbidden, it retries namespace by namespace.
func listAcross[T any](ctx context.Context, c *Client, ns string, list func(ns string) ([]T, error)) ([]T, error) {
	items, err := list(ns)
	if err == nil || ns != AllNamespaces || !apierrors.IsForbidden(err) {
		return items, err
	}
	names, nerr := c.namespaceNames(ctx)
	if nerr != nil {
		return nil, err
	}
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		out     []T
		allowed int
		sem     = make(chan struct{}, 8)
	)
	for _, n := range names {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			got, e := list(n)
			if e != nil {
				return
			}
			mu.Lock()
			out = append(out, got...)
			allowed++
			mu.Unlock()
		}(n)
	}
	wg.Wait()
	if allowed == 0 {
		return nil, err
	}
	return out, nil
}

// Typed, RBAC-tolerant list helpers used by Scan and Pulse.

func (c *Client) listPodsAcross(ctx context.Context, ns string) ([]corev1.Pod, error) {
	return listAcross(ctx, c, ns, func(n string) ([]corev1.Pod, error) {
		l, err := c.Clientset.CoreV1().Pods(n).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (c *Client) listDeploymentsAcross(ctx context.Context, ns string) ([]appsv1.Deployment, error) {
	return listAcross(ctx, c, ns, func(n string) ([]appsv1.Deployment, error) {
		l, err := c.Clientset.AppsV1().Deployments(n).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (c *Client) listStatefulSetsAcross(ctx context.Context, ns string) ([]appsv1.StatefulSet, error) {
	return listAcross(ctx, c, ns, func(n string) ([]appsv1.StatefulSet, error) {
		l, err := c.Clientset.AppsV1().StatefulSets(n).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (c *Client) listDaemonSetsAcross(ctx context.Context, ns string) ([]appsv1.DaemonSet, error) {
	return listAcross(ctx, c, ns, func(n string) ([]appsv1.DaemonSet, error) {
		l, err := c.Clientset.AppsV1().DaemonSets(n).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (c *Client) listPVCsAcross(ctx context.Context, ns string) ([]corev1.PersistentVolumeClaim, error) {
	return listAcross(ctx, c, ns, func(n string) ([]corev1.PersistentVolumeClaim, error) {
		l, err := c.Clientset.CoreV1().PersistentVolumeClaims(n).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (c *Client) listJobsAcross(ctx context.Context, ns string) ([]batchv1.Job, error) {
	return listAcross(ctx, c, ns, func(n string) ([]batchv1.Job, error) {
		l, err := c.Clientset.BatchV1().Jobs(n).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

func (c *Client) listWarningEventsAcross(ctx context.Context, ns string) ([]corev1.Event, error) {
	return listAcross(ctx, c, ns, func(n string) ([]corev1.Event, error) {
		l, err := c.Clientset.CoreV1().Events(n).List(ctx, metav1.ListOptions{FieldSelector: "type=Warning"})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}
