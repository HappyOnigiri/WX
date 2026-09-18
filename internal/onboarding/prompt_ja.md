# このリポジトリの wx worktree セットアップを確認する

wx が `{{.Workspace}}` の初回セットアップ検査を貸出 worktree `{{.SlotPath}}` で行いました。
自動検査で問題がない場合も、リポジトリ固有の build・test・lint と依存関係の準備が十分か確認してください。

この依頼は、上記の貸出 worktree で起動したセッション内でも実行できます。
その場合は build・test を現在の worktree で実行し、各ソースリポジトリの main checkout は読み取り専用として扱ってください。
`.worktreeinclude`、`.worktreelink`、hook、wx 設定をソース側で変更する必要があれば、変更案を示してユーザーへ確認してください。

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

ソース側の変更が必要なら、ユーザーが反映した後に `{{.RecheckCommand}}` を実行してください。
problem または unchecked の finding がなく終了コード 0 になることが完了条件です。
