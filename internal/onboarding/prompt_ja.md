# このリポジトリの wx worktree セットアップを確認する

wx が `{{.Workspace}}` の初回セットアップ検査を貸出 worktree `{{.SlotPath}}` で行いました。
自動検査で問題がない場合も、リポジトリ固有の build・test・lint と依存関係の準備が十分か確認してください。

wx がこの貸出 worktree で既に起動したセッション内では、この依頼を実行しないでください。
`.worktreeinclude`、`.worktreelink`、hook、wx 設定の編集は各ソースリポジトリの main checkout で行ってください。
build・test コマンドは新しい wx worktree に `wx run <コマンド>` で実行してください。

## 検査結果

{{range .Findings}}

- **{{severity .Severity}} / {{.Check}}** — {{.Summary}}{{if .Target}}
  - 対象: `{{.Target}}`{{end}}{{if .Cause}}
  - 原因: {{.Cause}}{{end}}{{if .Action}}
  - 対処: {{.Action}}{{end}}
{{end}}

## リポジトリ

{{range .Repositories}}

- `{{.RelativePath}}`: main checkout `{{.MainPath}}`、検査した slot path `{{.SlotPath}}`
{{end}}

README、package.json、Makefile などから、このリポジトリで想定される build・test・lint・依存関係の準備を読み取ってください。
それらが wx worktree 内で通る状態にしてください。
wx の finding だけから不足ファイルを推測せず、main checkout と貸出 worktree を比較してください。

slot ごとに独立して必要な未追跡ファイルには `.worktreeinclude` を使います。
`.worktreelink` の項目は slot とソースリポジトリで同じ実体を共有し、slot からの書き込みがソース checkout に届きます。
link 候補は提案に留め、追加前にユーザーへ確認してください。

最後に `{{.RecheckCommand}}` を実行してください。problem または unchecked の finding がなく終了コード 0 になることが完了条件です。
