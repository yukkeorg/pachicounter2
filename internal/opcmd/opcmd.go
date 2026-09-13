// Package opcmd は操作コマンドを実装する。
//
// 操作コマンドは、記録を変える操作（補正、新しいセッションの開始）をコアに頼み、その前後に
// 今の数字を 1 回だけ確かめるためのコマンドである。数字を出し続けることや見た目は
// 受け持たず、それはフロントの役目である（CONTEXT.md）。コアと同じバイナリの
// サブコマンドとして動く。詳細は docs/adr/0011-operation-command-as-subcommand.md を参照。
package opcmd

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/yukkeorg/pachicounter2/api"
)

// 終了コード。コアと同じ決まりにする。
const (
	exitOK    = 0
	exitFail  = 1 // 操作が失敗した。コアが断った、繋がらない、確認で取りやめた
	exitUsage = 2 // 引数の誤り
)

// Env は操作コマンドが使う入出力。テストでは差し替える。
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// StdinIsTerminal は標準入力が端末かどうか。新しいセッションの確認を聞けるかが決まる。
	StdinIsTerminal bool

	// StderrIsTerminal は標準エラーが端末かどうか。端末でなければショートカットなどから
	// 動かされていてエラーが目に入らないので、失敗をデスクトップ通知でも知らせる。
	StderrIsTerminal bool

	// Notify はデスクトップ通知を出す。nil なら通知しない。
	Notify func(summary, body string)

	// HTTPClient はコアとの通信に使う。
	HTTPClient *http.Client
}

// DefaultEnv は実際の端末とデスクトップ通知を使う Env を返す。
func DefaultEnv() Env {
	return Env{
		Stdin:            os.Stdin,
		Stdout:           os.Stdout,
		Stderr:           os.Stderr,
		StdinIsTerminal:  isTerminal(os.Stdin),
		StderrIsTerminal: isTerminal(os.Stderr),
		Notify:           notifyDesktop,
		// コアは操作を受け付けるまで最大 2 秒、結果を返すまで最大 5 秒待つので、
		// それより長くしておく。
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// command はサブコマンド 1 つ。
type command struct {
	// name は 1 つ目の引数として指定する名前。
	name string

	// display は一覧や通知に出す名前。
	display string

	// summary は一覧に出す短い説明。
	summary string

	// usage は -h や引数の誤りのときに出す使い方。
	usage func() string

	run func(env Env, args []string) error
}

func commands() []command {
	return []command{
		{name: "correct", display: "correct", summary: "カウンタを補正する", usage: correctUsage, run: runCorrect},
		{name: "session", display: "session new", summary: "新しいセッションを始める", usage: sessionUsage, run: runSession},
		{name: "status", display: "status", summary: "今の数字を 1 回だけ表示する", usage: statusUsage, run: runStatus},
	}
}

// Summary はサブコマンドの一覧に出す 1 行。
type Summary struct {
	Name string
	Text string
}

// Summaries はサブコマンドの一覧を返す。コアの使い方に並べるために使う。
func Summaries() []Summary {
	all := commands()
	out := make([]Summary, 0, len(all))
	for _, c := range all {
		out = append(out, Summary{Name: c.display, Text: c.summary})
	}
	return out
}

// usageError は引数の誤り。使い方と一緒に出し、終了コードを 2 にする。
type usageError struct {
	err error
}

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

func usagef(format string, a ...any) error {
	return &usageError{err: fmt.Errorf(format, a...)}
}

// errCanceled は、確認で取りやめたことを表す。
var errCanceled = errors.New("新しいセッションは始めませんでした")

// Run は name のサブコマンドを動かし、終了コードを返す。出力と通知もここで出す。
func Run(name string, args []string, env Env) int {
	var cmd command
	found := false
	names := []string{}
	for _, c := range commands() {
		names = append(names, c.name)
		if c.name == name {
			cmd, found = c, true
		}
	}
	if !found {
		msg := fmt.Sprintf("不明なサブコマンドです: %s（使えるサブコマンド: %s）", name, strings.Join(names, ", "))
		fmt.Fprintln(env.Stderr, "エラー: "+msg)
		notifyFailure(env, name, msg)
		return exitUsage
	}

	err := cmd.run(env, args)

	var usage *usageError
	switch {
	case err == nil:
		return exitOK

	case errors.Is(err, flag.ErrHelp):
		fmt.Fprint(env.Stderr, cmd.usage())
		return exitOK

	case errors.Is(err, errCanceled):
		fmt.Fprintln(env.Stderr, err.Error())
		return exitFail

	case errors.As(err, &usage):
		fmt.Fprintln(env.Stderr, "エラー: "+err.Error())
		fmt.Fprintln(env.Stderr)
		fmt.Fprint(env.Stderr, cmd.usage())
		notifyFailure(env, cmd.display, err.Error())
		return exitUsage

	default:
		fmt.Fprintln(env.Stderr, "エラー: "+err.Error())
		notifyFailure(env, cmd.display, err.Error())
		return exitFail
	}
}

// notifyFailure は、端末の外から動かされたときだけ失敗をデスクトップ通知で知らせる。
// 成功は知らせない。成功はフロントの数字で確かめられ、配信に通知が映り込む機会を減らせる。
func notifyFailure(env Env, display, message string) {
	if env.StderrIsTerminal || env.Notify == nil {
		return
	}
	env.Notify("PachiCounter: "+display+" に失敗しました", message)
}

// newFlagSet はサブコマンド用の FlagSet を作る。flag パッケージは誤りを見つけると
// 英語のエラーと使い方を自分で出すので黙らせ、出し方は Run で揃える。
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

// parseFlags は fs で args を読む。-h は flag.ErrHelp のまま返し、それ以外の誤りは
// 引数の誤りにする。
//
// flag パッケージは最初の位置引数の手前で読むのをやめるので、オプションは位置引数より
// 前に書く。後ろに書いたオプションは位置引数として残り、各サブコマンドが
// 「引数が多すぎる」として断る。
func parseFlags(fs *flag.FlagSet, args []string) error {
	err := fs.Parse(args)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, flag.ErrHelp):
		return err
	default:
		return &usageError{err: errors.New(describeFlagError(err))}
	}
}

// describeFlagError は flag パッケージのエラーを表示用の文にする。コアの使い方と同じく、
// 定義されていないオプションだけは日本語にする。
func describeFlagError(err error) string {
	if name, ok := strings.CutPrefix(err.Error(), "flag provided but not defined: "); ok {
		return "不明なオプションです: " + name
	}
	return err.Error()
}

// coreFlag は、繋ぐコアを指定する -core を fs に足す。
func coreFlag(fs *flag.FlagSet) *string {
	return fs.String("core", api.DefaultAddr, "")
}

const coreOptionUsage = "  -core アドレス  繋ぐコア。コアの -listen と同じ host:port の形（既定 " + api.DefaultAddr + "）\n"

func correctUsage() string {
	var b strings.Builder
	b.WriteString("使い方:\n  pachicounter correct [-core アドレス] <カウンタ> <+量|-量> [理由]\n\n")
	b.WriteString("台の実際とずれたカウンタを、増減の量を指定して直す。量には必ず符号を付ける。\n")
	b.WriteString("値を直接設定する操作はない。理由は記録に残る。\n\n")
	b.WriteString("カウンタ:\n")
	for _, c := range counters {
		fmt.Fprintf(&b, "  %-18s %s\n", c.Name, c.Label)
	}
	b.WriteString("\nオプション:\n")
	b.WriteString(coreOptionUsage)
	return b.String()
}

func runCorrect(env Env, args []string) error {
	fs := newFlagSet("correct")
	addr := coreFlag(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	conn, err := newCoreConn(*addr, env.HTTPClient)
	if err != nil {
		return err
	}

	rest := fs.Args()
	switch {
	case len(rest) < 2:
		return usagef("カウンタと補正の量を指定してください（例: pachicounter correct normal_rotations +1）")
	case len(rest) > 3:
		return usagef("引数が多すぎます（理由に空白を含めるときは \"...\" で囲んでください）")
	}

	name := rest[0]
	label, ok := counterLabel(name)
	if !ok {
		return usagef("補正できないカウンタです: %s（使える名前: %s）", name, strings.Join(counterNames(), ", "))
	}
	delta, err := parseDelta(rest[1])
	if err != nil {
		return &usageError{err: err}
	}
	var note string
	if len(rest) == 3 {
		note = rest[2]
	}

	snap, err := conn.correct(api.CorrectRequest{Counter: name, Delta: delta, Note: note})
	if err != nil {
		return err
	}

	// 応答は補正を適用した後のスナップショットなので、補正前の値は量を引いて求める。
	after, _ := counterValue(snap.Counters, name)
	fmt.Fprintf(env.Stdout, "%s  %d → %d\n", label, after-delta, after)
	return nil
}

// parseDelta は補正の量を読む。符号を必須にする。
//
// 補正は増減であって値の設定ではない（CONTEXT.md の「補正」）。符号の無い数を許すと、
// 「310 にしたい」つもりの 310 が 310 の増加として送られてしまう。
func parseDelta(s string) (int, error) {
	if !strings.HasPrefix(s, "+") && !strings.HasPrefix(s, "-") {
		return 0, fmt.Errorf("補正の量には符号を付けてください（例: +1, -1）。値を設定する操作はありません: %s", s)
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("補正の量を整数として読めません（例: +1, -1）: %s", s)
	}
	if n == 0 {
		return 0, fmt.Errorf("補正の量が 0 です: %s", s)
	}
	return n, nil
}

func sessionUsage() string {
	return "使い方:\n  pachicounter session new [-core アドレス] [-yes] [覚書]\n\n" +
		"新しいセッションを始める。今のセッションには戻れない。端末から打つと確認を求め、\n" +
		"ショートカットなど端末以外から動かすときは -yes が必要。覚書は記録に残る。\n\n" +
		"オプション:\n" + coreOptionUsage +
		"  -yes            確認せずに始める\n"
}

func runSession(env Env, args []string) error {
	if len(args) == 0 {
		return usagef("session の操作を指定してください（使える操作: new）")
	}
	switch args[0] {
	case "new":
	case "-h", "-help", "--help":
		return flag.ErrHelp
	default:
		return usagef("不明な session の操作です: %s（使える操作: new）", args[0])
	}

	fs := newFlagSet("session new")
	addr := coreFlag(fs)
	yes := fs.Bool("yes", false, "")
	if err := parseFlags(fs, args[1:]); err != nil {
		return err
	}
	conn, err := newCoreConn(*addr, env.HTTPClient)
	if err != nil {
		return err
	}

	rest := fs.Args()
	if len(rest) > 1 {
		return usagef("引数が多すぎます（覚書に空白を含めるときは \"...\" で囲んでください）")
	}
	var note string
	if len(rest) == 1 {
		note = rest[0]
	}

	// 新しいセッションを始めると、今のセッションには戻れない。端末からなら確認し、
	// 端末でなければ -yes が無い限り始めない。ショートカットの設定を間違えても、
	// 配信中のセッションが黙って切れないようにするため。
	if !*yes {
		if !env.StdinIsTerminal {
			return usagef("新しいセッションを始めると今のセッションには戻れません。端末以外から始めるときは -yes を付けてください")
		}
		ok, err := confirmNewSession(env, conn)
		if err != nil {
			return err
		}
		if !ok {
			return errCanceled
		}
	}

	snap, err := conn.newSession(note)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "新しいセッションを始めました: %s\n", snap.Session.ID)
	return nil
}

// confirmNewSession は、閉じるセッションを見せて続けるかを聞く。
func confirmNewSession(env Env, conn *coreConn) (bool, error) {
	snap, err := conn.snapshot()
	if err != nil {
		return false, err
	}
	if snap.Session.ID == "" {
		// 再生中のコアはセッションを持たず、頼んでも断る。閉じるものが無いので聞かずに
		// 送り、断られた理由をそのまま見せる。
		return true, nil
	}

	fmt.Fprintf(env.Stderr, "今のセッション %s\n（大当り間回転数 %d / 大当り回数 %d）を閉じて、\n新しいセッションを始めます。元には戻せません。続けますか？ [y/N] ",
		snap.Session.ID, snap.Counters.CurrentRotations, snap.Counters.Bonuses)

	line, err := bufio.NewReader(env.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("答えを読めません: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

func statusUsage() string {
	return "使い方:\n  pachicounter status [-core アドレス]\n\n" +
		"今の数字を 1 回だけ表示する。表示し続けることはしない（それはフロントの役目）。\n\n" +
		"オプション:\n" + coreOptionUsage
}

func runStatus(env Env, args []string) error {
	fs := newFlagSet("status")
	addr := coreFlag(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usagef("不明な引数です: %s", strings.Join(fs.Args(), " "))
	}
	conn, err := newCoreConn(*addr, env.HTTPClient)
	if err != nil {
		return err
	}

	snap, err := conn.snapshot()
	if err != nil {
		return err
	}
	printStatus(env.Stdout, snap)
	return nil
}
