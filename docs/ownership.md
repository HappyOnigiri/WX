# 管理対象と所有権検証

## 削除権限

slotの削除権限はDBの`slots`・`roots`の登録だけで決まる。
登録済みpathの実体は、inode・marker・Git lock・HEADが食い違っていても回収し、workspace紐付けやrepository identityを失った隔離slotも対象になる。
逆に登録外の実体は`quarantined_artifacts`へ診断として記録するだけで、その記録は削除権限にならない（[AGENTS.md](../AGENTS.md)の不変条件）。

削除はpinしたroot descriptorの配下に閉じ、rootからleafの親までのsymlinkは辿らない。
leafがsymlinkの場合はリンク自体を削除し、リンク先へは踏み込まない。

実体化したsubmoduleのper-worktree gitdirは共有Gitディレクトリ側にあるが、wxは`.git`配下を個別に削除する経路を持たない。
Gitのworktree削除が失敗しても、worktree管理ディレクトリの一括削除が同じ回収を担保するため、失敗経路にも個別削除を足さない。
破壊的操作をpinしたroot descriptor配下に閉じる不変条件を、submoduleのために広げないためである。

## 削除で失われる作業の保護

正常終了の未保存作業はsnapshotで保護し、`--discard`が明示された場合だけ保存を省略する。

`internal/archive`のclean判定では`--untracked-files=all`・`--ignore-submodules=none`を維持する。
既定値ではGit設定で隠れる変更をcleanと誤判定し、保存せずに削除し得る。

`skip-worktree`・`assume-unchanged`が付いたpathはsnapshotの対象から外す（記録のされ方は[セッションと復元](session-lifecycle.md)）。
両flagが「このファイルのローカル差分を見ない」という宣言であり、hookが置いた個人設定や認証情報をsourceのrecovery objectに残さないためである。

準備・復元失敗などの隔離slotは`retention.quarantined`の経過後にGCが回収し、`wx clear`はこの経過を待たずに回収する。
recovery refの欠損で隔離したsnapshot・sessionはGCが触らず、`wx discard-recovery`だけが破棄する（[daemonの補充と回収](daemon-maintenance.md)）。

## 既存worktreeを書き換える前の証明

準備・復元・Hot Standby更新の前に、次の3つが同時に一致することを求める。

1. **DBの行**（`ValidateWorktreeOwnership`） — 突き合わせるのは絶対pathではなく、root世代・root相対のslot path・slot内のリポジトリ配置名・inode identityの組である。
   `storage.worktree_root`を変えても既存slotが登録済みのroot世代のまま寿命を全うできるようにするためである。
   identityは**fail closed**で、descriptorを握っている呼び出し元がidentityを渡したのに記録が空なら不一致として扱う。
   identityを渡さないのは、worktreeがまだ存在しないprepare前の検査だけである。
2. **ファイルシステム上のマーカー** — slotディレクトリ直下のマーカー（`workspace.MarkerIdentity`）が、slotの識別情報と一致すること。
   内容が一致しないマーカーは所有の否定として扱う。
   worktree自身ではなく**親**に置いてworktree削除後も残すのは、SQLiteを失ったときに残る唯一のディスク上の所有権の証拠だからである。
3. **Gitのworktree lock** — wx自身が付けたと判別できる理由であること（`domain.ValidWxLockReason`）。認識できない理由でlockされたworktreeは、wxのものではない。
   Hot Standby更新中は既存のREADY lockを維持したまま、DB側の状態と組み合わせて所有権を証明する。

rootのpinは`domain.OpenOwnedRoot`が握る。
root配下は全成分でsymlinkを拒否する一方、`domain.ValidatePhysicalLeaf`はleafだけを見るためroot自身より上の祖先成分は検査しない（[AGENTS.md](../AGENTS.md)の不変条件）。
マーカーとGit lockもpinしたroot descriptor配下の相対pathで検証し、子プロセスのCWDは`internal/fdexec`でdescriptorへ束縛する。

## identityの表現

DBが持つidentityはdescriptorが返す`vol:<inode>:<volume>`と同じ形式なので、DBとファイルシステムの2つの層が同じ対象を指していることを比較できる。
volume成分にdevice番号を使わないのは、macOSではmount順で決まるdevice番号が再起動をまたいで変わり、記録済みの行が一斉に一致しなくなるためである（darwinではmount point、linuxではfilesystem IDで表す）。
device番号を含む旧形式の行は、`EnsureActiveRoot`がroot世代ごと現行形式へ書き換える。

`workspace_repositories.relative_path`はソース側でのリポジトリ位置という本来の意味だけを担い、slot内の配置は`slot_repositories.dir_name`が持つ。

## この証明が保証しないこと

証明から操作までの間に、別のプロセスが対象へ書き込んだ内容は検出できない。
それを承知で採る方針なので、書き込み得るプロセスが並行しない位置（貸出前の準備・復元中など）に破壊的操作を置く。
