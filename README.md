# misskey-s3-compactor

Misskey が S3 (または S3 互換サービス) に保存しているメディアファイルを定期的にスキャンし、省力化可能なものだけを再圧縮して元のキーに置き換える Kubernetes CronJob 向けサーバアプリケーション。停止型の全件バッチではなく、すでに圧縮済みのオブジェクトはメタデータのマーカーでスキップするため、毎晩の差分運用に適した設計になっている。

> 本ツールは Misskey 本体を改変せず、S3 上のオブジェクト本体のみを置換する。前提・運用リスクは [制限事項・リスク](#制限事項リスク) を必ず確認すること。

## 概要

```
     ┌────────────────── K8s CronJob (nightly) ──────────────────┐
     │                                                             │
     │   ListObjectsV2 ─► HeadObject ─► skip if marker=="done"      │
     │       │                                                     │
     │       ▼                                                     │
     │   download → recompress (per-format) → ratio check → upload │
     │       │                                                     │
     │       └─► tag object with x-amz-meta-compactor=done         │
     └─────────────────────────────────────────────────────────────┘
                              S3 bucket (Misskey)
```

* **DB を触らない** — Misskey の `driveFile` テーブル(`fileSize` / `md5` など)は更新しない。ファイル本体だけを同じキー・同じ Content-Type で置換するため、Misskey 側の配信は維持され、キャッシュヘッダなども壊れない一方で DB 上のサイズ表記は実サイズとずれる点に留意。
* **形式保持** — JPEG/PNG/WebP/GIF/MP4/WebM/MOV/MKV を検出し、各々の元フォーマットを保ったまま最適化された圧縮方式を選択する。静止画を WebP/AVIF に変換するような破壊的フォーマット変更は行わない。
* **動画は HEVC 再エンコード** — `ffmpeg libx265` で再圧縮。`ffprobe` で `hevc` と判明したものはスキップ。
* **冪等** — 置換成功時に `x-amz-meta-compactor=done` をメタに付与し、次回実行は同一オブジェクトを処理しない(`SKIP_MARKED=false` で無効化可能)。
* **AWS S3 / S3 互換両対応** — `S3_ENDPOINT` と `S3_USE_PATH_STYLE` の組み合わせで MinIO / SeaweedFS / Cloudflare R2 / Backblaze B2 などを切り替え可能。
* **安全装置** — `MIN_SAVING_RATIO` 以下の削減は置換しない、`MAX_OBJECT_BYTES` 超はスキップ、`DRY_RUN=true` で差分だけ可視化、SIGTERM/SIGINT 受信時は即時終了。

## 動作環境

ランタイムコンテナには以下の外部バイナリが必要。`Dockerfile` では alpine の [community] レポジトリから導入済み。

| バイナリ | 用途 |
| --- | --- |
| `jpegoptim` | JPEG 再圧縮 |
| `oxipng` | PNG ロスレス最適化 |
| `libwebp-tools` (`cwebp`) | WebP 再エンコード |
| `gifsicle` | GIF ロスレス最適化 |
| `ffmpeg` / `ffprobe` | 動画の HEVC 再エンコードと事前コーデック判定 |

## クイックスタート

### 1. 設定を埋める

`deploy/secret.yaml` はプレースホルダを含んだテンプレート。実際のバケット名と認証情報に書き換えるか、[SealedSecrets](https://github.com/bitnami-labs/sealed-secrets) / [ExternalSecrets](https://external-secrets.io/) / [sops](https://github.com/getsops/sops) などの仕組みに置き換えること。

```yaml
# deploy/secret.yaml (抜粋)
stringData:
  S3_BUCKET: "misskey-media"
  AWS_ACCESS_KEY_ID:     "AKIAxxxxxxxxxxxxxxxx"
  AWS_SECRET_ACCESS_KEY: "xxxxxxxxxxxxxxxxxxxxxxxxxx"
  AWS_SESSION_TOKEN: ""   # STS 一時認証を使う場合のみ
```

`deploy/configmap.yaml` で Misskey の実際のバケットレイアウトに合わせる:

```yaml
data:
  S3_PREFIX:         "misskey/"   # Misskey の S3 prefix と一致させる
  S3_REGION:         "us-east-1"
  S3_ENDPOINT:       ""           # AWS S3 本体なら空文字、それ以外は https://s3.example.com 等
  S3_USE_PATH_STYLE: "true"       # MinIO/R2 等の S3 互換サービスでは true
```

イメージタグ、実行時刻、リソース制限は `deploy/cronjob.yaml` で調整する。既定では毎晩 03:00 JST (18:00 UTC) に起動し、1 回最大 6 時間で中断される。

### 2. デプロイ

```sh
kubectl apply -k deploy/
```

初回実行前に対象範囲と期待削減量を確認したい場合は、`deploy/configmap.yaml` の `DRY_RUN` を `"true"` にしてから apply する。ログは JSON 形式で標準出力に出るので `kubectl logs job/misskey-compactor-XXXXXXXX` などで確認。満足すれば `DRY_RUN="false"` に戻して再 apply する。

### 3. スケジュール実行を待ちたくない場合

```sh
kubectl -n misskey-compactor create job --from=cronjob/misskey-compactor misskey-compactor-manual
```

### Docker Compose でデプロイする場合

Kubernetes を使わず Docker Compose でも同等の機能を利用できる。コンテナ内で [`supercronic`](https://github.com/aptible/supercronic)(コンテナ向け cron) を動かし、K8s CronJob と同じスケジュール実行・非 root・読み取り専用ルート FS・tmpfs `/tmp` を再現している。

```sh
cd deploy/compose

# 1. 設定ファイルを用意
cp .env.example .env
#    .env を編集して S3 バケット名・認証情報・圧縮パラメータを設定する

# 2. 起動(スケジューラ常駐)
docker compose up -d

# 3. ログ確認
docker compose logs -f compactor

# 4. 手動で 1 回だけ実行したい場合
docker compose run --rm --entrypoint /usr/local/bin/compactor compactor

# 5. 停止
docker compose down
```

実行スケジュールは `deploy/compose/crontab` で変更する(既定は K8s と同じ `0 18 * * *` = 毎晩 03:00 JST)。`.env` に記述する環境変数は [設定リファレンス](#設定リファレンス) と同じ。

## 設定リファレンス

すべて環境変数で制御する。CI ビルド後は `kustomize edit set image` でタグが自動で `deploy/kustomization.yaml` に固定されるため、イメージ参照を手動更新する必要はない。

### S3 接続

| 変数 | 既定値 | 説明 |
| --- | --- | --- |
| `S3_BUCKET` | *(必須)* | 対象バケット名 |
| `S3_PREFIX` | `""` | スキャン対象のキー prefix。Misskey の設定と一致させる |
| `S3_REGION` | `us-east-1` | リージョン |
| `S3_ENDPOINT` | `""` | カスタムエンドポイント。空なら AWS S3 本体を想定 |
| `S3_USE_PATH_STYLE` | `false` | MinIO/R2 等の path-style アドレッシング。`true` 推奨 |
| `AWS_ACCESS_KEY_ID` | *(Secret)* | 認証キー |
| `AWS_SECRET_ACCESS_KEY` | *(Secret)* | 認証シークレット |
| `AWS_SESSION_TOKEN` | *(Secret)* | STS 一時クレデンシャルを使用する場合のみ |

### 画像圧縮

| 変数 | 既定値 | 説明 |
| --- | --- | --- |
| `JPEG_QUALITY` | `80` | 1-100。低いほど高圧縮 |
| `PNG_STRIP_METADATA` | `true` | `true` でメタデータも削除して最適化 |
| `WEBP_QUALITY` | `80` | 1-100 |
| `GIF_FLAGS` | `--optimize=3` | `gifsicle` に渡す追加フラグ |

### 動画圧縮

| 変数 | 既定値 | 説明 |
| --- | --- | --- |
| `VIDEO_CODEC` | `libx265` | 任意の ffmpeg ビデオエンコーダ(`libx264` にするなど) |
| `VIDEO_PRESET` | `medium` | x265 preset(`ultrafast`〜`placebo`) |
| `VIDEO_CRF` | `26` | 0-51。大きいほど高圧縮・低品質 |
| `VIDEO_PIXFMT` | `yuv420p` | ピクセルフォーマット |

### 挙動

| 変数 | 既定値 | 説明 |
| --- | --- | --- |
| `DRY_RUN` | `false` | `true` で置換を止めて差分ログだけ出力 |
| `SKIP_MARKED` | `true` | `MARKER_KEY` が `done` のオブジェクトをスキップ |
| `MARKER_KEY` | `x-amz-meta-compactor` | 置換済みマーカーとして付与するメタキー |
| `MIN_SAVING_RATIO` | `0.05` | 0-1。この割合以上の削減がない場合は置換しない |
| `MAX_OBJECT_BYTES` | `2147483648` (2 GiB) | このサイズ超のオブジェクトはスキップ。`0` で無制限 |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |

## 運用のヒント

| 目的 | 設定 |
| --- | --- |
| すでに圧縮済みのオブジェクトも再評価する | `SKIP_MARKED=false` |
| 全件を未圧縮状態に戻さずに再評価する | `SKIP_MARKED=true` のまま変更しない |
| 並列性を上げる | 単一ワーカーのみ。CPU と IO は CronJob の `resources` で制御する |

## アーキテクチャ

### 処理フロー

1. **S3 列挙** — `ListObjectsV2` で `S3_PREFIX` 配下の全キーを 1 頁ずつ舐める。DB 依存なし。
2. **HeadObject** — Content-Type と既存メタを取得。マーカーが `done` なら即座にスキップ。Content-Type が未設定なら拡張子から推定。
3. **ダウンロード** — 一時ファイルに保存。`MAX_OBJECT_BYTES` 超過は事前に除外。
4. **再圧縮** — Content-Type に応じて圧縮方式を選択(JPEG→`jpegoptim`、PNG→`oxipng` 等)。動画は `ffprobe` で既に HEVC かを判定し、そうならスキップ。
5. **効果判定** — 圧縮後サイズが `(1 - MIN_SAVING_RATIO)` 倍未満にならない場合は置換しない。
6. **アップロード** — 同じキーに対して `PutObject`。元の Content-Type を保ち、既存メタを引き継ぎつつ、最後に `MARKER_KEY=done` を付与する(`MetadataBehavior=REPLACE` 相当)。
7. **集計** — スキャン / スキップ / 圧縮 / 置換 / 削減バイト数などを構造化ログで出力して終了。

### フォーマット別圧縮方式

| Content-Type | 圧縮方式 | 備考 |
| --- | --- | --- |
| `image/jpeg` | `jpegoptim --max=N --all-progressive --strip-com` | 可逆; プログレッシブ化 |
| `image/png` | `oxipng -o 3 --strip=(all\|safe)` | ロスレス |
| `image/webp` | `cwebp -q N` | 可逆 |
| `image/gif` | `gifsicle --optimize=3` | ロスレス最適化 |
| `video/*` | `ffmpeg -c:v libx265 -preset P -crf N -c:a aac -b:a 128k +faststart` | 音声は AAC 128 kbps に固定 |
| `image/avif`, `image/bmp`, `image/svg+xml` 等 | *(スキップ)* | サポート対象外 |

### オブジェクトメタデータ更新の仕組み

`PutObject` で置換する際、`HeadObject` で取得した既存のメタデータ(ユーザカスタムメタ・`x-amz-meta-*` 等)を新しいメタに含め直すことで、元の情報を保持したまま最後に `x-amz-meta-compactor=done` だけを追加する。Content-Type や Cache-Control などの標準 HTTP ヘッダは `ContentType` フィールドで明示的に引き継ぐ(現状は Content-Type のみ引き継ぐ実装)。

## 制限事項・リスク

* **Misskey の DB サイズ欄は不整合する** — DB の `fileSize` や `md5` は更新しない。UI の表示サイズ、容量集計、一部のリモート連合で差分が生じ得る。Misskey の動作そのもの(再生・サムネイル・公開範囲)には影響しないことを確認済みだが、Misskey のバージョンアップによってチェックが導入された場合は挙動が変わる可能性がある。自己責任で導入すること。
* **元に戻せない** — バックアップは作らない(`置換のみ` 設定)。オリジナルの復元は S3 のバージョニング機能や別の退避先を運用側で用意すること。
* **動画再エンコードは不可逆** — HEVC 再エンコードで画質劣化が起きる。`VIDEO_CRF` と `VIDEO_PRESET` を慎重に調整すること。はじめは `DRY_RUN=true` と `LOG_LEVEL=debug` でサンプリングを推奨。
* **クライアントキャッシュ** — キーは変わらないため、CDN やブラウザが古いバージョンをキャッシュしたままになる場合がある。必要に応じて該当キーのキャッシュを無効化すること。
* **レートリミット・巨大バケット** — 本ツールは単一ワーカーで順次処理する設計のため、極端に巨大なバケット(数百万件以上)ではスキャンだけで対象数に比例した時間がかかる場合がある。必要に応じて `S3_PREFIX` を分割し、複数の CronJob を prefix ごとに並設するか、Job の並列化を自前で導入すること。
* **外部バイナリ依存** — `oxipng` などが alpine の community レポジトリに入っているが、ランタイムイメージの alpine バージョンを上げた際にバイナリが API 変動で動作しなくなる可能性がある。イメージの更新時は必ず CI を通すこと。

## トラブルシューティング

### `config invalid: S3_BUCKET must be set`

Secret または ConfigMap が CronJob のコンテナに読み込まれていない。`envFrom` の `secretRef`/`configMapRef` 名と実際のリソース名が一致しているか確認する。MinIO/R2 等では `S3_USE_PATH_STYLE=true` の設定漏れも多く、この場合 403/400 系のネットワークエラーが Head/GetObject で出る。

### `missing required binaries on $PATH`

カスタムイメージをビルドしている場合、`jpegoptim`/`oxipng`/`libwebp-tools`/`gifsicle`/`ffmpeg` がインストールされていない。提供している `Dockerfile` をそのまま使えば解決する。

### ローカルで雑に試す

```sh
go build -o /tmp/compactor ./cmd/compactor

S3_BUCKET=my-misskey-media \
S3_PREFIX=misskey/ \
S3_REGION=us-east-1 \
S3_ENDPOINT=http://localhost:9000 \
S3_USE_PATH_STYLE=true \
AWS_ACCESS_KEY_ID=minio \
AWS_SECRET_ACCESS_KEY=minio123 \
DRY_RUN=true \
LOG_LEVEL=debug \
/tmp/compactor
```

実行後は `{"level":"info","msg":"scan complete",...}` の一行が出て終了する。エラーは JSON 1 行に集約されるため、`jq` などで `select(.level=="ERROR")` で拾える。

### 全件再圧縮したい

一時的に `SKIP_MARKED=false` にして apply し直す。一度 `done` マーカーを付けてしまったオブジェクトも含めて再評価される。

### 対象件数を知りたい

`LOG_LEVEL=debug` にすると 1 件ごとのスキップ理由とサイズ推移が出る。件数だけなら既定の `info` でも `scanned` と `replaced` が最終行の JSON に出力される。

## CI / イメージビルド

`.github/workflows/build-and-publish.yml` が次を自動で行う:

1. `gofmt -l` / `go vet` / `go test -race` の失敗があれば差し戻す
2. `docker/build-push-action` で `linux/amd64` + `linux/arm64` のマルチプラットフォームイメージをビルド
3. GHCR (`ghcr.io/<owner>/<repo>`) に push。タグ戦略は:
   * `main`/`master` へのマージ → `latest` + `sha-<short>` + `main`
   * `v1.2.3` 形式のタグ → `1.2.3` + `1.2` + `1`
   * プルリク → `pr-<number>`(push はしない、ビルドのみ)
4. ビルドしたイメージの digest を `kustomize edit set image` で `deploy/kustomization.yaml` に固定し、コミットを push する([skip ci] 付きなので再帰はしない)

既定の制限:
* `permissions` を `contents: read` + `packages: write` に絞っている
* イメージの push は `pull_request` イベントでは行わない(フォークからの PR でも secrets が漏洩しない)
* SBOM と provenance を両方有効化済み(供給チェーンの完全性担保)

ローカルで完全に同じビルドを再現したい場合は:

```sh
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --tag ghcr.io/$(pwd | xargs basename):local \
  --load \
  .
```

## ローカル開発

```sh
git clone https://github.com/<owner>/misskey-s3-compactor.git
cd misskey-s3-compactor

go test -race ./...
go run ./cmd/compactor  # S3_BUCKET と認証情報が無いと config invalid で止まる

# MinIO を docker で立てておけば統合テストも可能
docker run -d --rm --name minio -p 9000:9000 \
  minio/minio server /data
```

内部構成:

| パス | 役割 |
| --- | --- |
| `cmd/compactor/main.go` | エントリポイント。設定読込 → バイナリチェック → スキャン → 終了コードで結果報告 |
| `internal/config` | 環境変数のパーサとバリデータ。基本値はここを見ればよい |
| `internal/s3client` | AWS SDK for Go v2 を使って S3 クライアントを構築。endpoint/path-style 切替を収める |
| `internal/walker` | `ListObjectsV2` の pagination を抽象化したイテレータ |
| `internal/compress` | 各圧縮バイナリのラッパ群。`Result` に original/output/skip/error を詰める |
| `internal/processor` | 1 オブジェクトの download→compress→upload とステータス管理 |
| `internal/tools` | 必須外部バイナリの有無を検証 |
| `Dockerfile` | golang ビルドステージ + alpine ランタイム(非 root) |
| `deploy/` | Kustomize マニフェスト一式 |

## 貢献

バグ報告・改善提案は Issue にて歓迎する。破壊的変更は事前に Issue で議論してから PR を出すこと。CI は以下を全て通すことが必須:

```sh
gofmt -l .              # 差分が無いこと
go vet ./...            # 警告が無いこと
go test -race ./...     # テストが通ること
kustomize build deploy  # manifest が有効な YAML であること
```
