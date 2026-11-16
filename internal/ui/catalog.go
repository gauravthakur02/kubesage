package ui

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"gopkg.in/yaml.v3"

	"github.com/gauravthakur02/kubesage/internal/config"
	"github.com/gauravthakur02/kubesage/internal/kube"
)

// DefaultPluginIndex is the community plugin catalogue in the kubesage repository.
const DefaultPluginIndex = "https://raw.githubusercontent.com/gauravthakur02/kubesage/main/plugins/index.yaml"

const catalogRes = "plugin-catalog"

type catalogEntry struct {
	Name        string        `yaml:"name"`
	Description string        `yaml:"description"`
	Requires    string        `yaml:"requires,omitempty"` // a tool that must be on PATH
	Plugin      config.Plugin `yaml:"plugin"`
}

func (a *App) pluginIndexURL() string {
	if a.cfg.PluginIndex != "" {
		return a.cfg.PluginIndex
	}
	return DefaultPluginIndex
}

func loadCatalog(ctx context.Context, src string) ([]catalogEntry, error) {
	var b []byte
	var err error
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
		req.Header.Set("User-Agent", "kubesage-plugins")
		resp, rerr := http.DefaultClient.Do(req)
		if rerr != nil {
			return nil, rerr
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: %s", src, resp.Status)
		}
		b, err = io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	} else {
		b, err = os.ReadFile(src)
	}
	if err != nil {
		return nil, err
	}
	var idx struct {
		Plugins []catalogEntry `yaml:"plugins"`
	}
	if err := yaml.Unmarshal(b, &idx); err != nil {
		return nil, fmt.Errorf("plugin index: %w", err)
	}
	return idx.Plugins, nil
}

func (a *App) catalogResource() kube.Resource {
	src, installed := a.pluginIndexURL(), map[string]bool{}
	for _, p := range a.cfg.Plugins {
		installed[p.Name] = true
	}
	return kube.LocalResource(catalogRes, "Plugin", "Plugins", func() kube.Table {
		t := kube.Table{Columns: []string{"NAME", "KEY", "FOR", "STATE", "DESCRIPTION", "RUNS"}}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		entries, err := loadCatalog(ctx, src)
		if err != nil {
			t.Note = "✗ " + err.Error()
			return t
		}
		for _, e := range entries {
			state, h := "available", kube.HealthNeutral
			if installed[e.Plugin.Name] {
				state, h = "installed", kube.HealthOK
			} else if e.Requires != "" && !onPath(e.Requires) {
				state, h = "needs "+e.Requires, kube.HealthWarn
			}
			t.Rows = append(t.Rows, kube.Row{Name: e.Name, Health: h, Cells: []string{e.Name, e.Plugin.Key, strings.Join(e.Plugin.Scopes, ","), state, e.Description,
				strings.TrimSpace(e.Plugin.Command + " " + strings.Join(e.Plugin.Args, " "))}})
		}
		t.Note = fmt.Sprintf("%d plugins in %s · enter install · D uninstall", len(entries), src)
		return t
	})
}

func (a *App) installPlugin(row kube.Row, remove bool) tea.Cmd {
	if remove {
		var keep []config.Plugin
		for _, p := range a.cfg.Plugins {
			if p.Name != row.Name {
				keep = append(keep, p)
			}
		}
		a.cfg.Plugins = keep
		if err := a.cfg.Save(); err != nil {
			a.setStatus("save: "+err.Error(), true)
			return nil
		}
		a.setStatus("✓ removed plugin "+row.Name, false)
		return a.refresh()
	}
	src := a.pluginIndexURL()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		entries, err := loadCatalog(ctx, src)
		if err != nil {
			return actionMsg{status: "install plugin", err: err}
		}
		for _, e := range entries {
			if e.Name == row.Name {
				return pluginChosenMsg{e: e}
			}
		}
		return actionMsg{status: "install plugin", err: fmt.Errorf("%s not in the index", row.Name)}
	}
}

type pluginChosenMsg struct{ e catalogEntry }

// handlePluginChosen shows exactly what will run before saving the plugin to the config.
func (a *App) handlePluginChosen(m pluginChosenMsg) tea.Cmd {
	p := m.e.Plugin
	for _, x := range a.cfg.Plugins {
		if x.Key == p.Key {
			a.setStatus(fmt.Sprintf("key %s is already used by plugin %s", p.Key, x.Name), true)
			return nil
		}
	}
	cmd := strings.TrimSpace(p.Command + " " + strings.Join(p.Args, " "))
	a.askConfirmPlain(fmt.Sprintf("Install plugin %s? Key %s on %s will run on your computer: %s (y/n)", p.Name, p.Key, strings.Join(p.Scopes, ","), cmd), func() tea.Cmd {
		a.cfg.Plugins = append(a.cfg.Plugins, p)
		if err := a.cfg.Save(); err != nil {
			a.setStatus("save: "+err.Error(), true)
			return nil
		}
		a.setStatus("✓ installed plugin "+p.Name+" on key "+p.Key, false)
		return a.refresh()
	})
	return nil
}

func init() {
	registerScreen(catalogRes, screenDef{
		title: func(a *App) string { return "Plugin catalogue · " + a.table.Note },
		hints: []kv{{"enter", "install"}, {"D", "uninstall"}, {"/", "filter"}, {"esc", "back"}},
		action: func(a *App, key string, row kube.Row) (tea.Cmd, bool) {
			switch key {
			case "enter":
				return a.installPlugin(row, false), true
			case "D":
				return a.installPlugin(row, true), true
			}
			return nil, false
		},
	})
}

func onPath(tool string) bool {
	_, err := exec.LookPath(tool)
	return err == nil
}
