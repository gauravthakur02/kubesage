package kube

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// PortTarget is a port that can be forwarded for an object.
type PortTarget struct {
	Pod        string // resolved pod that will receive traffic
	Namespace  string
	Port       int    // port on the pod (container port)
	ExposedAs  int    // port as named by the object (service port), for display
	Name       string // port name, if any
	Protocol   string
	ObjectDesc string // e.g. "svc/checkout"
	Resource   string // resource the user picked, e.g. "services"
	Object     string // its name
}

// Label renders a target for pickers.
func (t PortTarget) Label() string {
	s := fmt.Sprintf("%d", t.ExposedAs)
	if t.ExposedAs != t.Port {
		s += fmt.Sprintf(" → pod %d", t.Port)
	}
	if t.Name != "" {
		s += " (" + t.Name + ")"
	}
	return s
}

// PortTargets resolves forwardable ports for pods, services and pod-owning workloads.
func (c *Client) PortTargets(ctx context.Context, r Resource, ns, name string) ([]PortTarget, error) {
	if r.Name == "services" {
		return c.serviceTargets(ctx, ns, name)
	}
	pods, err := c.PodsFor(ctx, r, ns, name)
	if err != nil {
		return nil, err
	}
	pod := pickReadyPod(pods)
	if pod == nil {
		return nil, fmt.Errorf("no running pod found for %s/%s", r.Name, name)
	}
	desc := strings.TrimSuffix(r.Name, "s") + "/" + name
	var out []PortTarget
	for _, ctr := range pod.Spec.Containers {
		for _, p := range ctr.Ports {
			if p.Protocol != "" && p.Protocol != corev1.ProtocolTCP {
				continue
			}
			out = append(out, PortTarget{Pod: pod.Name, Namespace: ns, Port: int(p.ContainerPort), ExposedAs: int(p.ContainerPort),
				Name: p.Name, Protocol: "TCP", ObjectDesc: desc, Resource: r.Name, Object: name})
		}
	}
	if len(out) == 0 {
		// No declared ports: still allow forwarding, the user types the port.
		out = append(out, PortTarget{Pod: pod.Name, Namespace: ns, ObjectDesc: desc, Resource: r.Name, Object: name})
	}
	return out, nil
}

func (c *Client) serviceTargets(ctx context.Context, ns, name string) ([]PortTarget, error) {
	svc, err := c.Clientset.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if len(svc.Spec.Selector) == 0 {
		return nil, fmt.Errorf("service %s has no selector (headless/external); forward a pod instead", name)
	}
	list, err := c.Clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(svc.Spec.Selector).String()})
	if err != nil {
		return nil, err
	}
	pod := pickReadyPod(list.Items)
	if pod == nil {
		return nil, fmt.Errorf("service %s has no running pods", name)
	}
	var out []PortTarget
	for _, sp := range svc.Spec.Ports {
		if sp.Protocol != "" && sp.Protocol != corev1.ProtocolTCP {
			continue
		}
		port, err := resolveTargetPort(pod, sp.TargetPort, int(sp.Port))
		if err != nil {
			continue
		}
		out = append(out, PortTarget{Pod: pod.Name, Namespace: ns, Port: port, ExposedAs: int(sp.Port), Name: sp.Name,
			Protocol: "TCP", ObjectDesc: "svc/" + name, Resource: "services", Object: name})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("service %s exposes no TCP ports", name)
	}
	return out, nil
}

func resolveTargetPort(pod *corev1.Pod, tp intstr.IntOrString, servicePort int) (int, error) {
	switch {
	case tp.Type == intstr.Int && tp.IntVal != 0:
		return int(tp.IntVal), nil
	case tp.Type == intstr.String && tp.StrVal != "":
		for _, c := range pod.Spec.Containers {
			for _, p := range c.Ports {
				if p.Name == tp.StrVal {
					return int(p.ContainerPort), nil
				}
			}
		}
		return 0, fmt.Errorf("named port %q not found on pod %s", tp.StrVal, pod.Name)
	}
	return servicePort, nil
}

func pickReadyPod(pods []corev1.Pod) *corev1.Pod {
	var running *corev1.Pod
	for i := range pods {
		p := &pods[i]
		if p.Status.Phase != corev1.PodRunning || p.DeletionTimestamp != nil {
			continue
		}
		ready := true
		for _, cs := range p.Status.ContainerStatuses {
			ready = ready && cs.Ready
		}
		if ready {
			return p
		}
		if running == nil {
			running = p
		}
	}
	return running
}

// Forward is one active port-forward session.
type Forward struct {
	ID         int
	Context    string
	Namespace  string
	Pod        string
	ObjectDesc string
	Resource   string
	Object     string
	Exposed    int // port as named by the object (service port); equals Remote for pods
	Local      int
	Remote     int
	Started    time.Time

	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	err    error
}

// Stop ends the forward.
func (f *Forward) Stop() { f.cancel() }

// Err returns why the forward ended (nil while running or after a clean stop).
func (f *Forward) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

// Running reports whether the forward is still active.
func (f *Forward) Running() bool {
	select {
	case <-f.done:
		return false
	default:
		return true
	}
}

// String renders the forward for lists and status lines.
func (f *Forward) String() string {
	s := fmt.Sprintf("localhost:%d → %s:%d", f.Local, f.ObjectDesc, f.exposed())
	if f.exposed() != f.Remote {
		s += fmt.Sprintf(" (pod :%d)", f.Remote)
	}
	if f.ObjectDesc != "pod/"+f.Pod {
		s += "  (pod " + f.Pod + ")"
	}
	return s
}

func (f *Forward) exposed() int {
	if f.Exposed > 0 {
		return f.Exposed
	}
	return f.Remote
}

// LocalPortFree reports whether nothing on this machine uses port on localhost.
// It checks both that something does not already answer (a server bound to
// "*:port" does not block binding 127.0.0.1:port on macOS, but would shadow
// http://localhost:port) and that the port can be bound on 127.0.0.1 and ::1.
func LocalPortFree(port int) bool {
	for _, host := range []string{"127.0.0.1", "::1"} {
		if conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 150*time.Millisecond); err == nil {
			conn.Close()
			return false
		}
	}
	for _, host := range []string{"127.0.0.1", "::1"} {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			if host == "::1" && !strings.Contains(err.Error(), "address already in use") {
				continue // IPv6 loopback not available on this machine
			}
			return false
		}
		ln.Close()
	}
	return true
}

// SuggestLocalPort returns preferred if free, else the next free port above it.
func SuggestLocalPort(preferred int) int {
	if preferred <= 0 {
		return 0
	}
	for p := preferred; p < preferred+100 && p <= 65535; p++ {
		if LocalPortFree(p) {
			return p
		}
	}
	return 0
}

// URL is the local address to open in a browser.
func (f *Forward) URL() string {
	scheme := "http"
	if f.Remote == 443 || f.Remote == 8443 {
		scheme = "https"
	}
	return fmt.Sprintf("%s://localhost:%d", scheme, f.Local)
}

// ForwardsFor returns running forwards that target an object in a context.
// A pod also matches forwards that reach it through a service or workload,
// reported with direct=false.
func ForwardsFor(kctx, resource, ns, name string) (direct, indirect []*Forward) {
	for _, f := range Forwards() {
		if !f.Running() || f.Context != kctx || f.Namespace != ns {
			continue
		}
		switch {
		case f.Resource == resource && f.Object == name:
			direct = append(direct, f)
		case resource == "pods" && f.Pod == name:
			indirect = append(indirect, f)
		}
	}
	return direct, indirect
}

// RegisterForward adds an externally created forward (used by tests).
func RegisterForward(f *Forward) {
	fwMu.Lock()
	defer fwMu.Unlock()
	if f.done == nil {
		f.done = make(chan struct{})
	}
	if f.cancel == nil {
		done := f.done
		var once sync.Once
		f.cancel = func() { once.Do(func() { close(done) }) }
	}
	f.ID = fwNextID
	fwNextID++
	forwards = append(forwards, f)
}

var (
	fwMu     sync.Mutex
	fwNextID = 1
	forwards []*Forward
)

// Forwards returns all forwards started in this process, newest last.
func Forwards() []*Forward {
	fwMu.Lock()
	defer fwMu.Unlock()
	out := append([]*Forward(nil), forwards...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// RemoveForward stops and forgets a forward.
func RemoveForward(id int) {
	fwMu.Lock()
	defer fwMu.Unlock()
	for i, f := range forwards {
		if f.ID == id {
			f.Stop()
			forwards = append(forwards[:i], forwards[i+1:]...)
			return
		}
	}
}

// StopAllForwards is called on exit.
func StopAllForwards() {
	for _, f := range Forwards() {
		f.Stop()
	}
}

// PortForward starts forwarding localhost:local to pod:remote. local=0 picks a free port.
// It returns once the listener is ready (or failed).
func (c *Client) PortForward(t PortTarget, local, remote int) (*Forward, error) {
	if c.Rest == nil {
		return nil, fmt.Errorf("port-forward needs a live cluster connection")
	}
	if local > 0 && !LocalPortFree(local) {
		alt := SuggestLocalPort(local + 1)
		return nil, fmt.Errorf("local port %d is already used by another program on this machine; try %d", local, alt)
	}
	req := c.Clientset.CoreV1().RESTClient().Post().Resource("pods").Namespace(t.Namespace).Name(t.Pod).SubResource("portforward")
	transport, upgrader, err := spdy.RoundTripperFor(c.Rest)
	if err != nil {
		return nil, err
	}
	var dialer httpstream.Dialer = spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, req.URL())
	// Prefer websockets (Kubernetes 1.30+), falling back to SPDY like kubectl does.
	if ws, err := portforward.NewSPDYOverWebsocketDialer(req.URL(), c.Rest); err == nil {
		dialer = portforward.NewFallbackDialer(ws, dialer, func(err error) bool {
			return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	pf, err := portforward.NewOnAddressesWithContext(ctx, dialer, []string{"localhost"},
		[]string{fmt.Sprintf("%d:%d", local, remote)}, ready, io.Discard, io.Discard)
	if err != nil {
		cancel()
		return nil, err
	}
	f := &Forward{Context: c.Context, Namespace: t.Namespace, Pod: t.Pod, ObjectDesc: t.ObjectDesc, Resource: t.Resource, Object: t.Object,
		Exposed: t.ExposedAs,
		Local:   local, Remote: remote,
		Started: time.Now(), cancel: cancel, done: make(chan struct{})}
	if f.ObjectDesc == "" {
		f.ObjectDesc = "pod/" + t.Pod
	}
	if f.Resource == "" {
		f.Resource, f.Object = "pods", t.Pod
	}
	errc := make(chan error, 1)
	go func() {
		err := pf.ForwardPorts()
		f.mu.Lock()
		if ctx.Err() == nil {
			f.err = err
			if f.err == nil {
				f.err = fmt.Errorf("connection to pod closed")
			}
		}
		f.mu.Unlock()
		close(f.done)
		errc <- err
	}()
	select {
	case <-ready:
	case err := <-errc:
		cancel()
		if err == nil {
			err = fmt.Errorf("port-forward ended before it was ready")
		}
		return nil, err
	case <-time.After(20 * time.Second):
		cancel()
		return nil, fmt.Errorf("timed out starting port-forward")
	}
	if ports, err := pf.GetPorts(); err == nil && len(ports) > 0 {
		f.Local = int(ports[0].Local)
	}
	fwMu.Lock()
	f.ID = fwNextID
	fwNextID++
	forwards = append(forwards, f)
	fwMu.Unlock()
	return f, nil
}
