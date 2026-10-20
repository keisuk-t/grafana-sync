# Grafana 13 バージョン依存性調査

調査日: 2026-10-20。比較対象は Grafana の公式タグ
`v12.2.0`、`v13.0.0`、`v13.2.4`。
Unified Storage のデータソース移行・dual-write は対応範囲から除外する。

## 結論

Grafana 13.2.4 の標準 SQL 構成で、現行の4テーブル転送方式が動作することを
実機テストで確認した。Grafana の Go パッケージを直接リンクしていないため、
Grafana 本体の Go バージョンに合わせた依存ライブラリ更新は不要。
バージョン指定フラグも不要。現行実装は実 DB のカラム名・型・バイナリ属性を
検査し、`TEXT` と `MEDIUMTEXT` の両方を扱えるため、同じ処理を使用する。
バージョン指定によるスキーマの仮定や自動 DDL は追加しない。

## 依存性と変更点

| 項目 | 確認結果 | この CLI への影響 |
| --- | --- | --- |
| `org` | 比較対象の migration ファイルに差分なし | 転送方式の変更は不要と判断 |
| `data_source` | 13.0.0 で MySQL の `json_data` を `MEDIUMTEXT` に変更。13.0.0 と13.2.4の該当 migration は同一 | 現行の `decodeCell` は `TEXT` と `MEDIUMTEXT` に対応。64 KiB を超える JSON を追加検証する |
| `data_keys`・`secrets` | 該当 migration ファイルに差分なし | 鍵の主キー `name` とバイナリ `encrypted_data` の前提を維持 |
| datasource の秘密情報 | 13.2.4 でも SQL の SecretsStore に `org_id`・データソース名・datasource 種別で保存 | 現行の抽出条件と一致 |
| 暗号化 | envelope encryption の無効化分岐が削除。旧形式の復号分岐は残存 | 暗号文の無変換コピー方針を維持。共通の `security.secret_key` または同じ KMS 設定が引き続き必要 |
| Grafana Secrets Manager | 従来の秘密情報サービスとは別の保存機構が存在 | 外部・共有秘密情報まで4テーブルで移行できるとは扱わない |
| Unified Storage | 13.0 で dashboard/folder が自動移行。13.2.4 には datasource の移行機構もあるが既定は無効 | ユーザー指定により対応対象外 |
| HTTP API | 数値 ID を使う旧 datasource API が13で既定無効。UID APIへ移行 | SQL を使う CLI 本体には影響なし。本体検証では UID API を使う |
| プラグイン | 13.0 は React 19 へ移行し、公式ガイドはアップグレード前のプラグイン更新を要求 | 移行先には Grafana 13 と互換性のあるデータソースプラグインを別途導入する。この CLI はプラグインをインストール・更新しない |
| サーバー起動 | `grafana-server`・`grafana-cli` コマンドが13で削除 | 本体検証の起動は `grafana server` を使う |
| DB 要件 | 13.2.4 の文書は MySQL 8.0+／PostgreSQL 12+。公式サポート期間の条件もある | 既存テストの MySQL 8.4／PostgreSQL 17 は下限条件を満たす |
| Go | Grafana 13.0.0 は1.25.9、13.2.4 は1.26.9。CLI は独立した Go 1.26.0 モジュール | Grafana 本体をビルドしない場合、その Go 要件は CLI の直接依存ではない |

13.0.0 は Git Sync を有効にした12系からの更新時のデータ損失問題により
配布から取り下げられている。実機検証には13.0系の修正版または13.2.4を使用する。
この問題は Grafana 自身のアップグレード処理であり、この CLI による
データソースの転送を Grafana 全体のアップグレードの代用にしてはならない。

## 自動検証

`make grafana-integration` で Grafana 本体を使う検証を実行する。
既定は13.2.4。`GRAFANA_TEST_VERSION` で検証する本体イメージのタグを指定する。
CI では12.2.0・13.0.11・13.2.4を対象にする。

1. Grafana 13 の実際の migration が作成した MySQL／PostgreSQL スキーマを使い、
   export と各 import モードを確認する。同じエンジンと相互移行を含める。
2. `json_data` に64 KiBを超える値を保存し、欠落・切り詰めなく往復できることを確認する。
   古い MySQL の `TEXT` カラムへの逆移行では容量制限が残るため、
   12↔13 の無条件な互換性は主張しない。
3. 共通 secret_key の本体を起動し、移行した暗号化 HTTP ヘッダーが
   UID ベースのデータソースプロキシ経由で復号・送信されることを確認する。
   現行鍵と無効化済み鍵、sync 時の既存鍵保持も確認する。

専用 Compose プロジェクトの DB を Grafana 本体の migration で初期化し、
API で作成したデータソースを使う。CLI の export/import、全4経路・全3モード、
insert-only/sync の再実行、sync の dry-run による不変性を確認する。
Grafana は CLI の実行前に停止し、転送後に再起動して UID API とプロキシで検証する。
sync が追加した鍵の `active=false` もスナップショット比較で確認する。
テストは隔離した DB と固定のテスト用 secret_key を使用し、本番 DSN は受け付けない。

KMS／Enterprise 固有の暗号方式はこのテストでは検証しない。

### 今回の実行結果

- `make grafana-integration`: Grafana 13.2.4、MySQL 8.4／PostgreSQL 17 の
  全4経路 × 3モード（12ケース）が成功。全カラムの値と暗号文、大きな JSON、
  現行鍵／無効化済み鍵による秘密 HTTP ヘッダーの復号を確認した。
- `make lint test build` 相当の lint・race detector 付き単体テスト・ビルド、および
  既存の `make integration` が成功。
- 新しく追加した12.2.0／13.0.11の実機テストは CI 対象として設定したが、
  今回のローカル実行では未実行。12↔13の異なる本体バージョン間の移行も未検証。

検証用 HTTP サーバーは小さな Go バイナリから専用イメージを作り、
Compose 内の通信だけで使用する。クラウド環境がコンテナへプロキシ変数を
自動注入する場合に備え、テストサービス名を `NO_PROXY` に指定する。
Buildx の状態は一時ディレクトリへ保存し、読み取り専用のホームへの書き込みを避ける。

## 一次資料

- [13.0 アップグレードガイド](https://github.com/grafana/grafana/blob/v13.2.4/docs/sources/upgrade-guide/upgrade-v13.0/index.md)
- [13.2 アップグレードガイド](https://github.com/grafana/grafana/blob/v13.2.4/docs/sources/upgrade-guide/upgrade-v13.2/index.md)
- [DB 要件](https://github.com/grafana/grafana/blob/v13.2.4/docs/sources/setup-grafana/installation/_index.md)
- [data_source migration](https://github.com/grafana/grafana/blob/v13.2.4/pkg/services/sqlstore/migrations/datasource_mig.go)
- [org migration](https://github.com/grafana/grafana/blob/v13.2.4/pkg/services/sqlstore/migrations/org_mig.go)
- [暗号化テーブル migration](https://github.com/grafana/grafana/blob/v13.2.4/pkg/services/sqlstore/migrations/secrets_mig.go)
- [datasource の秘密情報保存](https://github.com/grafana/grafana/blob/v13.2.4/pkg/services/datasources/service/datasource.go)
- [秘密情報の暗号化・復号](https://github.com/grafana/grafana/blob/v13.2.4/pkg/services/secrets/manager/manager.go)
- [Unified Storage の移行対象と既定値](https://github.com/grafana/grafana/blob/v13.2.4/pkg/storage/unified/migrations/README.md)
- [datasource のストレージ選択](https://github.com/grafana/grafana/blob/v13.2.4/pkg/registry/apis/datasource/register.go)
- [機能フラグ既定値](https://github.com/grafana/grafana/blob/v13.2.4/pkg/services/featuremgmt/registry.go)
