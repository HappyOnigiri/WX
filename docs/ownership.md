# 管理対象と所有権検証

## 削除

slot の削除権限は DB の `slots` と `roots` の登録で決まる。
登録済み path の実体は inode・marker・Git lock・HEAD が変わっていても回収し、workspace 紐付けや repository identity が欠けた隔離 slot も対象になる。
登録外の実体は `quarantined_artifacts` の診断記録だけに残し、その記録は削除権限にならない。
削除実装は `internal/daemon/registered_removal.go` を参照する。

削除は pin した root descriptor の配下に閉じ、root から leaf の親までの symlink は辿らない。
leaf が symlink の場合はリンク自体を削除し、リンク先へは踏み込まない。

正常終了の未保存作業は snapshot で保護し、`wx clear --discard` が明示された場合だけ保存を省略する。
`internal/archive` の clean 判定では `--untracked-files=all`・`--ignore-submodules=none` を維持する。
既定値では Git 設定で隠れる変更を clean と誤判定し、保存せずに削除し得る。
準備・復元失敗などの隔離 slot は `retention.quarantined` の経過後に GC が回収し、`wx clear` はこの経過を待たずに回収する。

## 準備・復元

準備・復元・Hot Standby更新で既存 worktree を書き換える前に求める証明は、次の3つが同時に一致することである。

1. **DBの行** — `ValidateWorktreeOwnership`が突き合わせるのは絶対pathではない。
   root世代（`roots.id`）・root相対のslot path（`slots.rel_path`）・slot内のリポジトリ配置名（`slot_repositories.dir_name`）・inode identity（`dir_identity`）の4つである。
   これに加えて、slotとリポジトリのstate、common dir、workspace内の相対pathを確かめる。
   identityは**fail closed**で、descriptorを握っている呼び出し元がidentityを渡したのに記録が空なら不一致として扱う。
   identityを渡さないのは、worktreeがまだ存在しないprepare前の検査だけである。
2. **ファイルシステム上のマーカー** — slotディレクトリ直下の`.wx-owner-<repository_id>`が、slotの識別情報と一致すること。
   内容が一致しないマーカーは所有の否定として扱う。
   マーカーをworktreeの**親**に置くのは、worktreeの再作成中もslotの識別情報を維持するためである。
3. **Gitのworktree lock** — wx自身が付けた`wx:<slot-id>:READY`・`PREPARING`・`RESTORING`のいずれかであること（`domain.ValidWxLockReason`）。
   認識できない理由でlockされたworktreeは、wxのものではない。
   Hot Standby更新中は既存のREADY lockを維持し、DBのPREPARING/UPDATE_RUNNINGと組み合わせて所有権を証明する。

rootのpinは`os.Root`と`domain.OpenOwnedRoot`、配下のsymlink拒否は`domain.PhysicalPathInfo`が担う。
`domain.ValidatePhysicalLeaf`はleafだけを見るため、root自身より上の祖先成分は検査しない（[AGENTS.md](../AGENTS.md)の不変条件）。
子プロセスのCWDは`internal/fdexec`でdescriptorへ束縛する。
マーカーとGit lockはpinしたroot descriptor配下の相対pathで検証する。

DBが持つidentityはdescriptorが返す`vol:<inode>:<volume>`と同じ形式なので、2つの層が同じ対象を指していることを比較できる。
volume成分にdevice番号を使わないのは、macOSではmount順で決まるdevice番号が再起動をまたいで変わり、記録済みの行が一斉に一致しなくなるためである（darwinではmount point、linuxではfilesystem IDで表す）。
device番号を含む旧形式で記録された行は、`EnsureActiveRoot`がinodeの一致を確かめた上で、root世代とその配下のslot・リポジトリまとめて現行形式へ書き換える。
`workspace_repositories.relative_path`はソース側でのリポジトリ位置という本来の意味だけを担い、slot内の配置は`slot_repositories.dir_name`が持つ。

この粒度では、証明から操作までの間に別のプロセスが対象へ書き込んだ内容を検出できない。
それを承知で採る方針なので、書き込み得るプロセスが並行しない位置（貸出前の準備・復元中など）に破壊的操作を置く。
