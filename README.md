PachiCounter
============

パチンコ台の外部情報出力端子から出る信号を USB-HID の GPIO デバイス経由で読み取り、
現在の回転数や初当たり確率を配信する、ホール設置のデータカウンター相当のソフトウェアです。

[yukkeorg/PachiCounter](https://github.com/yukkeorg/PachiCounter)（Python 版）を Go で
書き直したものです。以降の開発はこちらで行います。Python 版はそのまま残してあります。

**コア（常駐して集計と配信をする部分）**、**機種プラグイン（機種の仕様）**、
**フロント（見た目）** を分けた構成にしました。


構成
----

```
台 ──> USB-HID GPIO デバイス ──> コア ──(SSE)──> フロント（ブラウザ / OBS / 自作）
                                  │
                                  └──> 生信号ログ ＋ スナップショット
```

コアは見た目を持ちません。HTTP で待ち受けて集計結果を SSE で流すので、フロントは
いくつでも好きな技術で繋げます。既定のフロントはバイナリに埋め込んであるので、
別途ビルドや配布は要りません。

- **コア** — 信号を集計し、HTTP で配信する常駐部分
- **機種プラグイン** — 信号の意味、カウンタの増減規則、状態の遷移、機種固有の呼び名
- **フロント** — 色・書体・配置だけを受け持つ。機種の仕様を知らない

設計上の判断は [docs/adr/](docs/adr/) に、用語は [CONTEXT.md](CONTEXT.md) にまとめてあります。


必要なもの
----------

- Go 1.24 以降（ビルド時のみ。cgo は使いません）
- USB-HID で GPIO を出せるデバイス
  - [km2net USB-IO 2.0](http://km2net.com/usb-io2.0/index.shtml)（[秋月電子の互換品](http://akizukidenshi.com/catalog/g/gM-05131/)も可）
- Linux または Windows
  - Linux は `/dev/hidraw` を直接読みます（udev ルールが必要）
  - Windows 向けのハードウェア層は未実装です。ダミーと記録再生は動きます


配線
----

| 役割 | 内容 | 既定のビット |
|---|---|---|
| `start` | スタート信号（図柄確定ごとに 1 パルス＝1 回転） | 0 |
| `bonus` | 大当り信号 | 1 |
| `densapo` | 電サポ信号（**大当り中も出ます**） | 2 |
| `payout_bonus` | 出玉あり大当り信号（任意） | 3 |
| `payout` | 賞球信号（任意。繋ぐと獲得玉数が実測になります） | — |

外部出力端子はオープンコレクタなので、プルアップ抵抗と組み合わせると信号が出ている間
LOW になります。既定でアクティブローとして扱います（`-active-low=false` で切れます）。

電サポ信号からは「玉が減らない状態にいる」ことしか分かりません。その区間が高確率
（確変・ST）なのか通常確率（時短）なのかは機種の仕様知識であり、機種プラグインが判断します。
この点が初当たり確率の分母の決まり方に直結します（[ADR-0004](docs/adr/0004-per-machine-denominator-rule.md)）。


導入
----

```
$ git clone https://github.com/yukkeorg/pachicounter2.git
$ cd pachicounter2
$ go build -o pachicounter ./cmd/pachicounter
```

Linux ではデバイスを読む権限が必要です。

```
$ sudo cp contrib/udev/60-pachicounter.rules /etc/udev/rules.d/
$ sudo udevadm control --reload-rules && sudo udevadm trigger
```

Raspberry Pi などへ持っていく場合は、そのままクロスコンパイルできます。

```
$ CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o pachicounter-arm64 ./cmd/pachicounter
```


使い方
------

```
$ ./pachicounter -list-machines          # 対応している機種を見る
$ ./pachicounter -list-devices           # デバイスの検出結果を見る
$ ./pachicounter -machine stealth        # 集計を始める
```

起動したら http://127.0.0.1:18888 を開きます。OBS で使う場合は、このアドレスを
ブラウザソースに指定してください。

台の調整値は台ごと・日ごとに変わるので、フラグで渡します。

```
$ ./pachicounter -machine vb -rotation-rate 18 -densapo-base 0.85
```

| フラグ | 意味 |
|---|---|
| `-machine` | 集計する機種の ID |
| `-source` | 信号源: `usbhid`（既定）/ `dummy` / `file:<ログ>` / `loop:<ログ>` |
| `-wire` | 配線。`start=0,bonus=1,densapo=2` の形 |
| `-rotation-rate` | 回転率（貸玉 250 個あたりの回転数）。台の調整値 |
| `-densapo-base` | 電サポ中の玉持ち率。台の調整値 |
| `-poll-interval` | ポーリング間隔（既定 5ms） |
| `-debounce` | 接点バウンスを無視する時間（既定 8ms） |
| `-listen` | 待ち受けアドレス（既定 `127.0.0.1:18888`） |
| `-front-dir` | 外部のフロントを使う（埋め込みより優先） |
| `-new-session` | 続けられるセッションがあっても新しく始める |

デバイスが繋がっていなくても起動し、接続を待ち続けます。途中で抜けても止まりません。


記録と再生
----------

集計したセッションは生信号ログとして残ります（既定 `~/.local/state/pachicounter/sessions/`）。
このログは信号源の 1 つとして扱えるので、実機なしでコアを丸ごと動かせます。

```
$ ./pachicounter -source file:~/.local/state/pachicounter/sessions/20260913-120000-stealth.jsonl
```

機種と配線はログの先頭に記録されているので、`-machine` や `-wire` の指定は要りません。
集計規則を直したあとに過去のセッションを流し直して確認する、という使い方ができます。
`loop:` にすると繰り返し再生するので、フロントの見た目を調整するときに便利です。

再生中は保存先に何も書きません。実機で集計していたセッションの続きになることも無いので、
配信用の数字に影響しません。そのぶん、補正と新しいセッションの操作は受け付けません。


HTTP API
--------

| メソッド | パス | 内容 |
|---|---|---|
| GET | `/events` | スナップショットを SSE で流し続ける |
| GET | `/api/snapshot` | 現在のスナップショットを 1 回返す |
| GET | `/api/machines` | 対応している機種の一覧 |
| POST | `/api/session/new` | 新しいセッションを始める |
| POST | `/api/correct` | カウンタを手動補正する |

補正は集計結果を直接書き換えるのではなく、生信号ログ上のイベントとして記録されるので、
再集計しても補正が残ります。

```
$ curl -X POST http://127.0.0.1:18888/api/correct \
    -H 'Content-Type: application/json' \
    -d '{"counter":"normal_rotations","delta":1,"note":"取りこぼし"}'
```

React などでフロントを作る場合は、ビルド成果物を `-front-dir` で指定するか、別ポートの
開発サーバから繋ぎます。後者はオリジンが異なるので `-allow-origin` の指定が必要です。


対応している機種
----------------

| ID | 機種 |
|---|---|
| `stealth` | CR STEALTH block.III |
| `vb` | CR ウイルスブレイカー |

実機で検証できない機種は移植していません。経過時間からラウンド数を逆推定するような
ヒューリスティックは、実機の信号タイミングが無いと正しさを確認できないためです
（[ADR-0010](docs/adr/0010-no-blind-port-of-unverifiable-machines.md)）。


ライセンス
----------

MIT License
