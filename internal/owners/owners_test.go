package owners

import "testing"

func TestResolve(t *testing.T) {
	rules := []Rule{{Team: "payments", Namespaces: "^pay-"}, {Team: "checkout", Label: "app.kubernetes.io/part-of=checkout", Contact: "#checkout"}}
	if o, ok := Resolve(rules, "pay-prod", nil, nil); !ok || o.Team != "payments" || o.Source != "rule" {
		t.Fatalf("namespace rule: %+v", o)
	}
	if o, _ := Resolve(rules, "shop", map[string]string{"app.kubernetes.io/part-of": "checkout"}, nil); o.Team != "checkout" || o.Contact != "#checkout" {
		t.Fatalf("label rule: %+v", o)
	}
	if o, _ := Resolve(nil, "shop", map[string]string{"team": "web"}, nil); o.Team != "web" || o.Source != "label team" {
		t.Fatalf("label: %+v", o)
	}
	if o, _ := Resolve(nil, "shop", nil, map[string]string{"owner": "alice"}); o.Team != "alice" {
		t.Fatalf("annotation: %+v", o)
	}
	if _, ok := Resolve(nil, "shop", nil, nil); ok {
		t.Fatal("owner invented")
	}
}
