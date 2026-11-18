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

func TestSecretReadsNeedApproval(t *testing.T) {
	for _, line := range []string{
		"kubectl get secret db -oyaml", "kubectl get secrets.v1 -o json", "kubectl get secret.v1. db --output=jsonpath={.data}",
		"kubectl get sa,secrets -ojson", "kubectl -n shop get secrets/db -o yaml", "kubectl describe secret db", "kubectl get --raw /api/v1/secrets",
	} {
		if c := Parse(line); c.Risk != Write {
			t.Errorf("%q = %v, want approval", line, c.Risk)
		}
	}
	for _, line := range []string{"kubectl get secrets", "kubectl get pods -o wide", "kubectl get secret-store-config -o yaml"} {
		if c := Parse(line); c.Risk != Read {
			t.Errorf("%q = %v, want read-only", line, c.Risk)
		}
	}
}

func TestRedactOutput(t *testing.T) {
	yaml := "spec:\n  containers:\n  - env:\n    - name: DB_PASSWORD\n      value: hunter2\n    - name: LOG_LEVEL\n      value: debug\n"
	out := RedactOutput(yaml)
	if strings.Contains(out, "hunter2") || !strings.Contains(out, "debug") {
		t.Fatalf("yaml env:\n%s", out)
	}
	js := `{"env":[{"name":"API_TOKEN","value":"abc\"def"},{"name":"MODE","value":"prod"}]}`
	if out := RedactOutput(js); strings.Contains(out, "abc") || !strings.Contains(out, `"prod"`) {
		t.Fatalf("json env: %s", out)
	}
	sec := "apiVersion: v1\ndata:\n  password: aHVudGVyMg==\n  user: YWRtaW4=\nkind: Secret\nmetadata:\n  name: db\n"
	if out := RedactOutput(sec); strings.Contains(out, "aHVudGVyMg") || strings.Contains(out, "YWRtaW4") || !strings.Contains(out, "name: db") {
		t.Fatalf("secret data:\n%s", out)
	}
	if out := RedactOutput("DATABASE_PASSWORD=s3cret\nPORT=8080"); strings.Contains(out, "s3cret") || !strings.Contains(out, "8080") {
		t.Fatalf("kv: %s", out)
	}
	if !strings.Contains(FormatResults([]Result{{Command: Parse("kubectl get pods -o yaml"), Output: yaml}}), "<redacted>") {
		t.Fatal("FormatResults does not redact")
	}
}

func TestIdentityFlagsStripped(t *testing.T) {
	for _, line := range []string{
		"kubectl get pods --as-uid=1000", "kubectl get pods --username=alice --password=s3cret", "kubectl get pods --username alice --password s3cret",
		"kubectl get pods --client-certificate=/tmp/c --client-key /tmp/k", "kubectl get pods --insecure-skip-tls-verify -n shop",
		"kubectl --as admin get pods", "kubectl get pods --certificate-authority=/x --tls-server-name=evil",
	} {
		c := Parse(line)
		d := c.Display()
		for _, bad := range []string{"--as", "--username", "--password", "s3cret", "--client-", "--insecure", "--certificate", "--tls-server", "admin", "alice"} {
			if strings.Contains(d, bad) {
				t.Errorf("%q kept %q: %s", line, bad, d)
			}
		}
		if !strings.Contains(d, "get pods") {
			t.Errorf("%q lost the command: %s", line, d)
		}
	}
	if d := Parse("kubectl get pods --insecure-skip-tls-verify -n shop").Display(); !strings.Contains(d, "-n shop") {
		t.Errorf("boolean flag swallowed the next argument: %s", d)
	}
}
