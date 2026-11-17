package kube

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MaxLogBytes caps one container's downloaded log (a runaway logger must not fill the disk).
const MaxLogBytes = 200 << 20

// WriteLogs copies a container's complete log (or the part newer than o.Since) to w and returns the bytes written.
func (c *Client) WriteLogs(ctx context.Context, w io.Writer, ns, pod string, o LogOptions) (int64, error) {
	o.Follow, o.Tail, o.All = false, 0, true // everything the kubelet still has
	rc, err := c.Logs(ctx, ns, pod, o)
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	n, err := io.Copy(w, io.LimitReader(rc, MaxLogBytes))
	if n == MaxLogBytes {
		fmt.Fprintf(w, "\n[kubesage] truncated at %d MiB\n", MaxLogBytes>>20)
	}
	return n, err
}

// LogBundleResult says what went into a bundle.
type LogBundleResult struct {
	Pods, Files int
	Bytes       int64
	Skipped     []string
}

// WriteLogBundle writes a zip with the logs of every pod and container of an object (a pod, Deployment, StatefulSet,
// DaemonSet, Job, …): <pod>/<container>.log, plus <container>.previous.log for containers that restarted, and a
// SUMMARY.txt with pod states and restart counts.
func (c *Client) WriteLogBundle(ctx context.Context, w io.Writer, r Resource, ns, name string, since time.Duration, timestamps bool) (LogBundleResult, error) {
	pods, err := c.PodsFor(ctx, r, ns, name)
	if err != nil {
		return LogBundleResult{}, err
	}
	if len(pods) == 0 {
		return LogBundleResult{}, fmt.Errorf("no pods for %s %s/%s", strings.TrimSuffix(r.Name, "s"), ns, name)
	}
	return c.writeBundle(ctx, w, pods, ns, strings.TrimSuffix(r.Name, "s")+" "+ns+"/"+name, since, timestamps)
}

// WriteLogBundleForPods is WriteLogBundle for named pods (a merged log stream).
func (c *Client) WriteLogBundleForPods(ctx context.Context, w io.Writer, ns string, names []string, since time.Duration) (LogBundleResult, error) {
	var pods []corev1.Pod
	for _, n := range names {
		p, err := c.Clientset.CoreV1().Pods(ns).Get(ctx, n, metav1.GetOptions{})
		if err != nil {
			continue // the pod may be gone by now
		}
		pods = append(pods, *p)
	}
	if len(pods) == 0 {
		return LogBundleResult{}, fmt.Errorf("none of the %d pods exist any more", len(names))
	}
	return c.writeBundle(ctx, w, pods, ns, fmt.Sprintf("%d pods in %s", len(pods), ns), since, true)
}

func (c *Client) writeBundle(ctx context.Context, w io.Writer, pods []corev1.Pod, ns, what string, since time.Duration, timestamps bool) (LogBundleResult, error) {
	var res LogBundleResult
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	zw := zip.NewWriter(w)
	var summary strings.Builder
	fmt.Fprintf(&summary, "kubesage log bundle\ncontext:   %s\nobject:    %s\ncollected: %s\nwindow:    %s\n\n",
		c.Context, what, time.Now().UTC().Format(time.RFC3339), windowLabel(since))
	add := func(path string, o LogOptions, pod string) {
		f, err := zw.CreateHeader(&zip.FileHeader{Name: path, Method: zip.Deflate, Modified: time.Now()})
		if err != nil {
			res.Skipped = append(res.Skipped, path+": "+err.Error())
			return
		}
		n, err := c.WriteLogs(ctx, f, ns, pod, o)
		res.Bytes += n
		res.Files++
		if err != nil {
			fmt.Fprintf(f, "\n[kubesage] could not read this log: %v\n", err)
			res.Skipped = append(res.Skipped, path+": "+firstLine(err.Error()))
		}
	}
	for _, p := range pods {
		res.Pods++
		fmt.Fprintf(&summary, "%s  phase=%s node=%s\n", p.Name, p.Status.Phase, p.Spec.NodeName)
		restarts := map[string]int32{}
		for _, s := range append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
			restarts[s.Name] = s.RestartCount
			state := "running"
			switch {
			case s.State.Waiting != nil:
				state = "waiting: " + s.State.Waiting.Reason
			case s.State.Terminated != nil:
				state = "terminated: " + s.State.Terminated.Reason
			}
			fmt.Fprintf(&summary, "  %-24s restarts=%d %s\n", s.Name, s.RestartCount, state)
		}
		for _, ct := range Containers(&p) {
			o := LogOptions{Container: ct, Since: since, Timestamps: timestamps}
			add(p.Name+"/"+ct+".log", o, p.Name)
			if restarts[ct] > 0 {
				o.Previous = true
				add(p.Name+"/"+ct+".previous.log", o, p.Name)
			}
		}
	}
	if len(res.Skipped) > 0 {
		fmt.Fprintf(&summary, "\nnot readable:\n  %s\n", strings.Join(res.Skipped, "\n  "))
	}
	if f, err := zw.Create("SUMMARY.txt"); err == nil {
		_, _ = io.WriteString(f, summary.String())
	}
	return res, zw.Close()
}

func windowLabel(d time.Duration) string {
	if d <= 0 {
		return "everything the kubelet still has"
	}
	return "last " + d.String()
}
