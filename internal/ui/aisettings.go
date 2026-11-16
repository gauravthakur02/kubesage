package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/gauravthakur02/kubesage/internal/config"
)

// openSecretInput asks for a value without displaying it (API keys).
func (a *App) openSecretInput(hint string, onSubmit func(string) tea.Cmd) {
	a.openInput(ovInput, hint, onSubmit)
	a.input.EchoMode = textinput.EchoPassword
	a.input.EchoCharacter = '•'
}

func (a *App) clearSecretInput() {
	if a.input.EchoMode == textinput.EchoPassword {
		a.input.SetValue("")
		a.input.EchoMode = textinput.EchoNormal
	}
}

// promptKey asks for a profile's API key and stores it in the OS keychain.
func (a *App) promptKey(profile string) tea.Cmd {
	if _, ok := a.cfg.AI.Profiles[profile]; !ok {
		a.setStatus("no AI profile "+profile+" (add one with :ai-add)", true)
		return nil
	}
	a.openSecretInput(fmt.Sprintf("API key for %s (hidden, stored in %s) › ", profile, config.KeyringName()), func(key string) tea.Cmd {
		if strings.TrimSpace(key) == "" {
			a.setStatus("no key entered; nothing changed", true)
			return nil
		}
		where, err := a.cfg.SetKey(profile, key)
		if err != nil {
			a.setStatus("could not store key: "+err.Error(), true)
			return nil
		}
		if err := a.cfg.Save(); err != nil {
			a.setStatus("key stored in "+where+" but config not saved: "+err.Error(), true)
			return nil
		}
		if profile == a.profile {
			a.setProfile(profile) // rebuild the provider with the new key
		}
		a.setStatus(fmt.Sprintf("✓ key for %s stored in %s · %s", profile, where, a.cfg.AI.Profiles[profile].KeyStatus()), false)
		a.renderChat()
		return nil
	})
	return nil
}

// addProfileWizard: provider → name → model → (endpoint) → key, then activates it.
func (a *App) addProfileWizard() tea.Cmd {
	a.openPicker("Add AI profile · choose provider", config.Providers(), func(provider string) tea.Cmd {
		p := config.ProviderDefaults(provider)
		a.openInput(ovInput, "profile name › ", func(name string) tea.Cmd {
			name = strings.TrimSpace(name)
			modelHint := "model › "
			if provider == "azure" {
				modelHint = "Azure deployment name › "
			} else if provider == "ollama" {
				modelHint = "model (empty = first installed) › "
			}
			a.openInput(ovInput, modelHint, func(model string) tea.Cmd {
				p.Model = strings.TrimSpace(model)
				finish := func() tea.Cmd {
					if err := a.cfg.AddProfile(name, p); err != nil {
						a.setStatus(err.Error(), true)
						return nil
					}
					a.cfg.AI.Active = name
					if err := a.cfg.Save(); err != nil {
						a.setStatus("profile added but config not saved: "+err.Error(), true)
					}
					a.setProfile(name)
					a.renderChat()
					if p.NeedsKey() && a.cfg.AI.Profiles[name].Key() == "" {
						return a.promptKey(name)
					}
					a.setStatus(fmt.Sprintf("✓ AI profile %s added and active · %s", name, a.cfg.AI.Profiles[name].KeyStatus()), false)
					return nil
				}
				switch provider {
				case "azure", "openai-compatible", "ollama":
					hint := "endpoint URL › "
					if provider == "azure" {
						hint = "Azure endpoint (https://<resource>.openai.azure.com) › "
					}
					a.openInput(ovInput, hint, func(u string) tea.Cmd {
						if u = strings.TrimSpace(u); u != "" {
							p.BaseURL = u
						}
						return finish()
					})
					a.input.SetValue(p.BaseURL)
					a.input.CursorEnd()
					return nil
				}
				return finish()
			})
			a.input.SetValue(p.Model)
			a.input.CursorEnd()
			return nil
		})
		a.input.SetValue(provider)
		a.input.CursorEnd()
		return nil
	})
	return nil
}
