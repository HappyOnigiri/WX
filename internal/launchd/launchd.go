package launchd

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
)

const (
	Label         = "com.user.wx"
	plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>{{.Label}}</string>
<key>ProgramArguments</key><array><string>{{.Binary | x}}</string><string>daemon</string><string>serve</string></array>
<key>RunAtLoad</key><true/><key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
<key>EnvironmentVariables</key><dict><key>HOME</key><string>{{.Home | x}}</string><key>PATH</key><string>{{.Path | x}}</string></dict>
<key>StandardOutPath</key><string>{{.Log | x}}</string><key>StandardErrorPath</key><string>{{.Log | x}}</string>
</dict></plist>`
)

func PlistPath() (string, error) {
	h, e := os.UserHomeDir()
	if e != nil {
		return "", e
	}
	return filepath.Join(h, "Library", "LaunchAgents", Label+".plist"), nil
}

func Render(binary, home, logPath string) ([]byte, error) {
	t, err := template.New("plist").Funcs(template.FuncMap{"x": xmlEscapeText}).Parse(plistTemplate)
	if err != nil {
		return nil, err
	}
	pathValue := filepath.Join(home, ".local", "bin") + ":/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"
	var b bytes.Buffer
	if err := t.Execute(&b, map[string]string{"Label": Label, "Binary": binary, "Home": home, "Path": pathValue, "Log": logPath}); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// xmlEscapeText is a template FuncMap entry: the template calls it on every
// value it interpolates, so every rendered plist is escaped by construction
// and does not need a separate re-parse to validate it afterwards.
func xmlEscapeText(value string) (string, error) {
	var escaped bytes.Buffer
	if err := xml.EscapeText(&escaped, []byte(value)); err != nil {
		return "", err
	}
	return escaped.String(), nil
}

func Install(ctx context.Context, binary, logPath string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path, err := PlistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return err
	}
	data, err := Render(binary, home, logPath)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".wx-plist-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	_ = exec.CommandContext(ctx, "launchctl", "bootout", domain, path).Run()
	if out, err := exec.CommandContext(ctx, "launchctl", "bootstrap", domain, path).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %s", bytes.TrimSpace(out))
	}
	return nil
}

func Uninstall(ctx context.Context) error {
	path, err := PlistPath()
	if err != nil {
		return err
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	if out, err := exec.CommandContext(ctx, "launchctl", "bootout", domain, path).CombinedOutput(); err != nil && !launchctlServiceMissing(out) {
		return fmt.Errorf("launchctl bootout: %s", bytes.TrimSpace(out))
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove LaunchAgent plist: %w", err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync LaunchAgent directory: %w", err)
	}
	return nil
}

func launchctlServiceMissing(output []byte) bool {
	message := strings.ToLower(string(output))
	return strings.Contains(message, "could not find service") || strings.Contains(message, "no such process") || strings.Contains(message, "service not found")
}

// ErrServiceMissing reports that launchctl does not know the wx LaunchAgent at
// all, as opposed to knowing it and failing to act on it. Callers use it to
// tell an operator to run wx daemon install instead of reporting a launchctl
// failure they cannot act on.
var ErrServiceMissing = errors.New("wx LaunchAgent is not installed")

func Kickstart(ctx context.Context) error {
	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), Label)
	out, err := exec.CommandContext(ctx, "launchctl", "kickstart", "-k", target).CombinedOutput()
	if err != nil {
		if launchctlServiceMissing(out) {
			return fmt.Errorf("launchctl kickstart: %s: %w", bytes.TrimSpace(out), ErrServiceMissing)
		}
		// launchctl prints nothing when it never ran (missing from PATH, a
		// cancelled context, a deadline reached while it was still working), so
		// the wrapped error is the only thing that identifies those failures.
		return fmt.Errorf("launchctl kickstart: %s: %w", bytes.TrimSpace(out), err)
	}
	return nil
}
