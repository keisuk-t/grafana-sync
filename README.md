# grafana-sync

Grafana の `org`・`data_source` と、データソースの暗号化情報に必要な `data_keys`・`secrets` を JSON にエクスポートし、別のデータベースへインポートする Go 製 CLI です。MySQL／PostgreSQL に対応します。ダッシュボード・ユーザー・アラート・その他のテーブルは移行しません。

| テーブル | 移行する行 |
| --- | --- |
| `org` | 全件 |
| `data_source` | 全件 |
| `data_keys` | 全件（無効化済みの鍵も含む） |
| `secrets` | `type = 'datasource'` かつ `org_id`・`namespace` が移行するデータソースの `org_id`・`name` に一致する行のみ |

`data_keys` は複数の用途で共有されるため、暗号文を解析せず、古い鍵も含めて保持します。`secrets` の別用途の行や孤立した行はエクスポートしません。

## セットアップ

Grafana 12／13 の標準 SQL ストレージを対象とします。データソースの Unified Storage 移行・dual-write を使用する構成は対象外です。バージョン番号による処理分岐は不要なため、Grafana バージョン指定フラグはありません。実際のカラム構成・型を検査して転送します。[Grafana 13 の依存性調査](docs/grafana-13-compatibility.md)も参照してください。

Grafana 13 では MySQL の `data_source.json_data` が `MEDIUMTEXT` に拡張されています。本ツールは対応していますが、Grafana 12.2.0 などの旧 `TEXT` カラムへ64 KiBを超える値を戻すことはできません。異なる Grafana バージョン間での無条件な互換性は保証しません。

Go 1.26 以上が必要です。`sync` の MySQL 対応は 8.0 以上です（論理キーの事前検査に `JSON_TABLE` を使用）。実 DB テストには Docker Engine と Docker Compose v2 も必要です。

```sh
go mod download
make build
./bin/grafana-sync --help
```

## エクスポート

MySQL の接続文字列は [go-sql-driver/mysql の DSN 形式](https://github.com/go-sql-driver/mysql#dsn-data-source-name)を使用します。

```sh
# 実際の資格情報はシークレット管理機能等から環境変数へ渡してください。
export GRAFANA_SYNC_DSN='grafana:password@tcp(db.example:3306)/grafana?charset=utf8mb4&tls=true'
./bin/grafana-sync export -db mysql -file grafana.dump.json
```

PostgreSQL は [pgx の接続文字列](https://pkg.go.dev/github.com/jackc/pgx/v5/pgconn#ParseConfig)を使用します。

```sh
export GRAFANA_SYNC_DSN='postgres://grafana:password@db.example:5432/grafana?sslmode=verify-full'
./bin/grafana-sync export -db postgres -file grafana.dump.json
```

接続文字列をコマンドライン引数に含めないため、DSN は環境変数からのみ読み込みます。`-dsn-env SOURCE_DSN` で変数名を変更できます。処理時間の上限は `-timeout 5m` で指定できます（既定値は 2 分）。MySQL の日時は `parseTime=true`、UTC として扱います。Grafana が UTC の日時を保存していることを前提とします。

MySQL のセッションは strict SQL mode に固定し、文字列の暗黙の切り詰めやゼロ日時の挿入を拒否します。既存のゼロ日時は、値を保持できないためエクスポート時に拒否します。

全対象テーブルを読み取り専用の repeatable-read トランザクションで取得します。出力先が存在する場合は上書きせず失敗します。ファイルは UNIX のパーミッション `0600` で作成します。

## インポート

```sh
export TARGET_DSN='postgres://grafana:password@target.example:5432/grafana?sslmode=verify-full'
./bin/grafana-sync import -db postgres -dsn-env TARGET_DSN -file grafana.dump.json
```

- **事前に Grafana と他の DB 書き込みクライアントを停止し、移行先 DB 全体をバックアップしてください。** [Grafana のバックアップ手順](https://grafana.com/docs/grafana/latest/administration/back-up-grafana/)を参照してください。
- 移行先は Grafana のマイグレーションを実行済みで、4 対象テーブルのカラム名とバイナリカラムが移行元と一致する必要があります。DDL・スキーママイグレーションは実行しません。同一 Grafana バージョンのスキーマを推奨します。`data_keys.name` が文字列の主キー、`encrypted_data` が BLOB／BYTEA である Grafana の SQL スキーマを対象とします。関連テーブルがない旧スキーマや外部の secrets ストアは対象外です。
- **既定の `--mode require-empty` では、移行先の `org`・`data_source`・`data_keys`・`secrets` がすべて空でなければなりません。** `secrets` に別用途の行しかない場合も拒否します。Grafana の通常の初回起動では既定の `org` が作成されるため、そのままではこのモードでインポートできません。既存 DB への非破壊的な追加には後述の `insert-only`、データソースの削除を含む同期には `sync` を使用してください。
- ID を保持して `org` → `data_keys` → `data_source` → `secrets` の順に挿入します。数値 ID・鍵名の重複、対応する組織がない `org_id`、移行するデータソースに対応しない `secrets` の行を拒否します。
- 全対象テーブルを 1 トランザクションで処理し、失敗時は行の変更をロールバックします。MySQL はすべての対象テーブルが InnoDB であることを確認します。
- PostgreSQL の関連 ID シーケンスはトランザクション内の `ALTER SEQUENCE ... RESTART` で更新します（シーケンス所有権が必要）。MySQL の `AUTO_INCREMENT` は明示 ID の挿入によって進みます。MySQL の採番カウンターはロールバック後も進む場合があります。
- 対象外のユーザー所属情報（`org_user`）などは作成・更新しません。本ツールは完全な Grafana バックアップ／復元ではありません。移行先の対象外テーブルに残る ID 参照や所属・権限は運用者が確認してください。
- 読み込みは全件をメモリに保持します。巨大なデータセット向けのストリーミング処理は未対応です。

### 既存 DB への追加（insert-only）

```sh
./bin/grafana-sync import -db postgres -dsn-env TARGET_DSN -file grafana.dump.json --mode insert-only
# import completed: mode=insert-only inserted=3 skipped=4
```

`insert-only` は既存行を変更せず、不足している主キーの行だけ追加するモードです。`append` や `upsert` と異なり、既存行の更新・置換は行いません。

- `org`・`data_source`・`secrets` は `id`、`data_keys` は `name` で重複を判定します。照合は移行先 DB の型・照合順序に従います。
- 同じ主キーが存在すれば、他フィールドの内容が異なっても行全体をスキップします。ID の再採番・組織へのマッピング変更・フィールド単位のマージもしません。
- 別 ID で同じデータソース名を使う場合など、主キー以外の一意制約に衝突すると失敗し、今回の追加行をすべてロールバックします。`INSERT IGNORE` や更新付き upsert は使用しません。
- 新しく追加する秘密情報の組織・データソース参照は移行先でも確認します。既存 ID の行に別の組織・名前が保存されていて参照が成立しない場合は、既存行を更新する代わりに失敗します。
- 同じスナップショットを再実行しても、追加済みの主キーはスキップします。`inserted`／`skipped` は4テーブルを合計した行数です。追加のないテーブルの PostgreSQL シーケンスは変更せず、追加がある場合も既存の採番位置を巻き戻しません。
- 入力には引き続き参照整合性を満たす version 2 のスナップショットを使用します。DB の既存行を参照するためだけに親行を入力から省略する形式には対応しません。

**同じ ID が同じ論理リソースを指す、互換性のある DB 間で使用してください。** 名前・暗号鍵・秘密情報が異なる既存行も優先されるため、追加した設定が元と同じ動作になる保証はありません。特に同じ `data_keys.name` に異なる暗号鍵が保存されている場合、既存鍵を変更しないため新しい暗号文を復号できないことがあります。事前のバックアップ・書き込み停止と、移行後の動作確認が必要です。

### データソースの同期（sync）

```sh
./bin/grafana-sync import -db postgres -dsn-env TARGET_DSN -file grafana.dump.json --mode sync --dry-run
./bin/grafana-sync import -db postgres -dsn-env TARGET_DSN -file grafana.dump.json --mode sync --allow-delete
```

`sync` は明示的に `--dry-run` または `--allow-delete` が必要です。`--dry-run` は合計・テーブル別の追加／更新／削除／変更なし／スキップ件数を表示するだけで、DB の行・シーケンスを変更しません。適用時はロックを取得して差分を再計算します。論理キーは移行先の照合順序で事前検査しますが、予定件数や dry-run の成功は、DB 独自の制約・トリガーを含む将来の適用成功を保証しません。既定モードや `insert-only` にこれらのフラグは使用できません。

**削除範囲は JSON の `org.rows` に含まれる組織に限定されます。** 通常のエクスポートは全組織を含むため、出力ファイルから対象組織のデータソース行を手動で省略すると、その行は削除対象になります。差分ファイルとしては扱いません。`org.rows` が空の場合は拒否します。対象組織の `data_source.rows` が空なら、その組織の全データソースを削除します。

| テーブル | 同期時の動作 |
| --- | --- |
| `org` | 未登録 ID の行のみ追加。既存行を更新・削除しない |
| `data_source` | 対象組織内で JSON の ID・値に一致するよう追加・更新・削除。別 ID で同名の旧行は先に削除 |
| `secrets` | 対象組織の `type = 'datasource'` の行のみ追加・更新・削除。JSONにない孤立したdatasource用secretも削除。その他の用途・対象外組織は保持 |
| `data_keys` | JSONにない既存鍵を保持。同名の暗号鍵本体・プロバイダー・スコープ・ラベルが異なれば失敗。新規鍵は復号用として `active=false` で追加し、既存鍵のactive状態は保持 |

名前・UID の入れ替えは一時キーで衝突を避けて適用します。全変更は1トランザクションでロールバックできますが、移行先のダッシュボード・権限・アラートなど**対象外テーブルの ID／UID 参照を移し替えません**。DB の外部キーや独自トリガーが削除を禁止する場合は変更を強行せず失敗します。適用後は Grafana で対象リソースと暗号化資格情報の動作を確認してください。鍵の有効化・整理・ローテーションは Grafana 側で行ってください。

### 機密情報と暗号化

JSON には `password`、`basic_auth_password`、`secure_json_data` などを含む全カラムが入ります。**出力ファイル自体は暗号化されません。** 安全に保管・転送し、リポジトリへコミットしないでください。`.gitignore` はルートの `*.dump.json` を除外しますが、任意のパスのファイルは除外しないため注意してください。

暗号文と暗号化されたデータ鍵はそのままコピーし、復号／再暗号化はしません。`data_keys.encrypted_data` のバイナリは Base64 で保存し、インポート時に元のバイト列へ戻します。通常のインポートでは鍵の `name`・`label`・`active`・`scope`・`provider`・日時も保持します。`sync` 時は上記のとおり有効鍵選択を保護します。無効化された鍵でも既存の暗号文の復号に必要な場合があるため、削除・再生成しません。

**DB 以外の鍵は移行できません。** 移行先にも元と同じ `[security] secret_key`（`GF_SECURITY_SECRET_KEY`）を設定してください。KMS 利用時は元の鍵、プロバイダー設定・認証情報・アクセス権も必要です。鍵のローテーションや暗号方式の変更は Grafana の機能で別途実施してください。[Grafana の DB 暗号化の説明](https://grafana.com/docs/grafana/latest/setup-grafana/configure-security/configure-database-encryption/)を参照してください。

`secrets` の抽出条件は [Grafana のデータソースサービス](https://github.com/grafana/grafana/blob/v12.2.0/pkg/services/datasources/service/datasource.go)の参照方法に従います。テーブル構造は [暗号化テーブルのマイグレーション](https://github.com/grafana/grafana/blob/v12.2.0/pkg/services/sqlstore/migrations/secrets_mig.go)で確認できます。

DB エラーに機密カラム値が含まれる場合があるため、接続・書き込みエラーの詳細は CLI に表示しません。テーブル／行番号と DB 側のログを使って調査してください。

## JSON 形式（version 2）

```json
{
  "version": 2,
  "org": {
    "columns": ["id", "name"],
    "rows": [["1", "Main Org."]]
  },
  "data_source": {
    "columns": ["id", "org_id", "name"],
    "rows": [["2", "1", "Prometheus"]]
  },
  "data_keys": {
    "columns": ["name", "encrypted_data"],
    "binary_columns": ["encrypted_data"],
    "rows": [["key-uuid", "AP+A"]]
  },
  "secrets": {
    "columns": ["id", "org_id", "namespace", "type", "value"],
    "rows": [["3", "1", "Prometheus", "datasource", "opaque-encrypted-value"]]
  }
}
```

上記は形式のみを示す簡略例です。実際には DB の全カラムを保存します。行内の値は `columns` の順序に対応し、非 NULL 値はすべて文字列、SQL NULL は JSON `null` として保存します。これにより 64 bit ID や空文字列と NULL の区別を保持します。真偽値は `"0"`／`"1"`、日時は RFC3339Nano です。`binary_columns` で指定したカラムは Base64、その他は従来通りの文字列です。移行先のカラム型に従って変換します。整数・真偽値・日時・UTF-8 テキスト・バイナリ以外の型は対象外です。

version 1 のファイルは鍵テーブルを含まず、不完全な暗号化情報の移行を避けるため受け付けません。version 2 で再エクスポートしてください。空のテーブルも `columns` と空の `rows` 配列を含めて保存します。

## 開発・検証

```sh
make lint         # gofmt チェックと go vet
make test         # 単体テストと race detector
make build
make integration  # 一時的な MySQL 8.4 / PostgreSQL 17 を Docker で起動して検証
make grafana-integration # Grafana 13.2.4 本体のスキーマ・暗号文で CLI を検証（Python 3 も必要）
```

`make integration` は専用の Compose プロジェクトを作成し、終了時にそのコンテナとボリュームを削除します。ローカルホストのランダムポートのみ公開します。MySQL → MySQL、PostgreSQL → PostgreSQL、両エンジン間の往復、ID 採番、NULL・Unicode・暗号文・バイナリ鍵・無効化済み鍵の保持、秘密情報の抽出範囲、追加テーブルの既存行拒否、全対象テーブルのロールバック、対象外テーブルの不変性を検証します。

`insert-only` でも全4経路を検証し、既存の値が異なる行の保持、未登録主キーのみの追加、再実行、DB 照合順序、別 ID の一意制約違反・参照不成立時のロールバック、シーケンスの巻き戻し防止を確認します。CLI 経由でも両 DB でモード指定と追加・スキップ件数を確認します。パッケージ間で使い捨て DB を共有するため、統合テストは `-p 1` で直列実行します。

`sync` でも両 DB 間の4経路とCLIを検証します。同名・別IDの再作成、更新・削除・再実行、対象外組織と用途の保護、共有鍵の非上書き、dry-run、制約違反時のロールバックを確認します。

既存の **空の使い捨てテスト DB** を使う場合は、`MYSQL_TEST_DSN` と `POSTGRES_TEST_DSN` を設定して以下を実行できます。テストは `org`・`data_source`・`data_keys`・`secrets`・`dashboard` を作成し、終了時に削除します。**本番 DB を指定しないでください。** テストは代表的なカラムのスキーマを使用し、Grafana 本体は起動しません。

```sh
go test -race -tags=integration -p 1 ./internal/cli ./internal/syncdb -count=1 -v
```

GitHub Actions でも lint・単体テスト・ビルド・実 DB テストを実行します。

`make grafana-integration` は専用の MySQL／PostgreSQL と Grafana 本体を起動し、本体のマイグレーション・API でスキーマと暗号文を生成します。CLI 経由で両 DB 間の4経路・3つのインポートモード、再実行、sync の dry-run、現行鍵と無効化済み鍵の保持・復号、13系で64 KiBを超える JSON の保持を検証します。復号は UID ベースのデータソースプロキシからテスト用 HTTP サーバーへ秘密ヘッダーが送信されることで確認します。ヘッダー受信用の小さな Go 製 HTTP サーバーを専用コンテナで起動し、Compose の内部ネットワークで接続します。受信サーバーのポートはホストへ公開しません。ローカルのテストサービスは `NO_PROXY` に指定します。外部 DB の接続文字列は受け付けず、終了時に専用コンテナ・ボリュームを削除します。

`GRAFANA_TEST_VERSION=12.2.0 make grafana-integration` のように本体の検証バージョンを変更できます（CLI のフラグではありません）。CI は12.2.0・13.0.11・13.2.4を対象にします。Grafana 13.0.0 は Git Sync のアップグレード障害で配布が取り下げられているため使用しません。Grafana 自身のアップグレードや対象外テーブルの移行は本ツールでは行いません。

追加の手動検証では Grafana 12.2.0 本体の SQL スキーマと共通の `secret_key` を使用し、MySQL → PostgreSQL → MySQL の移行後に、現行鍵と無効化済み鍵で暗号化されたデータソースの HTTP ヘッダーが復号・送信されることを API プロキシ経由で確認しました。KMS／Enterprise 暗号方式は未検証です。

`sync` も Grafana 12.2.0 本体で MySQL → PostgreSQL／PostgreSQL → MySQL の両方向を検証しました。同名データソースを別 ID で再作成し、新しい暗号鍵を追加した状態で、dry-run の不変性、実適用、再実行、移行鍵が無効のまま秘密 HTTP ヘッダーを復号・送信できることを確認しています。`insert-only` の検証は上記の代表的な SQL スキーマによる統合テストです。
