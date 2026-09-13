// Command pachicounter はパチンコ台の外部情報出力端子から出る信号を集計し、
// 現在の回転数や初当たり確率を配信するコアである。
//
// 見た目は持たない。HTTP で待ち受け、集計結果のスナップショットを SSE で流すので、
// フロントは何個でも好きな技術で繋げる。
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/yukkeorg/pachicounter2/internal/app"
	"github.com/yukkeorg/pachicounter2/internal/config"
	"github.com/yukkeorg/pachicounter2/internal/httpapi"
	"github.com/yukkeorg/pachicounter2/internal/source"
	"github.com/yukkeorg/pachicounter2/internal/store"
	"github.com/yukkeorg/pachicounter2/pkg/machine"
	pcsignal "github.com/yukkeorg/pachicounter2/pkg/signal"

	// 機種プラグイン・信号源・デバイスドライバは動的ロードせず、ここでの import に
	// よってバイナリに含める。詳細は
	// docs/adr/0003-compile-time-plugin-registration.md を参照。
	_ "github.com/yukkeorg/pachicounter2/internal/hidgpio/usbio2"
	_ "github.com/yukkeorg/pachicounter2/internal/source/replay"
	_ "github.com/yukkeorg/pachicounter2/internal/source/usbhid"
	_ "github.com/yukkeorg/pachicounter2/pkg/machine/stealth"
	_ "github.com/yukkeorg/pachicounter2/pkg/machine/vb"
)

func main() {
	err := run(os.Args[1:])

	var usage *usageError
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		// -h で使い方を出したのはエラーではない。
	case errors.As(err, &usage):
		// エラーと使い方は parseFlags が出している。
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "エラー: "+err.Error())
		os.Exit(1)
	}
}

// usageError はオプションの誤り。parseFlags がエラーと使い方を出したあとに返す。
// 終了コードは flag パッケージの ExitOnError と同じ 2 にする。
type usageError struct {
	err error
}

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

type options struct {
	machineID string
	variant   string

	sourceSpec string

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
	listSources  bool

	// setFlags には明示的に指定されたフラグ名が入る。再生時に、指定の無かった
	// 分だけログの記録で埋めるために使う。
	setFlags map[string]bool
}

func run(args []string) error {
	opts, err := parseFlags(args, os.Stderr)
	if err != nil {
		return err
	}

	log := newLogger(opts.logLevel)

	if opts.listMachines {
		return printMachines()
	}
	if opts.listSources {
		return printSources()
	}

	src, reg, err := source.Open(opts.sourceSpec, source.Env{Logger: log})
	if err != nil {
		return err
	}
	applyRecordedSettings(&opts, src, log)

	if opts.machineID == "" {
		return fmt.Errorf("機種を指定してください（例: pachicounter -machine stealth）。-list-machines で一覧が出ます")
	}

	wiring, err := config.ParseWiring(opts.wireSpec)
	if err != nil {
		return err
	}
	wiring.ActiveLow = opts.activeLow

	// 再生は記録を流し直して確かめるための実行なので、保存先を開かず何も書かない。
	// 実機と同じ保存先を使うと、直近のセッションの続きとみなされ、再生した信号が
	// そのログに混ざる。詳細は docs/adr/0006-raw-signal-log-plus-snapshot.md を参照。
	replaying := reg.Kind == source.KindReplay

	var st *store.Store
	if !replaying {
		st, err = store.Open(opts.stateDir)
		if err != nil {
			return err
		}
	}

	core, err := app.New(app.Options{
		MachineID:       opts.machineID,
		Variant:         opts.variant,
		Wiring:          wiring,
		Tuning:          opts.tuning,
		Ops:             opts.ops,
		Source:          src,
		Store:           st,
		Replay:          replaying,
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

	attrs := []any{
		"machine", machineSpec(opts.machineID, opts.variant),
		"source", src.Name(),
		"wiring", wiring.String(),
	}
	if replaying {
		attrs = append(attrs, "state_dir", "（再生中は記録しない）")
	} else {
		attrs = append(attrs, "state_dir", st.Dir(), "session", core.SessionID())
	}
	log.Info("PachiCounter", attrs...)

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

func parseFlags(args []string, stderr io.Writer) (options, error) {
	var opts options

	defaultTuning := config.DefaultTuning()
	defaultOps := config.DefaultOps()
	defaultWiring := config.DefaultWiring()

	fs := flag.NewFlagSet("pachicounter", flag.ContinueOnError)
	// flag パッケージは誤りを見つけると、英語のエラーと使い方を自分で出す。エラーと
	// 使い方はここで順番を決めて 1 回だけ出すので、Parse の間は黙らせる。
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}

	fs.StringVar(&opts.machineID, "machine", "", "集計する機種の ID（-list-machines で一覧）")
	fs.StringVar(&opts.variant, "variant", "", "機種のバリアント（スペック違いがある機種のみ）")

	fs.StringVar(&opts.sourceSpec, "source", "usbhid",
		"信号源。名前 または 名前:引数 で指定する（-list-sources で一覧と書き方）")

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
	fs.BoolVar(&opts.listSources, "list-sources", false, "登録されている信号源と、その書き方を一覧する")

	if err := fs.Parse(args); err != nil {
		fs.SetOutput(stderr)
		if errors.Is(err, flag.ErrHelp) {
			printUsage(fs)
			return opts, err
		}
		fmt.Fprintln(stderr, "エラー: "+describeFlagError(err))
		fmt.Fprintln(stderr)
		printUsage(fs)
		return opts, &usageError{err: err}
	}

	// 位置引数は受け付けない。黙って無視すると、操作コマンドのつもりの打ち間違い
	// （pachicounter corect …）でコアがもう 1 つ起動し、動作中のコアのセッションに
	// 書き込んでしまう。詳細は docs/adr/0011-operation-command-as-subcommand.md を参照。
	if fs.NArg() > 0 {
		err := fmt.Errorf("不明な引数です: %s", strings.Join(fs.Args(), " "))
		fs.SetOutput(stderr)
		fmt.Fprintln(stderr, "エラー: "+err.Error())
		fmt.Fprintln(stderr)
		printUsage(fs)
		return opts, &usageError{err: err}
	}

	opts.setFlags = map[string]bool{}
	fs.Visit(func(f *flag.Flag) { opts.setFlags[f.Name] = true })
	return opts, nil
}

// printUsage は使い方を書く。
func printUsage(fs *flag.FlagSet) {
	out := fs.Output()
	fmt.Fprintln(out, "pachicounter - パチンコ台のデータカウンター（コア）")
	fmt.Fprintln(out, "\n使い方:")
	fmt.Fprintln(out, "  pachicounter -machine <機種 ID> [オプション]")
	fmt.Fprintln(out, "\n例:")
	fmt.Fprintln(out, "  pachicounter -machine stealth")
	fmt.Fprintln(out, "  pachicounter -machine vb -rotation-rate 18")
	fmt.Fprintln(out, "  pachicounter -machine stealth -source usbhid:driver=usbio2")
	fmt.Fprintln(out, "  pachicounter -source file:session.jsonl")
	fmt.Fprintln(out, "\nオプション:")
	fs.PrintDefaults()
}

// describeFlagError は flag パッケージのエラーを表示用の文にする。定義されていない
// オプションだけは、いちばん起きやすい誤りなので日本語にする。
func describeFlagError(err error) string {
	if name, ok := strings.CutPrefix(err.Error(), "flag provided but not defined: "); ok {
		return "不明なオプションです: " + name
	}
	return err.Error()
}

// applyRecordedSettings は記録を流す信号源のとき、明示指定の無かった設定を
// 記録された内容で埋める。
//
// 当時の配線が分からないとポート値を解釈できないため、配線をログの 1 行目に
// 書いている。せっかく書いたものを読まずに既定の配線で解釈すると、記録と
// 違う配線で集計してしまう。
func applyRecordedSettings(opts *options, src pcsignal.Source, log *slog.Logger) {
	recording, ok := src.(source.Recording)
	if !ok {
		return
	}

	rec, err := recording.Recorded()
	if err != nil {
		// 記録が読めなくても、指定されたフラグだけで動かせる。
		log.Warn("記録された設定を読めませんでした。指定された設定で再生します", "err", err)
		return
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

	if !opts.setFlags["wire"] && len(rec.Wiring.Bits) > 0 {
		opts.wireSpec = rec.Wiring.Spec()
		if !opts.setFlags["active-low"] {
			opts.activeLow = rec.Wiring.ActiveLow
		}
		log.Info("記録された配線を使います", "wiring", rec.Wiring.String())
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

func printSources() error {
	all := source.All()
	if len(all) == 0 {
		fmt.Println("登録されている信号源がありません。")
		return nil
	}

	// 名前の列（"  " + 10 桁 + " "）に揃えて字下げする。
	const indent = "             "

	fmt.Println("登録されている信号源:")
	for _, reg := range all {
		fmt.Printf("  %-10s %s\n", reg.Name, reg.Summary)
		fmt.Printf("%s書き方: %s\n", indent, reg.Usage)
		if reg.PrintDetails == nil {
			continue
		}

		var details bytes.Buffer
		reg.PrintDetails(&details)
		for _, line := range strings.Split(strings.TrimRight(details.String(), "\n"), "\n") {
			fmt.Printf("%s%s\n", indent, line)
		}
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
