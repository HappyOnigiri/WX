# ディスク配置とroot世代

worktree root（`storage.worktree_root`、既定`$HOME/wx`）配下は次の形になる。

```text
<worktree_root>/<workspace-id>/<slot-id>/<RepoName>/
<worktree_root>/_unbound/<slot-id>/
<worktree_root>/_recovery/workspace-snapshots/<安定ID>.tar
```

`_unbound/<slot-id>`は旧リリースが生成した残骸の回収用にだけ残り、新しいslotの生成には使わない。
workspace-idとslot-idは6桁固定の小文字英数字（base36、`domain.NewShortID`）である。
slot-idはleaseのsession IDと同値なので、`wx slots`が出すIDをそのまま`wx resume`に渡せる。
大文字を混ぜないのはAPFSが既定でcase-insensitiveなためで、同じ理由からslot内の配置名の衝突判定も小文字化して行い、衝突したら`-2`のサフィックスを付ける。

`_`始まりはwxの予約プレフィックスで、workspace IDもリポジトリ配置名もこの接頭辞を拒否し、旧`_unbound`は回収用に特別扱いする。
孤児スキャン（`ownedRootArtifactPaths`）は予約名を通常workspaceとして列挙せず、旧`_unbound`だけを回収対象として特別扱いする。
旧UNBOUND行と`_unbound`の回収分岐は1リリースだけ維持し、孤児検出後に`Release`→`REMOVE`で自然に削除する。

エージェントへ貸し出す単位（`Lease.Path`）は、単一リポジトリworkspaceなら`<slot-id>/<RepoName>`、multi_repositoryなら`<slot-id>`である。
単一リポジトリでCWDをリポジトリ直下にすることで、`.wx-owner-*`がCWDの親に残りエージェントから見えない。
`Lease.Path`と`state.Slot.Path`（常にslotディレクトリ）を混同しないこと。

`RepoName`の決定順は`repositories.<main path>.dir_name` → `repositories.<main path>.dir_source` → `storage.repo_dir_source`（既定`remote`）→ main worktreeのディレクトリ名である。
`remote`は`git remote get-url origin`の出力から末尾の`.git`を除いたbasenameで、取れないときはディレクトリ名へ落ちる。
採用した値は`slot_repositories.dir_name`に記録され、以後はその値が権威になる。
設定やremote URLが後から変わっても既存slotは記録済みの名前で動き続け、`workspace.Fingerprint`が準備入力を含むので新規slotから新しい設定を使う。

参照するslotもスナップショットも無くなった`roots`行はGCが削除するが、ディレクトリの実体は消さない。

multi_repositoryのworkspaceスナップショットは、slotディレクトリ自体をbundle rootとしてtarに詰める。
そのためリポジトリのworktreeと`.wx-owner-*`はどちらも除外リストに載せる（`workspaceRecoveryExclusions`）。
除外に使うのは`slot_repositories.dir_name`で、ソース側の`workspace_repositories.relative_path`ではない。
マーカーを除外しないと、archiveが別slotのIDを運び、復元前のpruneが現在のslotの所有権証拠を消してしまう。

状態は`~/Library/Application Support/wx/state.db`が持ち、同じ場所の`state.db.backups/`にオンラインバックアップを世代保存する。

リポジトリ単位のsnapshotの保存先・公開順序は[セッションと復元](session-lifecycle.md)を参照する。
ソースリポジトリを読めるプロセスからは、そのsnapshotの中身も読める。
