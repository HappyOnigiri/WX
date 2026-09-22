package daemon

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// updateApplyLogMode は自動適用の子の出力ファイルの権限で、daemon log と同じく利用者だけが読む。
const updateApplyLogMode = 0o600

// spawnDetached は argv の子 process をセッションを切り離して起動し、終了を待たずに返す。
// install.sh は自分で `wx daemon restart` を呼んで親 daemon を止めるため、待つと自分の停止を待つ循環になる。
// Setsid を付けないと、その停止で親と同じ session ごと子も落ちる。exec.CommandContext も同じ事故になるので使わない。
func spawnDetached(argv, env []string, logPath string) error {
	if len(argv) == 0 {
		return errors.New("spawn requires a command")
	}
	// 試行ごとに truncate し、最新の 1 回分だけを残す。
	out, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, updateApplyLogMode)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	// CommandContext は使えない。context の取り消しが子を殺すため、親の停止を子が生き延びられない。
	command := exec.Command(argv[0], argv[1:]...) //nolint:gosec,noctx // 上のコメントの理由による
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	command.Stdin = nil
	command.Stdout, command.Stderr = out, out
	command.Env = env
	if err := command.Start(); err != nil {
		return err
	}
	// 結果は待たないが、Wait を呼ばないと終了した子が zombie として残る。
	go func() { _ = command.Wait() }()
	return nil
}
