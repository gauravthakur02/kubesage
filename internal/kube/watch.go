package kube

import (
	"context"
	"errors"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
)

// ErrWatchClosed is returned when the API server ends a watch (it does so periodically).
var ErrWatchClosed = errors.New("watch closed by server")

// Watchable reports whether the resource is a real API kind that supports watch.
func (r Resource) Watchable() bool { return !r.keepOrder && r.GVR.Resource != "" }

// WatchChanges calls notify for every add/update/delete of the resource in the client's
// namespace. It blocks until ctx ends (nil) or the watch fails (error).
func (c *Client) WatchChanges(ctx context.Context, r Resource, notify func()) error {
	var ri dynamic.ResourceInterface = c.Dynamic.Resource(r.GVR)
	if r.Namespaced && c.Namespace != AllNamespaces {
		ri = c.Dynamic.Resource(r.GVR).Namespace(c.Namespace)
	}
	// Start from the current state so existing objects are not replayed as ADDED events.
	list, err := ri.List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		return err
	}
	w, err := ri.Watch(ctx, metav1.ListOptions{ResourceVersion: list.GetResourceVersion(), AllowWatchBookmarks: true})
	if err != nil {
		return err
	}
	defer w.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.ResultChan():
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				return ErrWatchClosed
			}
			switch ev.Type {
			case watch.Error:
				return apierrors.FromObject(ev.Object)
			case watch.Bookmark:
				continue
			}
			notify()
		}
	}
}
