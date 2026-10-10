package ui

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"fyne.io/fyne/v2/test"
	"modbus-ai-studio/internal/ai"
)

func setAITestConfigDir(t *testing.T, dir string) {
	t.Helper()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("APPDATA", dir)
	case "darwin":
		t.Setenv("HOME", dir)
	default:
		t.Setenv("XDG_CONFIG_HOME", dir)
	}
}

// Ordinary tests must never read or change credentials from the user's profile.
// The explicitly enabled deepseek_live test keeps its separate openWS path.
func openAIWS(t *testing.T, demo bool) *Workspace {
	t.Helper()
	setAITestConfigDir(t, t.TempDir())
	t.Setenv("DEEPSEEK_API_KEY", "")
	return openWS(t, test.NewTempApp(t), demo)
}

func TestAITestWorkspaceIsolatesUserCredentials(t *testing.T) {
	setAITestConfigDir(t, t.TempDir())
	t.Setenv("DEEPSEEK_API_KEY", strings.Repeat("x", 16))
	callerPath, err := ai.KeyPath()
	if err != nil {
		t.Fatal("cannot resolve the synthetic caller config directory")
	}
	if ai.ProtectedStorage {
		if err := ai.SaveKey(callerPath, strings.Repeat("y", 16)); err != nil {
			t.Fatal("cannot prepare synthetic caller credentials")
		}
	}
	ws := openAIWS(t, false)
	locked(func() {
		ws.openAI(nil)
		tool := ws.ai
		if tool.key.Text != "" || tool.keyLoadErr != "" {
			t.Error("ordinary AI workspace loaded caller credentials")
		}
		if filepath.Clean(tool.keyPath) == filepath.Clean(callerPath) {
			t.Error("ordinary AI workspace uses the caller credential path")
		}
		if _, err := os.Stat(tool.keyPath); !os.IsNotExist(err) {
			t.Error("isolated AI workspace unexpectedly has a saved credential file")
		}
	})
	if ai.ProtectedStorage {
		key, err := ai.LoadKey(callerPath)
		if err != nil || key != strings.Repeat("y", 16) {
			t.Error("ordinary AI workspace changed caller credentials")
		}
	}
}
