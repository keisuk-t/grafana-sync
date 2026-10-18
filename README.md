# grafana-sync

Grafana の組織・データソース・ダッシュボードを JSON にエクスポートし、別のデータベースへインポートする Go 製 CLI です。MySQL／PostgreSQL に対応します。対象は `--targets org,datasource,dashboard` の組み合わせで指定します。**既定値は `org,datasource`** で、ダッシュボードは明示的に選択した場合のみ処理します。ユーザー・チーム・アラート本体は移行しません。

| テーブル | 移行する行 |
| --- | --- |
| `org` | 全件 |
| `data_source` | 全件 |
| `data_keys` | 全件（無効化済みの鍵も含む） |
| `secrets` | `type = 'datasource'` かつ `org_id`・`namespace` が移行するデータソースの `org_id`・`name` に一致する行のみ |
| `dashboard` | `dashboard` 対象選択時のみ。通常のダッシュボードと `is_folder=1` のフォルダ行 |
| `folder` | `dashboard` 対象選択時のみ。ルート／ネストフォルダの階層情報 |
| `dashboard_tag`・`dashboard_version` | `dashboard` 対象選択時のみ。対象ダッシュボードのタグと保存履歴（孤立行は除外） |
| `library_element`・`library_element_connection` | `dashboard` 対象選択時のみ。ライブラリパネル本体（未使用のものも含む）とダッシュボードへの関連付け |
| 標準ロールのリソース権限 | `dashboard` 対象選択時のみ。Viewer／Editor のダッシュボード・フォルダ・ライブラリパネル固有権限を正規化して保存 |

`data_keys` は複数の用途で共有されるため、暗号文を解析せず、古い鍵も含めて保持します。`secrets` の別用途の行や孤立した行はエクスポートしません。

### 対象の選択

`org` は組織、`datasource` は `data_source`・`data_keys`・対象データソース用 `secrets`、`dashboard` は上表のダッシュボード関連データを一体で扱います。全7通りの非空の組み合わせに対応し、指定順序は問いません。未知名・重複・空要素はエラーです。公開名は `datasource` で、`data_source` は指定できません。

```sh
./bin/grafana-sync export -db mysql -file all.dump.json --targets org,datasource,dashboard
./bin/grafana-sync import -db postgres -file all.dump.json --targets dashboard --mode sync --dry-run
./bin/grafana-sync import -db postgres -file all.dump.json --targets dashboard --mode sync --allow-delete
```

import も同じ既定値なので、JSON に dashboard が含まれていても既定では処理しません。要求したグループが JSON にない場合はエラーです。未選択グループへの DML・採番変更は行いません。整合性確認のため、組織や参照するデータソース等を読み取る場合はあります。`org` を選ばない場合は、`scope.org_ids` の組織が移行先に存在する必要があります。

## セットアップ

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
- 移行先は Grafana のマイグレーションを実行済みで、選択対象テーブルのカラム名とバイナリカラムが移行元と一致する必要があります。DDL・スキーママイグレーションは実行しません。同一 Grafana バージョンのスキーマを推奨します。`data_keys.name` が文字列の主キー、`encrypted_data` が BLOB／BYTEA である Grafana の SQL スキーマを対象とします。関連テーブルがない旧スキーマや外部の secrets ストアは対象外です。
- **既定の `--mode require-empty` では、移行先の選択対象テーブルがすべて空でなければなりません。** 既定対象では `org`・`data_source`・`data_keys`・`secrets` の4テーブルです。`secrets` に別用途の行しかない場合も拒否します。Grafana の通常の初回起動では既定の `org` が作成されるため、そのままではこのモードで組織をインポートできません。既存 DB への非破壊的な追加には `insert-only`、削除を含む同期には `sync` を使用してください。
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
- 同じスナップショットを再実行しても、追加済みの主キーはスキップします。`inserted`／`skipped` は選択対象の行と標準権限を合計した件数です。追加のないテーブルの PostgreSQL シーケンスは変更せず、追加がある場合も既存の採番位置を巻き戻しません。
- 入力には参照整合性を満たす version 3 または version 2 のスナップショットを使用します。version 3 は org グループを省略して移行先の組織を参照できますが、選択したグループ内の親子関係は完結している必要があります。

**同じ ID が同じ論理リソースを指す、互換性のある DB 間で使用してください。** 名前・暗号鍵・秘密情報が異なる既存行も優先されるため、追加した設定が元と同じ動作になる保証はありません。特に同じ `data_keys.name` に異なる暗号鍵が保存されている場合、既存鍵を変更しないため新しい暗号文を復号できないことがあります。事前のバックアップ・書き込み停止と、移行後の動作確認が必要です。

### 選択対象の同期（sync）

```sh
./bin/grafana-sync import -db postgres -dsn-env TARGET_DSN -file grafana.dump.json --mode sync --dry-run
./bin/grafana-sync import -db postgres -dsn-env TARGET_DSN -file grafana.dump.json --mode sync --allow-delete
```

`sync` は明示的に `--dry-run` または `--allow-delete` が必要です。`--dry-run` は合計・テーブル別の追加／更新／削除／変更なし／スキップ件数を表示するだけで、DB の行・シーケンスを変更しません。適用時はロックを取得して差分を再計算します。論理キーは移行先の照合順序で事前検査しますが、予定件数や dry-run の成功は、DB 独自の制約・トリガーを含む将来の適用成功を保証しません。既定モードや `insert-only` にこれらのフラグは使用できません。

**削除範囲は version 3 の `scope.org_ids`（version 2 は `org.rows`）の組織に限定されます。** 通常のエクスポートは全組織を含むため、対象組織の選択済みリソース行を手動で省略すると、その行は削除対象になります。差分ファイルとしては扱いません。scope が空の場合は拒否します。選択したグループのテーブルが空なら、その scope 内のグループ全体を削除します。未選択グループは空データと見なさず保持します。

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

DB エラーに機密カラム値が含まれる場合があるため、生のドライバエラーメッセージは CLI に表示しません。ダッシュボードの依存・権限検査では、タイムアウト／キャンセル、MySQL エラー番号、PostgreSQL SQLSTATE を安全な診断情報として表示します。テーブル／行番号と DB 側のログを使って調査してください。

### 大量のダッシュボードとタイムアウト

`--timeout` の既定値は DB 処理全体で2分です。依存関係とリソース権限は件数を区切って一括照会し、1件ずつの照会による通信往復を減らしています。それでも DB の負荷・通信遅延・JSON の量によってタイムアウトする場合は、次のように延長できます。

```sh
./bin/grafana-sync export -db mysql -file all.dump.json --targets org,datasource,dashboard --timeout 10m
```

`cannot inspect dashboard dependencies in ...: query deadline exceeded` は検査自体のタイムアウトです。一方、sync 時の `dashboard has unsupported dependencies in dashboard_public; no changes applied` は、移行先の公開共有参照を壊す削除・ID／UID／組織等の変更を検出したことによる意図的な拒否です。こちらはタイムアウトを延長しても解消しません。

## ダッシュボードの対応範囲

**Grafana 12.2.0 の従来 SQL storage と、Grafana 12.4.3 の SQL スキーマを検証対象とします。Unified Storage・dual-write・移行中の状態には対応しません。** `kv_store` の保存方式フラグに加え、`resource` に Unified Storage のダッシュボード／フォルダが存在する場合もエラーにします。Grafana 12.4.3 の既定構成では API 経由の新規ダッシュボードが Unified Storage に保存される場合があるため、移行元・先とも従来 SQL storage のみで動作していることを管理者側でも確認してください。DB 外の設定は移行しません。

- ID／UID を保持します。フォルダ階層・タグ・全保存履歴を扱い、孤立参照・循環・タイトルや UID の不整合を拒否します。
- `require-empty` は選択した6テーブル（version 3 入力では4テーブル）が空であることを要求します。対象UIDの既存権限も競合として拒否します。`insert-only` は既存 dashboard ID を優先し、その配下のタグ・履歴・パネル関連付け・権限を追記しません。既存 library element ID の定義や権限も更新しませんが、新規ダッシュボードから既存パネルへの整合する関連付けは追加できます。
- `sync` は scope 内のフォルダ・ダッシュボード・タグ・履歴・ライブラリパネルと関連付け・標準権限を同期します。再作成、UID／名前の交換、フォルダ移動に対応し、対象外組織は保持します。
- ライブラリパネルは ID／UID と model JSON を保持します。通常のパネル（`kind=1`）とダッシュボードへの関連付け（`kind=1`）のみ対応し、未知の種類・欠落した本体／ダッシュボード・組織をまたぐ関連付け・不整合なフォルダ参照は拒否します。本体・履歴 JSON の `libraryPanel.uid` を解決し、現行本体の参照には対応する関連付けも要求します。履歴からのみ参照するパネルも本体が必要です。
- 対象外ダッシュボードの関連付けは削除しません。対象外／不明な接続先があるパネル本体の更新・削除は事前エラーです。共有パネルを更新すると、同期対象内の全利用ダッシュボードへ変更が反映されます。
- Viewer／Editor の `managed:builtins:*:permissions` に属するリソース権限を、組織・標準ロール名・action・scope で保存します。移行先の role ID は名前と `builtin_role` の結び付きを検証して解決し、必要な管理ロールがなければトランザクション内で作成します。permission／role の元DB固有IDはコピーしません。
- **公開共有設定は移行しませんが、共有設定のあるダッシュボード本体は export できます。** `dashboard_public` の公開URL・アクセストークン・有効／無効等は JSON に含めず、`dashboard.is_public` は出力内で `0` にします。移行元 DB の設定は変更しません。移行先に共有設定がなければ公開共有は作成されません。
- 移行先の既存の公開共有設定は削除・更新しません。同じ組織・UIDの本体を更新すると、移行先の既存共有URLにも更新内容が反映され得るため、新規共有を作らないことは移行先を非公開にする保証ではありません。sync で既存共有の参照を壊す削除・ID／UID／組織等の変更は事前エラーにします。
- **ユーザー／チームの個別権限・カスタムロール・個別ACL・provisioning は拒否します。Grafana が作成者に自動付与した権限も個別権限として停止します。** ライブラリパネル固有の `library.panels:uid:*` 権限にも同じ制限を適用し、フォルダ経由の継承権限は既存のフォルダ権限と一緒に扱います。標準ロールの承認だけで個別権限を捨てることはありません。これらを持つ通常のダッシュボードでも本ツールでは移行できない場合があります。実データの権限を削除して回避せず、必要な権限と別途の移行方法を検討してください。
- stars・playlist・annotations・alerts 等は移行しません。移行先で削除や ID／UID／組織変更がこれらの参照を壊す場合は事前エラーにします。
- dashboard本体・履歴JSONおよびライブラリパネル model の静的な datasource UID／名前参照は、移行後のデータソース状態に対して確認します。`datasource` を選ばない場合は既存 DB を参照します。動的変数・プラグイン固有の外部参照は完全検査できず警告します。移行後に Grafana で表示とクエリを確認してください。
- `created_by`／`updated_by`・保存履歴内の作成者IDは原値のままで、ユーザー自体は移行せず、IDも再マッピングしません。

## JSON 形式（version 4）

新しい export は選択対象だけを `tables` に保存します。例えば `--targets org` の簡略例は以下です。

```json
{
  "version": 4,
  "targets": ["org"],
  "scope": {"org_ids": ["1"]},
  "tables": {"org": {"columns": ["id", "name"], "rows": [["1", "Main Org."]]}}
}
```

dashboard を含む場合は `dashboard_permissions` 配列も必須です。権限がなくても空配列 `[]` を明示し、他グループではこのフィールドを含めません。保存例の要素は `{"org_id":"1","role":"Viewer","action":"folders:read","scope":"folders:uid:folder-uid"}` です。targets・tables・scope・権限の対応を検証し、未知フィールドや重複JSONキーも拒否します。

version 4 の `dashboard` には、従来の4テーブルに加え `library_element` と `library_element_connection` を必ず含めます。空の場合もカラムと空配列を明示し、「未対応」と「空なので削除」を区別します。

### version 3 の読み込み互換

version 3 は従来の対象範囲のまま読み込めます。ライブラリテーブルやパネル固有権限は更新・削除せず保持し、その参照を壊すダッシュボードの削除・ID／UID／組織変更やフォルダ削除は拒否します。パネル参照を含む本体・履歴を移行する場合は、version 4 で再エクスポートしてください。

### version 2 の読み込み互換

従来の以下の形式は `org,datasource` の入力として引き続き読み込めます。新規exportはversion 4です。

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

version 1 のファイルは鍵テーブルを含まず、不完全な暗号化情報の移行を避けるため受け付けません。version 4 で再エクスポートしてください。空の選択対象テーブルも `columns` と空の `rows` 配列を含めて保存します。

## 開発・検証

```sh
make lint         # gofmt チェックと go vet
make test         # 単体テストと race detector
make build
make integration  # 一時的な MySQL 8.4 / PostgreSQL 17 を Docker で起動して検証
```

`make integration` は専用の Compose プロジェクトを作成し、終了時にそのコンテナとボリュームを削除します。ローカルホストのランダムポートのみ公開します。MySQL → MySQL、PostgreSQL → PostgreSQL、両エンジン間の往復、ID 採番、NULL・Unicode・暗号文・バイナリ鍵・無効化済み鍵の保持、秘密情報の抽出範囲、追加テーブルの既存行拒否、全対象テーブルのロールバック、対象外テーブルの不変性を検証します。

`insert-only` でも全4経路を検証し、既存の値が異なる行の保持、未登録主キーのみの追加、再実行、DB 照合順序、別 ID の一意制約違反・参照不成立時のロールバック、シーケンスの巻き戻し防止を確認します。CLI 経由でも両 DB でモード指定と追加・スキップ件数を確認します。パッケージ間で使い捨て DB を共有するため、統合テストは `-p 1` で直列実行します。

`sync` でも両 DB 間の4経路とCLIを検証します。同名・別IDの再作成、更新・削除・再実行、対象外組織と用途の保護、共有鍵の非上書き、dry-run、制約違反時のロールバックを確認します。

ダッシュボードも全7通りの対象指定×3モード×DB間4経路を検証します。ネストフォルダ・タグ・複数履歴・静的参照・標準権限、UID交換・移動・再作成・削除、既存親の子データ保持、他グループ／組織の保護、Unified Storage・個別権限・対象外依存の拒否、dry-run の行・PostgreSQLシーケンス不変性、失敗時ロールバックを含みます。

ライブラリパネルも、1パネルを14ダッシュボードで共有する fixture で3モード×DB間4経路を検証します。本体／関連付け／標準権限の往復、既存IDの非更新、対象外接続の保護、不明な種類や未解決参照の拒否、旧version 3入力での保護、dry-run・制約違反時のロールバック・再実行・削除順序を含みます。

既存の **空の使い捨てテスト DB** を使う場合は、`MYSQL_TEST_DSN` と `POSTGRES_TEST_DSN` を設定して以下を実行できます。テストは対象テーブル・権限テーブル・保存方式／依存検査用テーブルを作成し、終了時に削除します。**本番 DB を指定しないでください。** テストは代表的なカラムのスキーマを使用し、Grafana 本体は起動しません。

```sh
go test -race -tags=integration -p 1 ./internal/cli ./internal/syncdb -count=1 -v
```

GitHub Actions でも lint・単体テスト・ビルド・実 DB テストを実行します。

追加の手動検証では Grafana 12.2.0 本体の SQL スキーマと共通の `secret_key` を使用し、MySQL → PostgreSQL → MySQL の移行後に、現行鍵と無効化済み鍵で暗号化されたデータソースの HTTP ヘッダーが復号・送信されることを API プロキシ経由で確認しました。KMS／Enterprise 暗号方式は未検証です。

`sync` も Grafana 12.2.0 本体で MySQL → PostgreSQL／PostgreSQL → MySQL の両方向を検証しました。同名データソースを別 ID で再作成し、新しい暗号鍵を追加した状態で、dry-run の不変性、実適用、再実行、移行鍵が無効のまま秘密 HTTP ヘッダーを復号・送信できることを確認しています。`insert-only` の検証は上記の代表的な SQL スキーマによる統合テストです。

ダッシュボードも Grafana 12.2.0 の実スキーマで MySQL↔PostgreSQL を検証し、API経由で移行後のネストフォルダ・タグ・履歴取得、Viewerの参照と書き込み拒否、Editorの保存、暗号化データソースの利用を確認しました。自動生成される作成者個別権限は最初にexport拒否を確認し、標準ロールのみの使い捨てfixtureで移行を検証しています。個別権限を移行できることを意味しません。
