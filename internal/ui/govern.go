package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gauravthakur02/kubesage/internal/kube"
)

// openUpgrade checks readiness for the next (or given) Kubernetes version (:upgrade [1.36]).
func (a *App) openUpgrade(target string) tea.Cmd {
	c := a.client
	return a.openReport("upgrade", "checking upgrade readiness…", func(ctx context.Context) (string, string, error) {
		r, err := c.UpgradeReadiness(ctx, target)
		if err != nil {
			return "", "", err
		}
		s := a.st
		var lines []string
		blocking := 0
		for _, f := range r.Findings {
			if f.Blocking {
				blocking++
			}
		}
		head := s.OKText.Render(fmt.Sprintf("✓ no use of APIs removed by %s found", r.Target))
		if blocking > 0 {
			head = s.ErrText.Bold(true).Render(fmt.Sprintf("✗ %d use(s) of APIs removed by %s: fix before upgrading", blocking, r.Target))
		}
		lines = append(lines, head, "")
		if len(r.Findings) > 0 {
			lines = append(lines, s.Title.Render("Deprecated and removed APIs"))
			for _, f := range r.Findings {
				st, tag := s.WarnText, "deprecated"
				if f.Blocking {
					st, tag = s.ErrText, "REMOVED in "+f.API.Removed
				} else if f.API.Removed != "" {
					tag = "removed in " + f.API.Removed
				}
				lines = append(lines, fmt.Sprintf("  %s %s %s  %s  %s %s", st.Render(fmt.Sprintf("%-16s", tag)), f.API.GroupVersion, f.API.Kind,
					s.Muted.Render("→ "+f.API.Replacement), s.YAMLKey.Render(f.Source), f.Object))
			}
			lines = append(lines, "")
		}
		lines = append(lines, s.Title.Render("Versions"), fmt.Sprintf("  control plane %s → target %s", r.Server, r.Target))
		srv := kube.Minor(r.Server)
		pools := map[string]map[string]int{}
		for _, n := range r.Nodes {
			if pools[n.Pool] == nil {
				pools[n.Pool] = map[string]int{}
			}
			pools[n.Pool][n.Kubelet]++
		}
		for pool, vs := range pools {
			for v, n := range vs {
				skew := srv - kube.Minor(v)
				st := s.OKText
				note := ""
				switch {
				case skew > 3 || kube.Minor(r.Target)-kube.Minor(v) > 3:
					st, note = s.ErrText, "  too far behind: upgrade this pool first (kubelet may be at most 3 minors older)"
				case skew > 0:
					st, note = s.WarnText, fmt.Sprintf("  %d minor behind the control plane", skew)
				}
				lines = append(lines, fmt.Sprintf("  pool %-14s %s × %d%s", nonEmptyStr(pool, "-"), st.Render(v), n, s.Muted.Render(note)))
			}
		}
		if len(pools) > 1 || len(r.Nodes) > 0 && len(pools[r.Nodes[0].Pool]) > 1 {
			lines = append(lines, s.WarnText.Render("  ⚠ node versions differ: finish the previous node pool upgrade first"))
		}
		if r.AKS != "" {
			lines = append(lines, "", s.Title.Render("AKS"), "  "+r.AKS)
		}
		if r.MetricsNote != "" {
			r.Notes = append(r.Notes, r.MetricsNote)
		}
		if len(r.Notes) > 0 {
			lines = append(lines, "", s.Muted.Render("Coverage notes:"))
			for _, n := range r.Notes {
				lines = append(lines, s.Muted.Render("  · "+n))
			}
		}
		lines = append(lines, "", s.Muted.Render("Checked: Helm release manifests, kubectl last-applied annotations, and the API server's record of deprecated API calls."),
			s.Muted.Render("Also re-check CRDs and operators (their own APIs) and the release notes of the target version."))
		return fmt.Sprintf("upgrade readiness %s → %s · %d findings · %d blocking", r.Server, r.Target, len(r.Findings), blocking), strings.Join(lines, "\n"), nil
	})
}

// accessNamespace is the namespace for RBAC checks: the focused one, or the selected row's.
func (a *App) accessNamespace() string {
	ns := a.client.Namespace
	if ns == kube.AllNamespaces {
		if row, ok := a.selected(); ok && row.Namespace != "" {
			return row.Namespace
		}
		return "default"
	}
	return ns
}

// openAccess shows what the current user may do in the namespace (:rbac / :access).
func (a *App) openAccess() tea.Cmd {
	c, ns := a.client, a.accessNamespace()
	return a.openReport("rbac", "checking your permissions…", func(ctx context.Context) (string, string, error) {
		cells, err := c.AccessMatrix(ctx, ns)
		if err != nil {
			return "", "", err
		}
		s := a.st
		allowed := map[string]bool{}
		for _, cl := range cells {
			allowed[cl.Resource+"|"+cl.Verb] = cl.Allowed
		}
		head := fmt.Sprintf("  %-26s", "")
		for _, v := range kube.AccessMatrixVerbs {
			head += fmt.Sprintf("%-8s", v)
		}
		lines := []string{s.Title.Render(head)}
		total, yes := 0, 0
		for _, r := range kube.AccessMatrixResources {
			row := fmt.Sprintf("  %-26s", r)
			for _, v := range kube.AccessMatrixVerbs {
				ok, checked := allowed[r+"|"+v]
				switch {
				case !checked:
					row += s.Muted.Render(fmt.Sprintf("%-8s", "·"))
				case ok:
					row += s.OKText.Render(fmt.Sprintf("%-8s", "✓"))
					total, yes = total+1, yes+1
				default:
					row += s.ErrText.Render(fmt.Sprintf("%-8s", "✗"))
					total++
				}
			}
			lines = append(lines, row)
		}
		lines = append(lines, "", s.Muted.Render("Asked the API server (SelfSubjectAccessReview) as you. pods/exec and pods/portforward: create = SPDY, get = WebSocket."),
			s.Muted.Render(":whocan <verb> <resource> lists who else has access · :why explains the last forbidden error"))
		return fmt.Sprintf("your access in %s on %s · %d of %d allowed", ns, c.Context, yes, total), strings.Join(lines, "\n"), nil
	})
}

// openWhoCan lists the subjects allowed to do verb on resource (:whocan get secrets).
func (a *App) openWhoCan(arg string) tea.Cmd {
	f := strings.Fields(arg)
	if len(f) < 2 {
		a.setStatus("usage: :whocan <verb> <resource>   e.g. :whocan get secrets", true)
		return nil
	}
	c, ns, verb, res := a.client, a.accessNamespace(), f[0], f[1]
	return a.openReport("rbac", "resolving role bindings…", func(ctx context.Context) (string, string, error) {
		grants, notes, err := c.WhoCan(ctx, verb, res, ns)
		mine, _, _ := c.CanAccess(ctx, verb, res, ns)
		s := a.st
		var lines []string
		me := s.ErrText.Render("✗ you may not")
		if mine {
			me = s.OKText.Render("✓ you may")
		}
		lines = append(lines, fmt.Sprintf("%s %s %s in %s", me, verb, res, ns), "")
		if err != nil {
			lines = append(lines, s.WarnText.Render("Cannot list the bindings to see who else can: "+err.Error()))
		} else {
			lines = append(lines, s.Title.Render(fmt.Sprintf("%d grants", len(grants))))
			for _, g := range grants {
				lines = append(lines, fmt.Sprintf("  %-50s %s  %s", g.Subject, s.Muted.Render("via "+g.Binding), s.YAMLKey.Render(g.Role)))
			}
			for _, n := range notes {
				lines = append(lines, s.Muted.Render("  · "+n))
			}
		}
		return fmt.Sprintf("who can %s %s in %s", verb, res, ns), strings.Join(lines, "\n"), nil
	})
}

// openWhy explains the most recent "forbidden" error (:why).
func (a *App) openWhy() tea.Cmd {
	msg := a.status
	if !strings.Contains(msg, "forbidden") && strings.Contains(a.table.Note, "forbidden") {
		msg = a.table.Note
	}
	if !strings.Contains(msg, "forbidden") && a.listErr != nil {
		msg = a.listErr.Error()
	}
	verb, res, ns, ok := kube.ParseForbidden(msg)
	if !ok {
		a.setStatus("no forbidden error to explain right now (it must be on the status line or in the table)", true)
		return nil
	}
	if ns == "" {
		ns = a.accessNamespace()
	}
	c := a.client
	return a.openReport("rbac", "explaining…", func(ctx context.Context) (string, string, error) {
		s := a.st
		lines := []string{s.ErrText.Render(fmt.Sprintf("You may not %s %s in %s.", verb, res, nonEmptyStr(ns, "the cluster"))), ""}
		var where []string
		for _, other := range []string{"get", "list", "watch"} {
			if other == verb {
				continue
			}
			if ok, _, _ := c.CanAccess(ctx, other, res, ns); ok {
				where = append(where, other)
			}
		}
		if len(where) > 0 {
			lines = append(lines, fmt.Sprintf("You can %s %s there, so a narrower action may work.", strings.Join(where, "/"), res))
		}
		if grants, _, err := c.WhoCan(ctx, verb, res, ns); err == nil && len(grants) > 0 {
			lines = append(lines, "", s.Title.Render("Who has it"))
			for i, g := range grants {
				if i >= 15 {
					break
				}
				lines = append(lines, fmt.Sprintf("  %-50s %s", g.Subject, s.Muted.Render("via "+g.Binding+" → "+g.Role)))
			}
			lines = append(lines, "", "Ask for one of these groups or roles, or a RoleBinding in "+ns+" granting "+verb+" on "+res+".")
		} else {
			lines = append(lines, "", "Your account cannot read the role bindings, so kubesage cannot list who has this permission.",
				"Ask a cluster admin for a RoleBinding in "+ns+" that grants "+verb+" on "+res+".")
		}
		lines = append(lines, "", s.Muted.Render(":rbac shows everything you can do in this namespace"))
		return "why forbidden: " + verb + " " + res, strings.Join(lines, "\n"), nil
	})
}

// openSAPermissions shows the bindings and rules of a service account (p on serviceaccounts).
func (a *App) openSAPermissions(ns, sa string) tea.Cmd {
	c := a.client
	return a.openReport("rbac", "resolving service account permissions…", func(ctx context.Context) (string, string, error) {
		lines, err := c.ServiceAccountRules(ctx, ns, sa)
		if err != nil {
			return "", "", err
		}
		if len(lines) == 0 {
			lines = []string{"No RoleBinding or ClusterRoleBinding names this service account (it only has the defaults for authenticated service accounts)."}
		}
		return fmt.Sprintf("permissions of serviceaccount %s/%s", ns, sa), strings.Join(lines, "\n"), nil
	})
}

// openNetCheck asks for a target, then tests DNS, TCP and HTTP from inside the pod's network
// using an ephemeral debug container (N on a pod, or :netcheck <target>).
func (a *App) openNetCheck(ns, pod, target string) tea.Cmd {
	if !a.guardWrite() {
		return nil
	}
	if target == "" {
		a.openInput(ovInput, "test from "+pod+" to (service, host:port or URL) › ", func(t string) tea.Cmd { return a.openNetCheck(ns, pod, t) })
		return nil
	}
	script, err := kube.NetCheckScript(target)
	if err != nil {
		a.setStatus(err.Error(), true)
		return nil
	}
	c, img := a.client, a.debugImage()
	pods, _ := kube.Lookup("pods")
	a.askConfirm(fmt.Sprintf("Test %s from pod %s/%s on %s?\nThis adds an ephemeral %s container to the pod (it stays until the pod is replaced).", target, ns, pod, c.Context, img), pod, func() tea.Cmd {
		a.audit("netcheck "+target, c.Context, ns, "pod/"+pod, nil)
		a.target = targetRef{pods, ns, pod}
		return a.openReport("netcheck", "running network checks from "+pod+" (pulling "+img+" if needed)…", func(ctx context.Context) (string, string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			out, err := c.RunEphemeral(ctx, ns, pod, img, script)
			if err != nil {
				return "", "", err
			}
			s := a.st
			var lines []string
			for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
				switch {
				case strings.HasPrefix(l, "=="):
					l = s.Title.Render(l)
				case strings.Contains(l, "FAILED") || strings.Contains(l, "can't resolve") || strings.Contains(l, "NXDOMAIN") || strings.Contains(l, "timed out"):
					l = s.ErrText.Render(l)
				case strings.Contains(l, "OK") || strings.Contains(l, "HTTP/1.1 2") || strings.Contains(l, "HTTP/1.1 3"):
					l = s.OKText.Render(l)
				}
				lines = append(lines, l)
			}
			lines = append(lines, "", s.Muted.Render("P shows the network policies of this pod · a asks the AI to interpret the result"))
			return fmt.Sprintf("network check %s → %s", pod, target), strings.Join(lines, "\n"), nil
		})
	})
	return nil
}
