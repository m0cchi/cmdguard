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

## セキュリティ上の注意

- ポリシーファイルは Claude ユーザーが編集できないようにしてください（`root:root 644`）
- `--namespace` モードが最も強力なバイパス防止を提供します
- シェルのビルトイン（`cd`, `echo`, `export` 等）はこの仕組みでは制御できません
- コンテナ運用（Level 3）を組み合わせるとさらに堅牢です
- **policy に定義していないコマンドは一切使えません**（ポジティブリスト方式）
