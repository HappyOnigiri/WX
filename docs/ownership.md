# 所有権証明

破壊的操作の前に求める証明は、次の3つが同時に一致することである。

1. **DBの行** — `ValidateWorktreeOwnership`が`roots`・`slots`・`slot_repositories`・`workspaces`・`repositories`・`workspace_repositories`の6表を結合して1行を読む。
   突き合わせるのは絶対pathではない。
   root世代（`roots.id`）・root相対のslot path（`slots.rel_path`）・slot内のリポジトリ配置名（`slot_repositories.dir_name`）・inode identity（`dir_identity`）の4つである。
   これに加えて、slotとリポジトリのstate、common dir、workspace内の相対pathを確かめる。
   読み取り専用トランザクションが囲むのはSELECTだけで、一致判定はcommit後に走る。
   identityは**fail closed**で、descriptorを握っている呼び出し元がidentityを渡したのに記録が空なら不一致として扱う。
   identityを渡さないのは、開くべきディレクトリが無い2つの場合だけである。
   worktreeがまだ存在しないprepare前の検査と、worktreeの実体が既に消えていてGitの登録だけが残っている削除（`archive.Manager.RemoveWorktree`のmissing-registration分岐）である。
   slotディレクトリ自体の証明（`ValidateSlotOwnership`）も同じで、削除直前の検査はpin済みroot descriptorから読んだ実inodeを渡す。
   `slots.dir_identity`を読み直して渡すと同じ行を自分自身と比べることになり、identity層が実効を失う。
2. **ファイルシステム上のマーカー** — slotディレクトリ直下の`.wx-owner-<repository_id>`に、slot ID・root ID・repository ID・common dirをJSONで書く（`version: 2`）。
   内容が一致しないマーカーは所有の否定として扱う。
   マーカーはworktreeの**親**に置く。
   worktree削除が中断されても、再試行時に所有権を証明できる唯一のディスク側証拠がこれだからである。
3. **Gitのworktree lock** — wx自身が付けた`wx:<slot-id>:READY`・`PREPARING`・`RESTORING`のいずれかであること（`domain.ValidWxLockReason`）。
   認識できない理由でlockされたworktreeは、wxのものではない。

マーカーとGit lockはpinしたroot descriptor配下の相対pathで検証する。
rootのpin・symlink検査・証明の回数は[AGENTS.md](../AGENTS.md)の不変条件に従う。
DBが持つidentityはdescriptorが返す`vol:<inode>:<volume>`と同じ形式なので、2つの層が同じ対象を指していることを比較できる。
volume成分にdevice番号を使わないのは、macOSではmount順で決まるdevice番号が再起動をまたいで変わり、記録済みの行が一斉に一致しなくなるためである（darwinではmount point、linuxではfilesystem IDで表す）。
device番号を含む旧形式で記録された行は、`EnsureActiveRoot`がinodeの一致を確かめた上で、root世代とその配下のslot・リポジトリまとめて現行形式へ書き換える。
`workspace_repositories.relative_path`はソース側でのリポジトリ位置という本来の意味だけを担い、slot内の配置は`slot_repositories.dir_name`が持つ。

この粒度では、証明から操作までの間に別のプロセスが対象へ書き込んだ内容を検出できない。
それを承知で採る方針なので、書き込み得るプロセスが並行しない位置（貸出前の準備・復元中など）に破壊的操作を置く。
