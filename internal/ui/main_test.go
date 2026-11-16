package ui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/gauravthakur02/kubesage/internal/config"
)

// Tests never touch the real keychain or the real config file.
func TestMain(m *testing.M) {
	keyring.MockInit()
	os.Exit(m.Run())
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	c, err := config.Load(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
