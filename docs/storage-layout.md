# ディスク配置とroot世代

worktree root（`storage.worktree_root`）配下は次の形になる。

```text
<worktree_root>/<workspace-id>/<slot-id>/<RepoName>/
<worktree_root>/_unbound/<slot-id>/
<worktree_root>/_recovery/workspace-snapshots/<安定ID>.tar
```

`_unbound/<slot-id>`は旧リリースが生成した残骸の回収用にだけ残り、新しいslotの生成には使わない。

workspace-idとslot-idは固定長の小文字英数字（`domain.NewShortID`）で、slot-idはleaseのsession IDと同値である。
slot内の配置名の衝突判定も小文字化して行い（APFSが既定でcase-insensitiveなため）、衝突したらサフィックスで避ける。

`_`始まりはwxの予約プレフィックスで、workspace IDもリポジトリ配置名もこの接頭辞を拒否する。
孤児スキャン（`ownedRootArtifactPaths`）は予約名を通常workspaceとして列挙せず、旧`_unbound`だけを回収対象として特別扱いする。
新規slotは未使用pathをDBへ予約してから作成し、既存pathとの衝突時は採用せず予約を取り消す。

エージェントへ貸し出す単位（`Lease.Path`）は、単一リポジトリworkspaceなら`<slot-id>/<RepoName>`、multi_repositoryなら`<slot-id>`である。
単一リポジトリでCWDをリポジトリ直下にすることで、`.wx-owner-*`がCWDの親に残りエージェントから見えない。
`Lease.Path`と`state.Slot.Path`（常にslotディレクトリ）を混同しないこと。

`RepoName`の解決順序（`repositories.<main path>.dir_name`・`dir_source`と`storage.repo_dir_source`）は`internal/workspace/dirname.go`が持つ。
採用した値は`slot_repositories.dir_name`へ記録して以後の権威にする。
設定やremote URLが後から変わっても既存slotは記録済みの名前で動き続け、`workspace.Fingerprint`が準備入力を含むので新規slotから新しい設定を使う。

参照するslotもスナップショットも無くなった`roots`行はGCが削除するが、ディレクトリの実体は消さない。
`storage.worktree_root`を変えても既存slotが登録済みのroot世代で動き続けることは、`internal/daemon`の
[`TestWorktreeRootChangeKeepsExistingSessionsAndPlacesNewOnesInTheNewRoot`](../internal/daemon/roots_integration_test.go)が固定している。

multi_repositoryのworkspaceスナップショットは、slotディレクトリ自体をbundle rootとしてtarに詰める。
そのためリポジトリのworktreeと`.wx-owner-*`はどちらも除外リストに載せる（`workspaceRecoveryExclusions`）。
除外に使うのは`slot_repositories.dir_name`で、ソース側の`workspace_repositories.relative_path`ではない。
マーカーを除外しないと、archiveが別slotのIDを運び、復元前のpruneが現在のslotの所有権証拠を消してしまう。

状態は設定に依らずOSのアプリケーション支援ディレクトリ配下に固定した`state.db`が持ち（worktree rootには含めない）、オンラインバックアップも同じ場所へ世代保存する。
Hot Standby更新の固定先と開始・完了境界、wxが配置したcopy/linkのfile単位履歴もこのDBに保存し、ディスク上へ別の管理directoryは作らない。

リポジトリ単位のsnapshotの保存先・公開順序は[セッションと復元](session-lifecycle.md)を参照する。
ソースリポジトリを読めるプロセスからは、そのsnapshotの中身も読める。
