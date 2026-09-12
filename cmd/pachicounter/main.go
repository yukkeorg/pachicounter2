// Command pachicounter はパチンコ台の外部情報出力端子から出る信号を集計し、
// 現在の回転数や初当たり確率を配信するコアである。
//
// 見た目は持たない。HTTP で待ち受け、集計結果のスナップショットを SSE で流すので、
// フロントは何個でも好きな技術で繋げる。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/yukkeorg/pachicounter2/internal/app"
	"github.com/yukkeorg/pachicounter2/internal/config"
	"github.com/yukkeorg/pachicounter2/internal/hidgpio"
	"github.com/yukkeorg/pachicounter2/internal/httpapi"
	"github.com/yukkeorg/pachicounter2/internal/source/dummy"
	"github.com/yukkeorg/pachicounter2/internal/source/replay"
	"github.com/yukkeorg/pachicounter2/internal/source/usbhid"
	"github.com/yukkeorg/pachicounter2/internal/store"
	"github.com/yukkeorg/pachicounter2/pkg/machine"
	pcsignal "github.com/yukkeorg/pachicounter2/pkg/signal"

	// 機種プラグインとデバイスドライバは動的ロードせず、ここでの import によって
	// バイナリに含める。詳細は
	// docs/adr/0003-compile-time-plugin-registration.md を参照。
	_ "github.com/yukkeorg/pachicounter2/internal/hidgpio/usbio2"
	_ "github.com/yukkeorg/pachicounter2/pkg/machine/stealth"
	_ "github.com/yukkeorg/pachicounter2/pkg/machine/vb"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "エラー: "+err.Error())
		os.Exit(1)
	}
}

type options struct {
	machineID string
	variant   string

	sourceSpec string
	driver     string

	wireSpec  string
	activeLow bool

	tuning config.Tuning
	ops    config.Ops

	listen      string
	allowOrigin string
	frontDir    string

	stateDir   string
	newSession bool

	logLevel     string
	listMachines bool
	listDevices  bool

	// setFlags には明示的に指定されたフラグ名が入る。再生時に、指定の無かった
	// 分だけログの記録で埋めるために使う。
	setFlags map[string]bool
}

func run(args []string) error {
	opts, err := parseFlags(args)
	if err != nil {
		return err
	}

	log := newLogger(opts.logLevel)

	if opts.listMachines {
		return printMachines()
	}
	if opts.listDevices {
		return printDevices()
	}
	if err := applyRecordedSettings(&opts, log); err != nil {
		return err
	}

	if opts.machineID == "" {
		return fmt.Errorf("機種を指定してください（例: pachicounter -machine stealth）。-list-machines で一覧が出ます")
	}

	wiring, err := config.ParseWiring(opts.wireSpec)
	if err != nil {
		return err
	}
	wiring.ActiveLow = opts.activeLow

	src, err := buildSource(opts, log)
	if err != nil {
		return err
	}

	st, err := store.Open(opts.stateDir)
	if err != nil {
		return err
	}

	core, err := app.New(app.Options{
		MachineID:       opts.machineID,
		Variant:         opts.variant,
		Wiring:          wiring,
		Tuning:          opts.tuning,
		Ops:             opts.ops,
		Source:          src,
		Store:           st,
		ForceNewSession: opts.newSession,
		Logger:          log,
	})
	if err != nil {
		return err
	}
	defer core.Close()

	server, err := httpapi.New(core, httpapi.Options{
		Addr:        opts.listen,
		AllowOrigin: opts.allowOrigin,
		FrontDir:    opts.frontDir,
		Logger:      log,
	})
	if err != nil {
		return err
	}

	spec := machineSpec(opts.machineID, opts.variant)
	log.Info("PachiCounter",
		"machine", spec,
		"source", src.Name(),
		"wiring", wiring.String(),
		"state_dir", st.Dir(),
		"session", core.SessionID())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errs := make(chan error, 2)
	go func() { errs <- core.Run(ctx) }()
	go func() { errs <- server.Run(ctx) }()

	// 先に落ちた方のエラーを返す。もう一方は ctx の終了で片付く。
	err = <-errs
	stop()

	select {
	case second := <-errs:
		if err == nil {
			err = second
		}
	case <-time.After(5 * time.Second):
	}

	log.Info("終了します", "session", core.SessionID())
	return err
}

func parseFlags(args []string) (options, error) {
	var opts options

	defaultTuning := config.DefaultTuning()
	defaultOps := config.DefaultOps()
	defaultWiring := config.DefaultWiring()

	fs := flag.NewFlagSet("pachicounter", flag.ContinueOnError)
	fs.StringVar(&opts.machineID, "machine", "", "集計する機種の ID（-list-machines で一覧）")
	fs.StringVar(&opts.variant, "variant", "", "機種のバリアント（スペック違いがある機種のみ）")

	fs.StringVar(&opts.sourceSpec, "source", "usbhid",
		"信号源: usbhid | dummy | file:<生信号ログのパス> | loop:<生信号ログのパス>")
	fs.StringVar(&opts.driver, "driver", "",
		"USB-HID GPIO デバイスのドライバ名（空なら自動検出。-list-devices で一覧）")

	fs.StringVar(&opts.wireSpec, "wire", defaultWiring.Spec(),
		"配線: 役割=ビット位置 をカンマ区切りで指定する")
	fs.BoolVar(&opts.activeLow, "active-low", defaultWiring.ActiveLow,
		"信号が出ている間にビットが 0 になる配線かどうか")

	fs.Float64Var(&opts.tuning.FireRate, "fire-rate", defaultTuning.FireRate,
		"打ち出し速度（玉/秒）。台の調整値")
	fs.Float64Var(&opts.tuning.RotationRate, "rotation-rate", defaultTuning.RotationRate,
		"回転率（貸玉 250 個あたりの回転数）。台の調整値")
	fs.Float64Var(&opts.tuning.DensapoBase, "densapo-base", defaultTuning.DensapoBase,
		"電サポ中の玉持ち率（1 なら玉が減らない）。台の調整値")

	fs.DurationVar(&opts.ops.PollInterval, "poll-interval", defaultOps.PollInterval,
		"信号源のポーリング間隔")
	fs.DurationVar(&opts.ops.Debounce, "debounce", defaultOps.Debounce,
		"同一ビットの再エッジを無視する時間（0 で無効）")
	fs.Float64Var(&opts.ops.MaxSecPerRotation, "max-sec-per-rotation", defaultOps.MaxSecPerRotation,
		"1 回転あたりの秒数の上限。これを超えた間隔は離席とみなす")

	fs.StringVar(&opts.listen, "listen", "127.0.0.1:18888",
		"HTTP の待ち受けアドレス。LAN に開くときだけ明示的に変える")
	fs.StringVar(&opts.allowOrigin, "allow-origin", "",
		"Access-Control-Allow-Origin に入れる値。別ポートの開発サーバからフロントを繋ぐときに指定する")
	fs.StringVar(&opts.frontDir, "front-dir", "",
		"外部のフロントを置いたディレクトリ（空なら埋め込みを使う）")

	fs.StringVar(&opts.stateDir, "state-dir", "",
		"状態の保存先（空なら XDG に従う）")
	fs.BoolVar(&opts.newSession, "new-session", false,
		"続けられるセッションがあっても新しく始める")

	fs.StringVar(&opts.logLevel, "log-level", "info", "ログの詳しさ: debug | info | warn | error")
	fs.BoolVar(&opts.listMachines, "list-machines", false, "登録されている機種を一覧する")
	fs.BoolVar(&opts.listDevices, "list-devices", false, "登録されているデバイスドライバと検出結果を一覧する")

	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprintln(out, "pachicounter - パチンコ台のデータカウンター（コア）")
		fmt.Fprintln(out, "\n使い方:")
		fmt.Fprintln(out, "  pachicounter -machine <機種 ID> [オプション]")
		fmt.Fprintln(out, "\n例:")
		fmt.Fprintln(out, "  pachicounter -machine stealth")
		fmt.Fprintln(out, "  pachicounter -machine vb -rotation-rate 18")
		fmt.Fprintln(out, "  pachicounter -machine stealth -source file:session.jsonl")
		fmt.Fprintln(out, "\nオプション:")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, nil
		}
		return opts, err
	}

	opts.setFlags = map[string]bool{}
	fs.Visit(func(f *flag.Flag) { opts.setFlags[f.Name] = true })
	return opts, nil
}

// applyRecordedSettings は生信号ログを再生するとき、明示指定の無かった設定を
// 記録された内容で埋める。
//
// 当時の配線が分からないとポート値を解釈できないため、配線をログの 1 行目に
// 書いている。せっかく書いたものを読まずに既定の配線で解釈すると、記録と
// 違う配線で集計してしまう。
func applyRecordedSettings(opts *options, log *slog.Logger) error {
	if !strings.HasPrefix(opts.sourceSpec, "file:") && !strings.HasPrefix(opts.sourceSpec, "loop:") {
		return nil
	}

	path := opts.sourceSpec[len("file:"):]
	rec, err := store.ReadSessionStart(path)
	if err != nil {
		// 記録が読めなくても、指定されたフラグだけで動かせる。
		log.Warn("記録された設定を読めませんでした。指定された設定で再生します", "err", err)
		return nil
	}

	if !opts.setFlags["machine"] && rec.Machine != "" {
		opts.machineID = rec.Machine
		log.Info("記録された機種を使います", "machine", rec.Machine)
	}
	if !opts.setFlags["variant"] && rec.Variant != "" {
		opts.variant = rec.Variant
	}
	if opts.setFlags["machine"] && rec.Machine != "" && rec.Machine != opts.machineID {
		log.Warn("記録された機種と指定された機種が違います",
			"recorded", rec.Machine, "requested", opts.machineID)
	}

	if !opts.setFlags["wire"] && len(rec.Wiring) > 0 {
		wiring := config.Wiring{Bits: rec.Wiring, ActiveLow: rec.ActiveLow}
		opts.wireSpec = wiring.Spec()
		if !opts.setFlags["active-low"] {
			opts.activeLow = rec.ActiveLow
		}
		log.Info("記録された配線を使います", "wiring", wiring.String())
	}
	return nil
}

// buildSource は -source の指定から信号源を組み立てる。
func buildSource(opts options, log *slog.Logger) (pcsignal.Source, error) {
	spec := opts.sourceSpec

	switch {
	case spec == "usbhid":
		return usbhid.New(usbhid.Options{
			Driver:   opts.driver,
			Interval: opts.ops.PollInterval,
			Logger:   log,
		}), nil

	case spec == "dummy":
		return dummy.New(nil, true), nil

	case strings.HasPrefix(spec, "file:"), strings.HasPrefix(spec, "loop:"):
		// file: と loop: はどちらも 5 文字なので、同じ位置で切れる。
		path := spec[len("file:"):]
		return replay.New(replay.Options{
			Path:     path,
			Realtime: true,
			Speed:    1,
			Loop:     strings.HasPrefix(spec, "loop:"),
		})

	default:
		return nil, fmt.Errorf("信号源 %q は指定できません（usbhid | dummy | file:<パス> | loop:<パス>）", spec)
	}
}

func printMachines() error {
	all := machine.All()
	if len(all) == 0 {
		fmt.Println("登録されている機種がありません。")
		return nil
	}

	fmt.Println("登録されている機種:")
	for _, reg := range all {
		if len(reg.Variants) > 0 {
			fmt.Printf("  %-10s %s（バリアント: %s）\n", reg.ID, reg.DisplayName, strings.Join(reg.Variants, ", "))
			continue
		}
		fmt.Printf("  %-10s %s\n", reg.ID, reg.DisplayName)
	}
	return nil
}

func printDevices() error {
	names := hidgpio.Names()
	if len(names) == 0 {
		fmt.Println("登録されているデバイスドライバがありません。")
		return nil
	}

	fmt.Println("登録されているデバイスドライバ:")
	for _, name := range names {
		driver, _ := hidgpio.Lookup(name)
		fmt.Printf("  %-10s %s（入力 %d ビット）\n", driver.Name, driver.DisplayName, driver.NumBits)
	}

	fmt.Println("\n検出結果:")
	found, err := hidgpio.Detect("")
	switch {
	case errors.Is(err, hidgpio.ErrNoDevice):
		fmt.Println("  対応するデバイスは見つかりませんでした。")
	case err != nil:
		fmt.Printf("  検出に失敗しました: %v\n", err)
	default:
		fmt.Printf("  %s を %s で検出しました（VID 0x%04x / PID 0x%04x）\n",
			found.Driver.DisplayName, found.Info.Path, found.Info.VendorID, found.Info.ProductID)
	}
	return nil
}

func machineSpec(id, variant string) string {
	if variant == "" {
		return id
	}
	return id + "/" + variant
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		lv = slog.LevelInfo
	}

	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv})
	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger
}
