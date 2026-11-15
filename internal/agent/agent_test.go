package agent

import (
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := map[string]Risk{
		"kubectl get pods -n shop -o wide":                                  Read,
		"kubectl describe deploy api -n shop":                               Read,
		"kubectl logs deploy/api -n shop --tail=50 --previous":              Read,
		"kubectl top pods -A":                                               Read,
		"kubectl rollout status deploy/api -n shop":                         Read,
		"kubectl auth can-i delete pods -n shop":                            Read,
		"kubectl get secret db -n shop -o yaml":                             Write,
		"kubectl describe secrets -n shop":                                  Write,
		"kubectl get secrets -n shop":                                       Read,
		"kubectl rollout restart deploy/api -n shop":                        Write,
		"kubectl rollout undo deploy/api -n shop":                           Write,
		"kubectl scale deploy api --replicas=3 -n shop":                     Write,
		"kubectl delete pod api-1 -n shop":                                  Write,
		`kubectl patch deploy api -n shop -p '{"spec":{"replicas":2}}'`:     Write,
		"kubectl apply -f fix.yaml --dry-run=server":                        Read,
		"kubectl exec api-1 -n shop -- env":                                 Write,
		"kubectl exec -it api-1 -- sh":                                      Blocked,
		"kubectl logs -f api-1":                                             Blocked,
		"kubectl get pods -w":                                               Blocked,
		"kubectl port-forward svc/api 8080:80":                              Blocked,
		"kubectl edit deploy api":                                           Blocked,
		"kubectl get pods | grep api":                                       Blocked,
		"kubectl get pods; kubectl delete ns shop":                          Blocked,
		"kubectl get pods $(whoami)":                                        Blocked,
		"kubectl apply -f fix.yaml --dry-run=none":                          Write,
		"kubectl get --raw /api/v1/namespaces/shop/secrets":                 Write,
		"kubectl frobnicate":                                                Write,
		"rm -rf /":                                                          Blocked,
		"kubectl drain node-1 --ignore-daemonsets":                          Write,
		"kubectl --context prod-cluster delete pod x":                       Write,
		"kubectl get pods --context=prod-cluster --kubeconfig /tmp/x -n ns": Read,
	}
	for line, want := range cases {
		if got := Parse(line); got.Risk != want {
			t.Errorf("%-70s got %v (%s), want %v", line, got.Risk, got.Reason, want)
		}
	}
}

func TestTargetingFlagsStripped(t *testing.T) {
	c := Parse("kubectl --context prod get pods --kubeconfig=/x --as admin -n shop")
	if got := c.Display(); got != "kubectl get pods -n shop" {
		t.Fatalf("got %q", got)
	}
}

func TestExtract(t *testing.T) {
	reply := "Let me look.\n```run\nkubectl get pods -n shop\n$ kubectl logs api-1 -n shop \\\n  --tail=20\nkubectl get pods -n shop\n```\n" +
		"Fix:\n```bash\nkubectl rollout restart deploy/api -n shop\necho done\n```"
	run := ExtractRun(reply)
	if len(run) != 2 || run[1].Display() != "kubectl logs api-1 -n shop --tail=20" {
		t.Fatalf("run = %+v", run)
	}
	sug := ExtractSuggested(reply)
	if len(sug) != 3 || sug[2].Risk != Write {
		t.Fatalf("suggested = %+v", sug)
	}
}

func TestFormatResults(t *testing.T) {
	out := FormatResults([]Result{
		{Command: Parse("kubectl get pods"), Output: "NAME READY\napi 1/1\n"},
		{Command: Parse("kubectl delete pod api"), Declined: true},
		{Command: Parse("kubectl get pods | wc")},
	})
	for _, want := range []string{"$ kubectl get pods", "api 1/1", "user declined", "not executed: shell operators"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}
