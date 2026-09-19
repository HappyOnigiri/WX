# このリポジトリの wx worktree セットアップを確認する

wx が `{{.Workspace}}` の初回セットアップ検査を貸出 worktree `{{.SlotPath}}` で行いました。
自動検査で問題がない場合も、リポジトリ固有の build・test・lint と依存関係の準備が十分か確認してください。

この依頼は、上記の貸出 worktree で起動したセッション内でも実行できます。
その場合は build・test を現在の worktree で実行してください。
各ソースリポジトリの tracked file・HEAD・index は読み取り専用です。
ただし、このタスクでは main checkout 直下の `.worktreelink`、`.worktreeinclude` と、そのリポジトリの `.git/info/exclude` を更新して構いません。
これらは変更案の提示に留めず、リポジトリ内の情報から判断できる限りユーザーへ質問せずに自動設定してください。

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

未追跡の必要な path は、原則として `.worktreelink` に追加してください。
`.worktreelink` は main checkout とすべての slot で同じ実体を共有します。
同じ実体を共有すると競合・破損・意図しない状態伝播が起きるものだけ `.worktreeinclude` に追加し、slot ごとにコピーしてください。
同じ path を両方の manifest に書かず、tracked path はどちらにも追加しないでください。

各 manifest と link 対象が Git の ignore 対象か `git check-ignore` で確認してください。
`.worktreelink` または `.worktreeinclude` 自体が ignore されていなければ、それぞれ `/.worktreelink` または `/.worktreeinclude` をそのリポジトリの `.git/info/exclude` に重複なく追加してください。
link 対象が ignore されていなければ、対象を限定する root 相対の規則も同じ exclude に追加してください。
linked worktree でも正しい exclude を更新できるよう、編集先は `git rev-parse --git-path info/exclude` で解決してください。

過去のセットアップでは次の問題が起きています。設定時に同じ問題がないか確認してください。

- `node_modules`、`vendor`、仮想環境、build cache などを worktree 間で共有すると、絶対 path を含む metadata、install、cleanup が互いを壊すことがあります。
  これらは link も include もせず、必要なら repository の既存の prepare command や hook で slot ごとに再生成してください。
- ディレクトリの link 対象を `.git/info/exclude` に書くとき、末尾 `/` がある規則は配置前の存在しない path に一致しないことがあります。
  `/tmp` のように末尾 `/` のない root 相対規則を使い、source と slot の両方で `git check-ignore` が成功することを確認してください。
- 複数 repository の workspace では、各 repository の main checkout 直下にその repository 用の manifest を置いてください。repository 内の下位ディレクトリに manifest を置いても読み込まれません。
- tracked file は checkout で配置されるため manifest へ残さないでください。以前は追跡化された `AGENTS.md` などが古い include に残り、準備失敗の原因になりました。

設定後は `{{.RecheckCommand}}` を自分で実行し、必要なら設定と検証を繰り返してください。
problem または unchecked の finding がなく終了コード 0 になることが完了条件です。
