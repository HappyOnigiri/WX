package daemon

import (
	"errors"

	"github.com/HappyOnigiri/WorktreeX/internal/gitx"
)

// gitFailureInfo は Git が既に書いた詳細ログを、処理種別の failure code と path に結び付ける。
// err が Git 以外なら ok=false とし、呼び出し側の既存の失敗分類を変えない。
func gitFailureInfo(kind string, err error, detailDir string) (code, detailPath string, ok bool) {
	var gitErr *gitx.Error
	if !errors.As(err, &gitErr) {
		return "", "", false
	}
	code = kind + "_FAILED"
	if gitErr.FailureID != "" {
		code += ":" + gitErr.FailureID
	}
	return code, gitx.DetailPath(detailDir, gitErr.FailureID), true
}
