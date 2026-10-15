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

Go 1.26 以上が必要です。実 DB テストには Docker Engine と Docker Compose v2 も必要です。

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
- **既定の `--mode require-empty` では、移行先の `org`・`data_source`・`data_keys`・`secrets` がすべて空でなければなりません。** `secrets` に別用途の行しかない場合も拒否します。Grafana の通常の初回起動では既定の `org` が作成されるため、そのままではこのモードでインポートできません。既存データの削除・置換・更新はこのツールでは行いません。既存 DB への追加は後述の `insert-only` を使用してください。
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

### 機密情報と暗号化

JSON には `password`、`basic_auth_password`、`secure_json_data` などを含む全カラムが入ります。**出力ファイル自体は暗号化されません。** 安全に保管・転送し、リポジトリへコミットしないでください。`.gitignore` はルートの `*.dump.json` を除外しますが、任意のパスのファイルは除外しないため注意してください。

暗号文と暗号化されたデータ鍵はそのままコピーし、復号／再暗号化はしません。`data_keys.encrypted_data` のバイナリは Base64 で保存し、インポート時に元のバイト列へ戻します。鍵の `name`・`label`・`active`・`scope`・`provider`・日時も保持します。無効化された鍵でも既存の暗号文の復号に必要な場合があるため、削除・再生成しません。

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
```

`make integration` は専用の Compose プロジェクトを作成し、終了時にそのコンテナとボリュームを削除します。ローカルホストのランダムポートのみ公開します。MySQL → MySQL、PostgreSQL → PostgreSQL、両エンジン間の往復、ID 採番、NULL・Unicode・暗号文・バイナリ鍵・無効化済み鍵の保持、秘密情報の抽出範囲、追加テーブルの既存行拒否、全対象テーブルのロールバック、対象外テーブルの不変性を検証します。

`insert-only` でも全4経路を検証し、既存の値が異なる行の保持、未登録主キーのみの追加、再実行、DB 照合順序、別 ID の一意制約違反・参照不成立時のロールバック、シーケンスの巻き戻し防止を確認します。CLI 経由でも両 DB でモード指定と追加・スキップ件数を確認します。パッケージ間で使い捨て DB を共有するため、統合テストは `-p 1` で直列実行します。

既存の **空の使い捨てテスト DB** を使う場合は、`MYSQL_TEST_DSN` と `POSTGRES_TEST_DSN` を設定して以下を実行できます。テストは `org`・`data_source`・`data_keys`・`secrets`・`dashboard` を作成し、終了時に削除します。**本番 DB を指定しないでください。** テストは代表的なカラムのスキーマを使用し、Grafana 本体は起動しません。

```sh
go test -race -tags=integration -p 1 ./internal/cli ./internal/syncdb -count=1 -v
```

GitHub Actions でも lint・単体テスト・ビルド・実 DB テストを実行します。

追加の手動検証では Grafana 12.2.0 本体の SQL スキーマと共通の `secret_key` を使用し、MySQL → PostgreSQL → MySQL の移行後に、現行鍵と無効化済み鍵で暗号化されたデータソースの HTTP ヘッダーが復号・送信されることを API プロキシ経由で確認しました。KMS／Enterprise 暗号方式は未検証です。

この Grafana 本体の確認は空 DB への通常インポートで実施しました。`insert-only` の検証は上記の代表的な SQL スキーマによる統合テストです。
