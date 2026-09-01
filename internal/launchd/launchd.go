package launchd

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"text/template"
)

const (
	Label         = "com.user.wx"
	plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>{{.Label}}</string>
<key>ProgramArguments</key><array><string>{{.Binary}}</string><string>daemon</string><string>serve</string></array>
<key>RunAtLoad</key><true/><key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
<key>EnvironmentVariables</key><dict><key>HOME</key><string>{{.Home}}</string><key>PATH</key><string>{{.Path}}</string></dict>
<key>StandardOutPath</key><string>{{.Log}}</string><key>StandardErrorPath</key><string>{{.Log}}</string>
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
	t, err := template.New("plist").Parse(plistTemplate)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	err = t.Execute(&b, map[string]string{"Label": Label, "Binary": binary, "Home": home, "Path": filepath.Join(home, ".local", "bin") + ":/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin", "Log": logPath})
	return b.Bytes(), err
}

func Install(binary, logPath string) error {
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
	_ = exec.Command("launchctl", "bootout", domain, path).Run()
	if out, err := exec.Command("launchctl", "bootstrap", domain, path).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %s", bytes.TrimSpace(out))
	}
	return nil
}

func Uninstall() error {
	path, err := PlistPath()
	if err != nil {
		return err
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	if out, err := exec.Command("launchctl", "bootout", domain, path).CombinedOutput(); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("launchctl bootout: %s", bytes.TrimSpace(out))
	}
	return nil
}

func Kickstart() error {
	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), Label)
	out, err := exec.Command("launchctl", "kickstart", "-k", target).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl kickstart: %s", bytes.TrimSpace(out))
	}
	return nil
}
