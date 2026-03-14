# claude-guard

Claude Code 向けのコマンドガードレール。YAMLポリシーで許可されたサブコマンド・オプションのみ実行を許可します。

## 概要

2つの動作モードがあります：

**exec モード（推奨）** — `claude-guard exec` で環境構築からコマンド起動まで一発で行う

```bash
claude-guard exec bash                    # ガード環境でbashを起動
claude-guard exec claude                  # ガード環境でClaude Codeを起動
claude-guard exec --namespace bash        # mount namespace隔離付き
```

**symlink モード** — 個別コマンドのシンボリックリンク経由で使用（exec モードが内部的に使う仕組み）

## 仕組み

```
claude-guard exec bash
         │
         ▼
┌──────────────────────────────────────┐
│  1. tmpdir を作成                      │
│  2. ポリシーの各コマンドの symlink を作成  │
│     tmpdir/bin/git → claude-guard     │
│     tmpdir/bin/ls  → claude-guard     │
│  3. PATH=tmpdir/bin                   │
│     ORIGINAL_PATH=元のPATH            │
│  4. bash を exec                      │
└──────────────────────────────────────┘
         │
   bash 内で git status と入力
         │
         ▼
┌──────────────────────────────────────┐
│  tmpdir/bin/git (symlink → guard)    │
│  argv[0] = "git" → ポリシーチェック    │
│  "status" = 許可 ✓                   │
│  ORIGINAL_PATH から本物の git を exec  │
└──────────────────────────────────────┘
```

### --namespace モード

```
claude-guard exec --namespace bash
         │
         ▼
┌──────────────────────────────────────┐
│  上記に加えて:                        │
│  5. origbin/ に本物のバイナリをコピー   │
│  6. mount namespace を作成            │
│  7. /usr/bin 等を bind-mount で上書き  │
│     → 中身は guard の symlink のみに   │
│  8. ORIGINAL_PATH=origbin/            │
└──────────────────────────────────────┘

結果:
  /usr/bin/git       → guard の symlink（直接パス指定でもガード経由）
  /usr/bin/wget      → 存在しない（policy に未定義）
  origbin/git        → 本物の git（guard がプロキシ時に使用）
```

## バイパス防止レベル

| レベル | 方法 | 防御範囲 |
|--------|------|----------|
| Level 1 | `exec` (PATHのみ) | `git`, `ls` 等のコマンド名での実行 |
| Level 2 | `exec --namespace` | `/usr/bin/git` 等の絶対パス指定も防御 |
| Level 3 | Docker + namespace | ファイルシステム全体を隔離 |
| Level 4 | AppArmor + exec | カーネルレベルで exec を強制（K8s対応） |

## YAML ポリシー

`claude-guard.yaml` をバイナリと同じディレクトリに配置：

```yaml
commands:
  git:
    # 全サブコマンド共通のオプション
    global_options:
      - "--no-pager"
      - "-C"

    # サブコマンドなし実行の可否
    allow_bare: false

    subcommands:
      status:
        allow: true
        options: ["-s", "--short", "--porcelain"]
        allow_any_args: true    # ファイルパス等の位置引数OK

      commit:
        allow: true
        options: ["-m", "--message", "-a", "--amend"]
        allow_any_args: false   # git commit somefile.txt は禁止

      clean:
        allow: false            # 明示的に拒否

  # サブコマンドなしのコマンド
  ls:
    allow_bare: true
    bare_options: ["-l", "-a", "-la", "-R", "--color"]
    subcommands: {}

  curl:
    allow_bare: true
    bare_options: ["-s", "-L", "-o", "-H", "-X", "-d", "-f"]
    subcommands: {}
```

### オプションマッチング

| 記法 | マッチ対象 |
|------|-----------|
| `-n` | `-n`, `-n5` |
| `--format` | `--format`, `--format=json` |
| `-la` | `-la` のみ |

## インストールと使用

### 方法 A: ビルドして直接使用

```bash
# ビルド
go build -ldflags="-s -w" -o claude-guard .

# 基本使用（Level 1）
./claude-guard exec bash

# namespace 隔離付き（Level 2、root必要）
sudo ./claude-guard exec --namespace bash

# Claude Code を起動
./claude-guard exec claude

# カスタムポリシー
./claude-guard exec --policy /path/to/custom.yaml bash
```

### 方法 B: setup.sh でシステムインストール

```bash
# Level 1 インストール
sudo ./setup.sh

# Level 2 パーミッションロックダウン付き
sudo ./setup.sh --lock-binaries --claude-user claude
```

### 方法 C: Docker（Level 3）

```bash
docker build -t claude-guard .
docker run -it claude-guard bash
```

### Claude Code での設定例

```bash
# exec モードで直接起動（推奨）
claude-guard exec claude

# またはシェルをガードして中で claude を実行
claude-guard exec bash
# bash 内で:
#   git status      → OK
#   git clean -fd   → BLOCKED
#   wget            → command not found
```

## exec コマンドリファレンス

```
claude-guard exec [options] <command> [args...]

Options:
  --namespace      mount namespace で /usr/bin 等を上書き（root/CAP_SYS_ADMIN 必要）
  --keep-tmpdir    tmpdir を削除しない（デバッグ用）
  --policy <path>  ポリシーファイルを指定
```

## その他のサブコマンド

```bash
claude-guard list    # ポリシーの内容を表示
claude-guard help    # ヘルプ
```

## ディレクトリ構成

exec モードで作成される tmpdir:

```
/tmp/claude-guard-XXXXXXXXXX/
├── bin/                         ← PATH がここを指す
│   ├── git -> .claude-guard-bin   (namespace時)
│   ├── ls  -> .claude-guard-bin
│   ├── cat -> .claude-guard-bin
│   ├── curl -> .claude-guard-bin
│   ├── .claude-guard-bin          guard バイナリのコピー
│   └── claude-guard.yaml          ポリシーのコピー
├── origbin/                     ← namespace時のみ、ORIGINAL_PATH がここを指す
│   ├── git                        本物の git のコピー
│   ├── ls                         本物の ls のコピー
│   └── ...
└── .exec-target                 ← ターゲットコマンドのコピー
```

## 方法 D: AppArmor（Level 4、K8s 向け）

AppArmor を使うとカーネルレベルで exec を制限できます。
Level 2（mount namespace）と組み合わせると、コンテナ外でも claude-guard 経由以外の実行を OS が強制します。

### プロファイル構成

`apparmor/claude-guard` に2つのプロファイルが定義されています：

| プロファイル | 適用対象 | 役割 |
|---|---|---|
| `claude-guard` | guard バイナリ自身 | tmpdir管理・ポリシー検証・本物バイナリ実行 |
| `claude-guard-confined` | `claude-guard exec` で起動した子プロセス | claude-guard 経由以外の exec を deny |

### exec 時の AppArmor 遷移

```
claude-guard exec claude
        │
        ▼ (Px -> claude-guard-confined)
  claude プロセス  ← "claude-guard-confined" プロファイルで動作
        │
        │  git status と実行
        ▼
  /tmp/claude-guard-*/bin/git  (symlink)
        │  AppArmor がシンボリックリンクを解決
        ├─ 非namespaceモード → /opt/claude-guard/claude-guard
        └─ namespaceモード   → /tmp/.../bin/.claude-guard-bin
        │
        ▼ (Px -> claude-guard)
  claude-guard  ← ポリシー検証
        │
        ▼ (Ux: unconfined)
  本物の /usr/bin/git
```

`deny /** x` により、claude-guard 以外のバイナリへの exec はカーネルレベルで拒否されます。

### K8s ノードへのデプロイ

```bash
# 各ノードへコピー
scp apparmor/claude-guard node:/etc/apparmor.d/claude-guard

# ノード上でプロファイルをロード
apparmor_parser -r /etc/apparmor.d/claude-guard

# 確認
aa-status | grep claude-guard
```

DaemonSet でノード全体に配布する場合の例：

```yaml
# apparmor-loader DaemonSet （抜粋）
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: apparmor-loader
spec:
  template:
    spec:
      initContainers:
      - name: install
        image: ubuntu
        command:
        - sh
        - -c
        - cp /profiles/claude-guard /etc/apparmor.d/ && apparmor_parser -r /etc/apparmor.d/claude-guard
        volumeMounts:
        - name: profiles
          mountPath: /profiles
        - name: apparmor
          mountPath: /etc/apparmor.d
      volumes:
      - name: profiles
        configMap:
          name: claude-guard-apparmor
      - name: apparmor
        hostPath:
          path: /etc/apparmor.d
```

### Pod への適用

```yaml
metadata:
  annotations:
    # コンテナ名に対して confined プロファイルを適用
    container.apparmor.security.beta.kubernetes.io/claude: localhost/claude-guard-confined
spec:
  containers:
  - name: claude
    command: ["/opt/claude-guard/claude-guard", "exec", "claude"]
```

> **注意**: `localhost/` プレフィックスはノードにロード済みのプロファイルを指定します。

### カスタマイズ

- `Px -> claude-guard-confined` の対象バイナリ（`/usr/local/bin/claude` 等）はパスを環境に合わせて調整
- `--namespace` モードを使わない場合は `mount` ルールを削除可能
- `@{HOME}/**  rwk` のスコープはワークスペースのパスに合わせて絞ることを推奨

## セキュリティ上の注意

- ポリシーファイルは Claude ユーザーが編集できないようにしてください（`root:root 644`）
- `--namespace` モードが最も強力なバイパス防止を提供します
- シェルのビルトイン（`cd`, `echo`, `export` 等）はこの仕組みでは制御できません
- コンテナ運用（Level 3）を組み合わせるとさらに堅牢です
- **policy に定義していないコマンドは一切使えません**（ポジティブリスト方式）
