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
| `dashboard_tag`・`dashboard_version` | `dashboard` 対象選択時のみ。対象ダッシュボードのタグと保存履歴（孤立行は除外。タグは下記のUID検査も実施） |
| `library_element`・`library_element_connection` | `dashboard` 対象選択時のみ。ライブラリパネル本体（未使用のものも含む）とダッシュボードへの関連付け |
| リソース権限・ロール依存 | `dashboard` 選択時のみ。Viewer／Editor／Admin、個別ユーザー・チーム、組織専用ロールと対応する旧ACL。共有・グローバルロールは既存定義との一致を要求 |

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

### ライブラリパネルを使うダッシュボードを除外する

ライブラリパネル機能を移行しない場合は、export 専用の明示フラグを指定できます。

```sh
./bin/grafana-sync export -db mysql -file no-library.dump.json \
  --targets org,datasource,dashboard \
  --exclude-library-panel-dashboards --timeout 10m
```

- 本体または履歴の JSON に null 以外の `libraryPanel` がある、または `kind=1` のライブラリ接続先になっているダッシュボードを丸ごと除外します。参照先が欠落・別組織にある場合や UID が不正な場合も対象です。**現在は使っていなくても、過去の履歴に参照があれば除外します。**
- 除外したダッシュボードのタグ・全履歴・個別の RBAC grant・旧 ACL と、全ライブラリパネル本体・全接続・それら専用のロールや principal を export 対象から外します。フォルダ、残すダッシュボードとその JSON、組織・データソースは保持します。
- 残すリソースでも利用する共有ロールは定義を保持し、権限の切り詰めや組織境界を越える付け替えは行いません。共有ロールに含まれる対象外スコープの権限は定義に残る場合があります。既存の import 側の共有ロール差分検査も維持します。
- 元 DB は変更しません。除外した本体・履歴・タグ・パネル・接続の件数を警告し、UID・名前・JSON 本文は表示しません。壊れた JSON 自体は黙って除外せず停止し、残したデータの検証も緩めません。
- `--targets` に `dashboard` が必要です。ライブラリ本体を移行しないため、`--rehome-orphan-library-panels`・`--rename-colliding-library-panels` とは併用できません。

出力は version 5 のまま `library_panels_excluded: true` を記録します。この JSON の dashboard 移行には **`require-empty` または `insert-only` だけ**を使用してください。除外したデータが削除対象になるのを防ぐため、dashboard を選択した `sync` は `--dry-run`・`--allow-delete` のどちらでも拒否します。dashboard を選択せず組織・データソースだけ同期する場合には、この制限は適用しません。旧版 CLI は未知のマーカーを拒否するため、export/import とも対応版を使用してください。マーカーを削除・変更して安全策を回避しないでください。

このフラグは完全移行ではなく、意図的に範囲を縮めるものです。残すダッシュボードのリンクや動的・プラグイン固有の参照が、除外したダッシュボードを指す可能性は残るため、移行後に確認してください。

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
- 入力には参照整合性を満たすversion 2〜5のスナップショットを使用します。version 3以降はorgグループを省略して移行先の組織を参照できますが、選択したグループ内の親子関係は完結している必要があります。

**同じ ID が同じ論理リソースを指す、互換性のある DB 間で使用してください。** 名前・暗号鍵・秘密情報が異なる既存行も優先されるため、追加した設定が元と同じ動作になる保証はありません。特に同じ `data_keys.name` に異なる暗号鍵が保存されている場合、既存鍵を変更しないため新しい暗号文を復号できないことがあります。事前のバックアップ・書き込み停止と、移行後の動作確認が必要です。

### 選択対象の同期（sync）

```sh
./bin/grafana-sync import -db postgres -dsn-env TARGET_DSN -file grafana.dump.json --mode sync --dry-run
./bin/grafana-sync import -db postgres -dsn-env TARGET_DSN -file grafana.dump.json --mode sync --allow-delete
```

`sync` は明示的に `--dry-run` または `--allow-delete` が必要です。`--dry-run` は合計・テーブル別の追加／更新／削除／変更なし／スキップ件数を表示するだけで、DB の行・シーケンスを変更しません。適用時はロックを取得して差分を再計算します。論理キーは移行先の照合順序で事前検査しますが、予定件数や dry-run の成功は、DB 独自の制約・トリガーを含む将来の適用成功を保証しません。既定モードや `insert-only` にこれらのフラグは使用できません。

**削除範囲はversion 3以降の `scope.org_ids`（version 2は `org.rows`）の組織に限定されます。** 通常のエクスポートは全組織を含むため、対象組織の選択済みリソース行を手動で省略すると、その行は削除対象になります。差分ファイルとしては扱いません。scope が空の場合は拒否します。選択したグループのテーブルが空なら、その scope 内のグループ全体を削除します。未選択グループは空データと見なさず保持します。

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
- export では、親の `dashboard_id` が存在しないタグ・履歴を除外します。また、タグの組織 ID が ID 参照先と一致し、空でない `dashboard_uid` の参照先が同じ組織に存在しない場合、そのタグ行も孤立行として除外します（Grafana 12.4.3 のタグ参照は組織 ID＋UID）。元 DB とダッシュボード本体の JSON 内のタグは変更しません。組織 ID の不一致、UID が別の既存ダッシュボードを指す場合、NULL／空 UID は引き続き拒否し、推測で修復しません。import／sync の JSON 検証も緩和しません。
- export では、`kind=1` のライブラリパネル関連付けのうち、関連付け行／パネル／ダッシュボードの ID が正の有効な値で、パネルが存在し、ダッシュボードの参照先が全組織で存在しない行だけを孤立行として JSON から除外し、除外件数を警告します。追加オプションは不要です。元 DB、パネル本体・UID・model・権限、実在するダッシュボードへの関連付けは変更しません。別組織やフォルダへの参照、不正 ID、未対応 kind、存在しないパネルへの参照は引き続き拒否します。import／sync も孤立した関連付けを含む入力を拒否し、移行先の孤立行を勝手に削除しません。
- `require-empty` は選択した6テーブル（version 3 入力では4テーブル）が空であることを要求します。対象UIDの既存権限も競合として拒否します。`insert-only` は既存 dashboard ID を優先し、その配下のタグ・履歴・パネル関連付け・権限を追記しません。既存 library element ID の定義や権限も更新しませんが、新規ダッシュボードから既存パネルへの整合する関連付けは追加できます。
- `sync` は scope 内のフォルダ・ダッシュボード・タグ・履歴・ライブラリパネルと関連付け・標準権限を同期します。再作成、UID／名前の交換、フォルダ移動に対応し、対象外組織は保持します。
- ライブラリパネルは ID／UID と model JSON を保持します。通常のパネル（`kind=1`）とダッシュボードへの関連付け（`kind=1`）のみ対応し、未知の種類・欠落した本体／ダッシュボード・組織をまたぐ関連付け・不整合なフォルダ参照は拒否します。本体・履歴 JSON の `libraryPanel.uid` を解決し、現行本体の参照には対応する関連付けも要求します。履歴からのみ参照するパネルも本体が必要です。
- `library_element.model` **直下**の `libraryPanel` オブジェクトは、`uid` 項目自体がない場合（`{}` や名前のみのメタデータ）、UID が空文字の場合、そのパネル自身の UID と一致する場合に許可し、model 全体をそのまま保持します。Grafana 12.4.3 は UID 項目のないオブジェクトも保存・取得でき、更新 API では空文字 UID も保存されます。[取得処理](https://github.com/grafana/grafana/blob/v12.4.3/pkg/services/libraryelements/database.go)は空文字 UID を応答内だけで自身の UID に補完し、[フロントエンド](https://github.com/grafana/grafana/blob/v12.4.3/public/app/features/library-panels/state/api.ts)は直下の `libraryPanel` を利用用モデルから除去します。この CLI は項目の除去・UID の補完・名前による参照解決を行いません。UID 項目が NULL／文字列以外の場合、空でない別 UID を参照する場合、NULL 以外の非オブジェクトの場合、さらに深い位置に `libraryPanel` がある場合は引き続き拒否します。直下の `libraryPanel` 自体が存在しない場合や NULL の場合は従来どおり許可します。エラーには分類と数値のパネル ID のみを表示し、UID・名前・model 内容は表示しません。`library_element.uid` 自体とダッシュボード本体／履歴の参照は引き続き空でない有効な UID を必須とし、参照先・組織・関連付けの検証も変更しません。
- 保存先フォルダを失ったライブラリパネルも既定では拒否します。元 DB を変更せず General へ移す明示オプションと権限上の注意は、下記を参照してください。
- 対象外ダッシュボードの関連付けは削除しません。対象外／不明な接続先があるパネル本体の更新・削除は事前エラーです。共有パネルを更新すると、同期対象内の全利用ダッシュボードへ変更が反映されます。
- version 5はViewer／Editor／Admin等の標準ロールと個別ユーザー・チームのリソース権限を、組織・論理ロール・action・scopeで保存します。移行先のroleは名前と割当を検証して解決し、不足するmanaged roleはトランザクション内で作成します。元DB固有のrole／permission／assignment／ACL IDはコピーしません。
- **公開共有設定は移行しませんが、共有設定のあるダッシュボード本体は export できます。** `dashboard_public` の公開URL・アクセストークン・有効／無効等は JSON に含めず、`dashboard.is_public` は出力内で `0` にします。移行元 DB の設定は変更しません。移行先に共有設定がなければ公開共有は作成されません。
- 移行先の既存の公開共有設定は削除・更新しません。同じ組織・UIDの本体を更新すると、移行先の既存共有URLにも更新内容が反映され得るため、新規共有を作らないことは移行先を非公開にする保証ではありません。sync で既存共有の参照を壊す削除・ID／UID／組織等の変更は事前エラーにします。
- version 5 は作成者の自動付与を含む個別ユーザー・チーム権限にも対応します。ユーザー・チーム・所属は事前作成し、下記の `--principal-map` で明示的に対応付けます。provisioning・未解決参照・不整合な旧ACLは拒否します。公開共有と Unified Storage は引き続き移行しません。
- stars・playlist・annotations・alerts 等は移行しません。移行先で削除や ID／UID／組織変更がこれらの参照を壊す場合は事前エラーにします。
- dashboard本体・履歴JSONおよびライブラリパネル model の静的な datasource UID／名前参照は、移行後のデータソース状態に対して確認します。`datasource` を選ばない場合は既存 DB を参照します。動的変数・プラグイン固有の外部参照は完全検査できず警告します。移行後に Grafana で表示とクエリを確認してください。
- `created_by`／`updated_by`・保存履歴内の作成者IDは原値のままで、ユーザー自体は移行せず、IDも再マッピングしません。

### フォルダ参照が欠落したライブラリパネルの明示的な救済

`library panel references a missing folder`（旧版では `root library panel has a folder_id`）で停止し、保存先が欠落していることを管理者が確認した場合に限り、export 専用の `--rehome-orphan-library-panels` を指定できます。`--targets` に `dashboard` を含める必要があります。既定値は無効で、import には指定できません。

```sh
./bin/grafana-sync export -db mysql -file rescued.dump.json \
  --targets org,datasource,dashboard --rehome-orphan-library-panels
```

救済するのは、`library_element.folder_uid` が NULL または空文字で、正の `folder_id` に対応する `dashboard` 行が全組織を通じて存在しないパネルだけです。出力 JSON 上でその2列のみを `folder_id=0`、`folder_uid=general` へ変更し、移動件数と警告を標準エラーへ表示します。元 DB は読み取り専用トランザクションで扱い、パネル本体・ID／UID・model・ダッシュボード本体／履歴・関連付け・明示的な RBAC 権限は変更しません。JSON は引き続き version 5 で、通常の import／sync で読み込めます。

**General への配置変更は、フォルダから継承する閲覧・編集権限やライブラリパネルの一覧表示を変える可能性があります。明示的な RBAC を保持しても、移行前と同じ実効アクセス権を保証しません。** 適用前に dry-run、DB バックアップ、General のアクセス設定を確認し、適用後に Grafana で表示・操作・アクセス権を確認してください。

救済対象外の不整合（実在する ID を指すのに UID が欠落、ID／UID の食い違い、空でない未解決 UID、`general` と正の ID の併存、不正な ID）は推測で修復せず拒否します。移動後の General 内の名前衝突も、既定では拒否します。正常なパネルや既存の General 配下のパネルは変更しません。import／sync の入力検証・対象外の共有参照保護も緩和しません。

#### General 内の名前衝突も明示的に救済する場合

パネル名の変更を許可する場合に限り、export 専用の `--rename-colliding-library-panels` を追加できます。単独では使用できず、`--rehome-orphan-library-panels` と `dashboard` 対象が必須です。

```sh
./bin/grafana-sync export -db mysql -file rescued.dump.json \
  --targets org,datasource,dashboard \
  --rehome-orphan-library-panels --rename-colliding-library-panels
```

- 改名できるのは、この export で General へ移した孤立パネルだけです。既存の General 内のパネルや正常なフォルダ内のパネルは改名しません。対象外の名前衝突は引き続きエラーです。
- 同じ組織の既存 General 内の名前を優先します。孤立パネル同士だけの衝突なら、移行元の数値 ID が最小のパネルは元の名前を保持します。入力行の並び順には依存しません。
- それ以外の衝突パネルには `元の名前 [migrated-移行元ID]` を使います。その候補も使用済みなら `元の名前 [migrated-移行元ID-2]` のように連番を加えます。他の移動対象の元の名前も予約して、無関係なパネルの改名を避けます。
- Grafana の上限150文字に収まるよう、必要な場合だけ元の名前の末尾を Unicode 文字単位で短縮して接尾辞を付けます。移行先 DB の照合順序による追加の衝突は import／sync の検査・DB制約に従い、強行しません。
- JSON 上の `library_element.name` と再配置の2列だけを変更し、改名件数を警告します。名前・UID・model 内容はログに出しません。元 DB、ID／UID、kind／type、model のバイト列、version、ダッシュボード本体／履歴、関連付け、明示的な RBAC は保持します。model 内の `libraryPanel.name` 等には元の名前が残る場合があります。Grafana が取得時に返す表示名は `library_element.name` に従います。
- 出力は通常の version 5 JSON で、import にこのオプションは不要です。`sync` は改名も反映しますが、`insert-only` は既存行の名前を変更しません。バックアップと dry-run、移行後の表示・参照・実効アクセス権の確認が必要です。

## 権限の対応付け（version 5）

ユーザー／サービスアカウント・チームを移行先に事前作成し、組織所属・標準ロール・管理者／無効化状態・サービスアカウント種別・チームメンバーとその権限を合わせてください。主体自体の作成・更新・所属変更は行いません。login／email／数値ID／UIDの一致だけで同一主体と推測しません。組織IDは本体と同じIDを使用します。

移行元／先のGrafanaバージョンと権限関連のfeature toggle（`onlyStoreActionSets`／`annotationPermissionUpdate`等）は合わせてください。`dashboards:view/edit/admin`・`folders:view/edit/admin` のaction setも元のまま保存しますが、その展開結果はGrafanaの実行時設定に依存し、DBだけでは検証できません。未知のactionや不整合なscopeは事前エラーにします。

`--principal-map` は別ファイルとして、すべての参照主体（チームの全メンバーを含む）を一対一で指定します。IDはJSON文字列です。`target_uid` は移行先の実際のユーザー／チームUIDを確認して記入します。パスワード・トークン等は記入しないでください。

```json
{
  "version": 1,
  "mappings": [
    {"org_id":"1","kind":"user","source_id":"23","target_id":"108","target_uid":"target-user-uid"},
    {"org_id":"1","kind":"team","source_id":"7","target_id":"45","target_uid":"target-team-uid"}
  ]
}
```

```sh
grafana-sync import -db postgres -file dashboards.dump.json --targets dashboard \
  --principal-map principals.json --mode sync --dry-run
grafana-sync import -db postgres -file dashboards.dump.json --targets dashboard \
  --principal-map principals.json --mode sync --allow-delete
```

- 不足・重複・異なるUID／主体種別・組織標準ロールやチーム所属の差異は変更前に停止します。対応付けファイルも適切なアクセス権で保管してください。
- managed user／team のロール名は対応後IDで作り直し、role／permission／assignment の数値IDはコピーしません。managed role のスナップショットUIDは照合用の論理値で、実DBのUIDではありません。
- 全権限が選択済みリソースに閉じた `custom:*` 組織専用ロールは不足時のみ作成します。既存の同名／同UIDロールの定義・割当が異なる場合は上書きせず停止します。
- グローバル（org_id=0）・固定・他リソースと共有するロールは作成・更新・削除しません。全定義・全割当が移行先に一致する場合だけ受け入れます。その依存比較に必要な主体も明示対応が必要です。対象外の権限は新規に付与しません。
- `sync` は対象組織の選択リソースにある managed 権限を JSON に合わせます。対象外のリソース権限・ロールの割当は保持し、空になったロールも残します。既存カスタム／共有ロールの変更が必要なら事前エラーです。`insert-only` は既存親の権限・ACLを変更せず、スキップされた親だけの主体には対応付けを要求しません。
- 旧 `dashboard_acl` は現在の managed RBAC と一意に対応し、read/edit/admin の定義が一致する行のみ移行します。欠落・競合・孤立・未知形式は拒否し、どちらかを推測で優先しません。
- dry-run は `rbac_roles`、`dashboard_permissions`、ユーザー／チーム／標準ロールの割当、`dashboard_acl` の件数を表示し、採番を消費しません。適用時のエラーは全選択データ・権限・割当をロールバックします（DBの自動採番には欠番が生じ得ます）。Grafanaを停止してバックアップ後に実行し、再起動後に権限と画面表示を確認してください。
- RBAC用メタデータにユーザーの password hash／salt／rands、認証トークン・API key・session は含めません。データソースの暗号化情報は従来の別グループです。

## JSON 形式（version 5）

新しい export は選択対象だけを `tables` に保存します。例えば `--targets org` の簡略例は以下です。

```json
{
  "version": 5,
  "targets": ["org"],
  "scope": {"org_ids": ["1"]},
  "tables": {"org": {"columns": ["id", "name"], "rows": [["1", "Main Org."]]}}
}
```

dashboard を含む場合は `rbac` オブジェクトが必須で、`roles`／`principals`／`grants`／`legacy_acl` 配列を持ちます。権限がなくても各配列を `[]` と明示します。ほかのグループには `rbac` を含めません。`grants[].role`／`legacy_acl[].role` は `roles[].ref` を参照し、ロールは全定義・割当の依存情報を保持します。未知フィールド・重複JSONキー・ロール／権限／ACLの不整合や欠落を拒否します。v5で旧 `dashboard_permissions` を併記できません。

version 4／5 の `dashboard` には、従来の4テーブルに加え `library_element` と `library_element_connection` を必ず含めます。空の場合もカラムと空配列を明示し、「未対応」と「空なので削除」を区別します。

### version 4 の読み込み互換

version 4の `dashboard_permissions`（Viewer／Editor）は従来どおり読み込みます。旧JSONで個別主体権限を空と解釈して削除せず、解釈できない移行先の権限があれば変更前に停止します。

### version 3 の読み込み互換

version 3 は従来の対象範囲のまま読み込めます。ライブラリテーブルやパネル固有権限は更新・削除せず保持し、その参照を壊すダッシュボードの削除・ID／UID／組織変更やフォルダ削除は拒否します。パネル参照を含む本体・履歴を移行する場合は、最新のversion 5で再エクスポートしてください。

### version 2 の読み込み互換

従来の以下の形式は `org,datasource` の入力として引き続き読み込めます。新規exportはversion 5です。

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

version 1のファイルは鍵テーブルを含まず、不完全な暗号化情報の移行を避けるため受け付けません。最新のversion 5で再エクスポートしてください。空の選択対象テーブルも `columns` と空の `rows` 配列を含めて保存します。

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

ダッシュボードも全7通りの対象指定×3モード×DB間4経路を検証します。ネストフォルダ・タグ・複数履歴・静的参照・標準権限、UID交換・移動・再作成・削除、既存親の子データ保持、他グループ／組織の保護、Unified Storage・不整合な権限・対象外依存の拒否、dry-runの行・PostgreSQLシーケンス不変性、失敗時ロールバックを含みます。

version 5 RBACも全7対象×3モード×DB間4経路を検証します。異なるユーザー／チーム／role／permission／assignment／ACL ID、明示対応の欠落・不一致、所属・状態の差異、組織専用ロール、global／fixed／shared roleの不足・差異、UID／名前／照合順序の衝突、旧ACLの一致、権限削除の範囲、ソース不変性と資格情報の非出力、採番・dry-run・再実行・途中失敗時のロールバックを含みます。

ライブラリパネルも、1パネルを14ダッシュボードで共有する fixture で3モード×DB間4経路を検証します。本体／関連付け／標準権限の往復、既存IDの非更新、対象外接続の保護、不明な種類や未解決参照の拒否、旧version 3入力での保護、dry-run・制約違反時のロールバック・再実行・削除順序を含みます。

既存の **空の使い捨てテスト DB** を使う場合は、`MYSQL_TEST_DSN` と `POSTGRES_TEST_DSN` を設定して以下を実行できます。テストは対象テーブル・権限テーブル・保存方式／依存検査用テーブルを作成し、終了時に削除します。**本番 DB を指定しないでください。** テストは代表的なカラムのスキーマを使用し、Grafana 本体は起動しません。

```sh
go test -race -tags=integration -p 1 ./internal/cli ./internal/syncdb -count=1 -v
```

GitHub Actions でも lint・単体テスト・ビルド・実 DB テストを実行します。

追加の手動検証では Grafana 12.2.0 本体の SQL スキーマと共通の `secret_key` を使用し、MySQL → PostgreSQL → MySQL の移行後に、現行鍵と無効化済み鍵で暗号化されたデータソースの HTTP ヘッダーが復号・送信されることを API プロキシ経由で確認しました。KMS／Enterprise 暗号方式は未検証です。

`sync` も Grafana 12.2.0 本体で MySQL → PostgreSQL／PostgreSQL → MySQL の両方向を検証しました。同名データソースを別 ID で再作成し、新しい暗号鍵を追加した状態で、dry-run の不変性、実適用、再実行、移行鍵が無効のまま秘密 HTTP ヘッダーを復号・送信できることを確認しています。`insert-only` の検証は上記の代表的な SQL スキーマによる統合テストです。

ダッシュボードもGrafana 12.2.0の実スキーマでMySQL↔PostgreSQLを検証し、API経由で移行後のネストフォルダ・タグ・履歴取得、Viewerの参照と書き込み拒否、Editorの保存、暗号化データソースの利用を確認しました。version 5では異なる主体IDを明示対応して双方向移行し、作成者の自動Admin権限、個別Edit、チーム経由のView、チームメンバーの保存拒否と無関係なユーザーの閲覧拒否も確認しました。Grafana 12.4.3本体のSQL移行後動作、Enterpriseでのカスタム／固定ロールの実動作は未検証です。
