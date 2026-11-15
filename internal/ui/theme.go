package ui

import (
	"sort"

	"github.com/charmbracelet/lipgloss"

	"kubesage/internal/kube"
)

// Theme is a palette. Colours are hex; lipgloss degrades them automatically
// to 256/16 colours on terminals (or Windows consoles) that lack truecolor.
type Theme struct {
	Name                                      string
	Bg, Fg, Muted, Subtle, Accent, Accent2    lipgloss.Color
	OK, Warn, Err, Info, SelBg, SelFg, Border lipgloss.Color
}

var themes = map[string]Theme{
	"tokyonight": {Name: "tokyonight", Bg: "#1a1b26", Fg: "#c0caf5", Muted: "#565f89", Subtle: "#292e42", Accent: "#7aa2f7",
		Accent2: "#bb9af7", OK: "#9ece6a", Warn: "#e0af68", Err: "#f7768e", Info: "#7dcfff", SelBg: "#364a82", SelFg: "#ffffff", Border: "#3b4261"},
	"dracula": {Name: "dracula", Bg: "#282a36", Fg: "#f8f8f2", Muted: "#6272a4", Subtle: "#343746", Accent: "#bd93f9",
		Accent2: "#ff79c6", OK: "#50fa7b", Warn: "#f1fa8c", Err: "#ff5555", Info: "#8be9fd", SelBg: "#44475a", SelFg: "#ffffff", Border: "#44475a"},
	"nord": {Name: "nord", Bg: "#2e3440", Fg: "#eceff4", Muted: "#4c566a", Subtle: "#3b4252", Accent: "#88c0d0",
		Accent2: "#b48ead", OK: "#a3be8c", Warn: "#ebcb8b", Err: "#bf616a", Info: "#81a1c1", SelBg: "#434c5e", SelFg: "#ffffff", Border: "#4c566a"},
	"catppuccin": {Name: "catppuccin", Bg: "#1e1e2e", Fg: "#cdd6f4", Muted: "#6c7086", Subtle: "#313244", Accent: "#89b4fa",
		Accent2: "#f5c2e7", OK: "#a6e3a1", Warn: "#f9e2af", Err: "#f38ba8", Info: "#94e2d5", SelBg: "#45475a", SelFg: "#ffffff", Border: "#45475a"},
	"gruvbox": {Name: "gruvbox", Bg: "#282828", Fg: "#ebdbb2", Muted: "#928374", Subtle: "#3c3836", Accent: "#83a598",
		Accent2: "#d3869b", OK: "#b8bb26", Warn: "#fabd2f", Err: "#fb4934", Info: "#8ec07c", SelBg: "#504945", SelFg: "#fbf1c7", Border: "#504945"},
	"solarized-light": {Name: "solarized-light", Bg: "#fdf6e3", Fg: "#586e75", Muted: "#93a1a1", Subtle: "#eee8d5", Accent: "#268bd2",
		Accent2: "#d33682", OK: "#859900", Warn: "#b58900", Err: "#dc322f", Info: "#2aa198", SelBg: "#268bd2", SelFg: "#fdf6e3", Border: "#93a1a1"},
}

// ThemeNames lists available themes.
func ThemeNames() []string {
	out := make([]string, 0, len(themes))
	for k := range themes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func themeByName(name string) Theme {
	if t, ok := themes[name]; ok {
		return t
	}
	return themes["tokyonight"]
}

// Styles are the derived lipgloss styles for a theme.
type Styles struct {
	T                                       Theme
	Base, Muted, Title, Logo, Pill, PillAlt lipgloss.Style
	Danger, TabActive, Tab, Box, BoxTitle   lipgloss.Style
	Header, Selected, Key, KeyDesc, Status  lipgloss.Style
	ErrText, OKText, WarnText, Code, Modal  lipgloss.Style
	YAMLKey, UserLabel, BotLabel            lipgloss.Style
}

func newStyles(t Theme) Styles {
	s := Styles{T: t}
	s.Base = lipgloss.NewStyle().Foreground(t.Fg)
	s.Muted = lipgloss.NewStyle().Foreground(t.Muted)
	s.Title = lipgloss.NewStyle().Foreground(t.Accent).Bold(true)
	s.Logo = lipgloss.NewStyle().Foreground(t.Bg).Background(t.Accent).Bold(true).Padding(0, 1)
	s.Pill = lipgloss.NewStyle().Foreground(t.Fg).Background(t.Subtle).Padding(0, 1)
	s.PillAlt = lipgloss.NewStyle().Foreground(t.Bg).Background(t.Accent2).Padding(0, 1)
	s.Danger = lipgloss.NewStyle().Foreground(lipgloss.Color("#ffffff")).Background(t.Err).Bold(true).Padding(0, 1)
	s.TabActive = lipgloss.NewStyle().Foreground(t.Bg).Background(t.Accent).Bold(true).Padding(0, 1)
	s.Tab = lipgloss.NewStyle().Foreground(t.Muted).Padding(0, 1)
	s.Box = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(t.Border)
	s.BoxTitle = lipgloss.NewStyle().Foreground(t.Accent).Bold(true)
	s.Header = lipgloss.NewStyle().Foreground(t.Accent2).Bold(true)
	s.Selected = lipgloss.NewStyle().Foreground(t.SelFg).Background(t.SelBg).Bold(true)
	s.Key = lipgloss.NewStyle().Foreground(t.Accent).Bold(true)
	s.KeyDesc = lipgloss.NewStyle().Foreground(t.Muted)
	s.Status = lipgloss.NewStyle().Foreground(t.Info)
	s.ErrText = lipgloss.NewStyle().Foreground(t.Err)
	s.OKText = lipgloss.NewStyle().Foreground(t.OK)
	s.WarnText = lipgloss.NewStyle().Foreground(t.Warn)
	s.Code = lipgloss.NewStyle().Foreground(t.Info).Background(t.Subtle)
	s.Modal = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(t.Accent).Padding(1, 2)
	s.YAMLKey = lipgloss.NewStyle().Foreground(t.Accent)
	s.UserLabel = lipgloss.NewStyle().Foreground(t.Bg).Background(t.Accent2).Bold(true).Padding(0, 1)
	s.BotLabel = lipgloss.NewStyle().Foreground(t.Bg).Background(t.Accent).Bold(true).Padding(0, 1)
	return s
}

func (s Styles) health(h kube.Health) lipgloss.Style {
	switch h {
	case kube.HealthErr:
		return s.ErrText
	case kube.HealthWarn:
		return s.WarnText
	case kube.HealthNeutral:
		return s.Muted
	}
	return s.Base
}

func (s Styles) dot(h kube.Health) string {
	switch h {
	case kube.HealthErr:
		return s.ErrText.Render("●")
	case kube.HealthWarn:
		return s.WarnText.Render("●")
	case kube.HealthNeutral:
		return s.Muted.Render("○")
	}
	return s.OKText.Render("●")
}
