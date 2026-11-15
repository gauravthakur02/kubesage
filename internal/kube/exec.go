package kube

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

// ShellCommand starts the best shell available in the container: bash, then ash, then sh.
var ShellCommand = []string{"sh", "-c",
	"command -v bash >/dev/null 2>&1 && exec bash -l; command -v ash >/dev/null 2>&1 && exec ash; exec sh"}

// ExecOptions describes a command to run inside a container.
type ExecOptions struct {
	Namespace, Pod, Container string
	Command                   []string
	TTY                       bool
	Stdin                     io.Reader
	Stdout, Stderr            io.Writer
	SizeQueue                 remotecommand.TerminalSizeQueue
}

// Exec runs a command in a container through the API server, like `kubectl exec`, without needing kubectl.
// It tries the WebSocket protocol first (GET; Kubernetes 1.30+) and falls back to SPDY (POST).
func (c *Client) Exec(ctx context.Context, o ExecOptions) error {
	if c.Rest == nil {
		return fmt.Errorf("exec needs a live cluster connection")
	}
	req := c.Clientset.CoreV1().RESTClient().Post().Resource("pods").Namespace(o.Namespace).Name(o.Pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: o.Container, Command: o.Command,
			Stdin: o.Stdin != nil, Stdout: o.Stdout != nil, Stderr: o.Stderr != nil && !o.TTY, TTY: o.TTY,
		}, scheme.ParameterCodec)
	spdy, err := remotecommand.NewSPDYExecutor(c.Rest, http.MethodPost, req.URL())
	if err != nil {
		return err
	}
	exec := spdy
	if ws, err := remotecommand.NewWebSocketExecutor(c.Rest, http.MethodGet, req.URL().String()); err == nil {
		exec, _ = remotecommand.NewFallbackExecutor(ws, spdy, func(err error) bool {
			return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
		})
	}
	opts := remotecommand.StreamOptions{Stdin: o.Stdin, Stdout: o.Stdout, Tty: o.TTY, TerminalSizeQueue: o.SizeQueue}
	if !o.TTY {
		opts.Stderr = o.Stderr
	}
	return exec.StreamWithContext(ctx, opts)
}

// NoShell reports whether an exec error means the image has no shell (distroless/scratch images).
func NoShell(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "executable file not found") || strings.Contains(s, "no such file or directory")
}

// CanI asks the API server whether the current user may perform verb on resource[/subresource]
// in ns (a SelfSubjectAccessReview: read-only, allowed for every authenticated user).
func (c *Client) CanI(ctx context.Context, verb, resource, subresource, ns string) (bool, string, error) {
	r, err := c.Clientset.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authv1.SelfSubjectAccessReview{
		Spec: authv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authv1.ResourceAttributes{
			Namespace: ns, Verb: verb, Resource: resource, Subresource: subresource}},
	}, metav1.CreateOptions{})
	if err != nil {
		return false, "", err
	}
	return r.Status.Allowed, r.Status.Reason, nil
}
