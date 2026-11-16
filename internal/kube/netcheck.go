package kube

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NetCheckScript builds a busybox-compatible shell script testing DNS, TCP and HTTP to target
// ("svc", "svc.ns:8080", "10.0.0.5:5432" or "http(s)://host[:port]/path").
func NetCheckScript(target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" || strings.ContainsAny(target, "'\"`$;&|<>\\\n") {
		return "", fmt.Errorf("invalid target %q", target)
	}
	host, port, httpURL := target, "", ""
	if strings.Contains(target, "://") {
		u, err := url.Parse(target)
		if err != nil {
			return "", err
		}
		host, port, httpURL = u.Hostname(), u.Port(), target
		if port == "" {
			port = map[string]string{"https": "443"}[u.Scheme]
			if port == "" {
				port = "80"
			}
		}
	} else if h, p, ok := strings.Cut(target, ":"); ok {
		host, port = h, p
		if p == "80" || p == "8080" || p == "8000" || p == "3000" || p == "9090" {
			httpURL = "http://" + target + "/"
		}
	}
	var b strings.Builder
	b.WriteString("echo '== resolv.conf'; cat /etc/resolv.conf 2>&1 | grep -v '^#'; echo\n")
	fmt.Fprintf(&b, "echo '== DNS %s'; nslookup %s 2>&1; echo\n", host, host)
	if port != "" {
		fmt.Fprintf(&b, "echo '== TCP %s:%s'; if nc -z -w 3 %s %s 2>&1; then echo 'TCP OK: port open'; else echo 'TCP FAILED: closed, filtered or unreachable'; fi; echo\n", host, port, host, port)
	}
	if httpURL != "" {
		fmt.Fprintf(&b, "echo '== HTTP %s'; wget -q -S -O /dev/null -T 5 %s 2>&1 | head -15 || echo 'HTTP FAILED'; echo\n", httpURL, httpURL)
	}
	b.WriteString("echo '== done'\n")
	return b.String(), nil
}

// RunEphemeral adds an ephemeral container running script to a pod (it shares the pod's network),
// waits for it to finish and returns its output. Ephemeral containers stay on the pod until it is replaced.
func (c *Client) RunEphemeral(ctx context.Context, ns, pod, image, script string) (string, error) {
	p, err := c.Clientset.CoreV1().Pods(ns).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("kubesage-net-%d", time.Now().Unix()%100000)
	p.Spec.EphemeralContainers = append(p.Spec.EphemeralContainers, corev1.EphemeralContainer{EphemeralContainerCommon: corev1.EphemeralContainerCommon{
		Name: name, Image: image, ImagePullPolicy: corev1.PullIfNotPresent, Command: []string{"sh", "-c", script},
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError}})
	if _, err := c.Clientset.CoreV1().Pods(ns).UpdateEphemeralContainers(ctx, pod, p, metav1.UpdateOptions{}); err != nil {
		return "", err
	}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
		cur, err := c.Clientset.CoreV1().Pods(ns).Get(ctx, pod, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		for _, s := range cur.Status.EphemeralContainerStatuses {
			if s.Name != name {
				continue
			}
			if w := s.State.Waiting; w != nil && w.Reason != "" && w.Reason != "ContainerCreating" && w.Reason != "PodInitializing" {
				return "", fmt.Errorf("debug container %s: %s %s", w.Reason, w.Message, "(check debug_image and registry access)")
			}
			if s.State.Terminated != nil {
				return c.LogsText(ctx, ns, pod, LogOptions{Container: name})
			}
		}
	}
	return "", fmt.Errorf("debug container did not finish within 90s")
}
