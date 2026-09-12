# 信号源を3段に分け、イベントストリーム型のインタフェースで抽象する

当初この ADR は「OS 別の pure Go 実装でハードウェア層を持つ」としていたが、2 つの軸を 1 つに
潰していた。OS 別トランスポート（hidraw / Win32 HID）は USB-IO 2.0 という**1 つの信号源の実装
詳細**にすぎず、抽象の主役に据えるものではない。実際 USB-HID で GPIO を出す製品は複数あり
（km2net USB-IO 2.0、Microchip MCP2221A、FTDI FT260 など）、VID/PID もレポート長もコマンド形式も
違う。加えて Raspberry Pi の GPIO を直接読む経路、さらに Linux では `hid-mcp2221` のように
カーネルが HID GPIO デバイスを gpiochip として見せる経路もある。

そこで 3 段に分ける。

1. **信号源（`pkg/signal.Source`）** — USB-HID GPIO デバイス / Raspberry Pi GPIO / 記録再生 / dummy
2. **デバイスドライバ（`internal/hidgpio`）** — USB-IO 2.0 / 他の HID GPIO 製品。VID/PID で
   自動検出し、設定で明示指定もできる
3. **OS 別 HID トランスポート（`internal/hid`）** — Linux は `/dev/hidrawN`、Windows は
   `setupapi.dll` + `hid.dll`。どちらも pure Go（Win32 API は syscall で呼べる）

## インタフェースはイベントストリーム型にする

旧 Python の `HwReceiver.get_port_value()` をそのまま踏襲するとポーリング型に固定され、
次の 4 つを失う。

- **Raspberry Pi GPIO の最大の利点**（カーネルのエッジ割り込みで通知でき、ポーリング不要で
  取りこぼしも無い）がインタフェースに押し潰される
- **マイコンを信号源にできない**（Pico/Arduino 側でエッジを検出して push する形が自然）
- **タイムスタンプを誰が打つのか決まらない**（ADR-0006 の生信号ログは時刻を持つ）
- **記録再生が信号源になれない**

したがって `Source` は「変化後のポート値＋単調増加時刻＋実時刻」のイベントを流すものとする。
ポーリングは USB-HID GPIO 信号源の内部に隠れる実装詳細になり、タイムスタンプの責任は信号源に
固定され、ADR-0006 のログ形式と一致する。そして**記録再生が `Source` の 1 実装になる**ので、
コア本体を `--source file:session.jsonl` で起動すれば、集計・スナップショット・SSE・フロントを
まとめて実機なしで検証できる。

デバウンスは信号源の外（集計側）に置く。全信号源が恩恵を受け、カーネル側でデバウンスできる
GPIO では無効化できる。

## cgo を持ち込まない

既製の Go 向け HID ライブラリはほぼすべて cgo + hidapi である（`karalabe/hid` 系は hidapi を
同梱するので外部依存は無いが cgo は必須、`sstallion/go-hid` は cgo に加えて hidapi の別途
インストールを要求する）。cgo なしの `zserge/hid` は Linux の 386/amd64 限定で、Raspberry Pi
（arm64）では使えない。よって第 3 段は自前で書く。単一静的バイナリを配信機や Raspberry Pi に
scp するだけで動く状態を守るためで、cgo が入るとクロスコンパイルに各ターゲットの C
ツールチェーンが必要になる。当面の対象は Linux と Windows。macOS が必要になった時点で IOKit の
ために cgo か purego が避けられなくなるが、その実装は `darwin` の build tag に閉じる。

## ポーリング間隔とデバウンス

USB-HID GPIO 信号源のポーリング間隔は 5ms（200Hz）を既定とする。旧実装の 50ms（20Hz）から
意図的に上げている。USB-IO 2.0 は 300Hz まで耐える仕様であり
（http://km2net.com/usb-io2.0/index.shtml）、スタート信号のパルス幅は配線資料に記載が無いため、
20ms 程度のパルスでも取りこぼさない余裕を確保する。回転の取りこぼしは「たまに 1 回転ずれる」
という気づきにくい壊れ方をするので、速い側に倒す。代わりに接点バウンスを拾う危険が出るため、
同一ビットの再エッジを一定時間無視するソフトウェアデバウンスを入れ、その時間を設定可能にする
（旧実装にデバウンスは無い）。
