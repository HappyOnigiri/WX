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

	"github.com/HappyOnigiri/WX/internal/config"
)

const (
	Label         = "com.user.wx"
	plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>{{.Label}}</string>
<key>ProgramArguments</key><array><string>{{.Binary | x}}</string><string>daemon</string><string>start</string><string>--foreground</string></array>
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

// ResolveBinary は LaunchAgent が起動する wx executable を返す。
// PATH 上の command を優先し、開発 build や test binary では os.Executable にフォールバックする。
// 既存 plist の診断も同じ解決処理を使い、インストールと判定の対象を揃える。
func ResolveBinary() (string, error) {
	binary, err := exec.LookPath("wx")
	if err == nil {
		return binary, nil
	}
	return os.Executable()
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

// PlistStatus は現在の wx executable から LaunchAgent を再生成できるかを示す。
// plist 不在や読み取り不能は Unknown とし、未導入・未読を stale と誤判定しない。
type PlistStatus uint8

const (
	PlistUnknown PlistStatus = iota
	PlistCurrent
	PlistStale
)

// CurrentPlistStatus は on-disk の LaunchAgent 全体を、現在の daemon install の出力と比較する。
// plist 全体を wx が所有するため byte 単位で比較し、差分は install の再実行で修復する。
func CurrentPlistStatus() (PlistStatus, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return PlistUnknown, err
	}
	path, err := PlistPath()
	if err != nil {
		return PlistUnknown, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return PlistUnknown, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return PlistUnknown, errors.New("unsafe symlink")
	}
	if !info.Mode().IsRegular() {
		return PlistUnknown, errors.New("LaunchAgent plist is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return PlistUnknown, err
	}
	binary, err := ResolveBinary()
	if err != nil {
		return PlistUnknown, fmt.Errorf("resolve wx executable: %w", err)
	}
	logPath, err := config.LogPath()
	if err != nil {
		return PlistUnknown, err
	}
	expected, err := Render(binary, home, logPath)
	if err != nil {
		return PlistUnknown, err
	}
	if bytes.Equal(data, expected) {
		return PlistCurrent, nil
	}
	return PlistStale, nil
}

// xmlEscapeText は template FuncMap の entry。補間する全値を escape するため、生成後の再解析は不要。
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

// ErrServiceMissing は launchctl が wx LaunchAgent を認識していないことを示す。
// 呼び出し側は操作不能な launchctl エラーではなく wx daemon install を案内できる。
var ErrServiceMissing = errors.New("wx LaunchAgent is not installed")

// Kickstart は実行中 service を置き換える。launchctl の -k で終了させてから launchd が再起動する。
// 稼働確認だけなら、実行中 daemon を残す Start を使う。
func Kickstart(ctx context.Context) error {
	return kickstart(ctx, "-k")
}

// Start は未稼働時だけ launchd に service の実行を依頼する。
// -k を使わないため、他セッションを処理中の daemon を終了させず繰り返し実行できる。
func Start(ctx context.Context) error {
	return kickstart(ctx)
}

func kickstart(ctx context.Context, flags ...string) error {
	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), Label)
	args := append([]string{"kickstart"}, flags...)
	args = append(args, target)
	out, err := exec.CommandContext(ctx, "launchctl", args...).CombinedOutput()
	if err != nil {
		if launchctlServiceMissing(out) {
			return fmt.Errorf("launchctl kickstart: %s: %w", bytes.TrimSpace(out), ErrServiceMissing)
		}
		// launchctl が実行されない場合は出力がないため、原因の識別には wrap 済みエラーだけを使う。
		return fmt.Errorf("launchctl kickstart: %s: %w", bytes.TrimSpace(out), err)
	}
	return nil
}
