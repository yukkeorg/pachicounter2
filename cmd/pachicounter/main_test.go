package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestParseFlagsRejectsPositionalArguments(t *testing.T) {
	cases := [][]string{
		// 操作コマンドのつもりで打ち間違えた。
		{"corect", "normal_rotations", "+1"},
		// オプションの後ろに余計な語が付いた。
		{"-machine", "stealth", "extra"},
	}

	for _, args := range cases {
		var stderr bytes.Buffer
		_, err := parseFlags(args, &stderr)

		var usage *usageError
		if !errors.As(err, &usage) {
			t.Errorf("parseFlags(%q) のエラー = %v, 期待は引数の誤り", args, err)
			continue
		}
		if !strings.Contains(stderr.String(), "不明な引数です") {
			t.Errorf("parseFlags(%q) の出力に理由が無い: %q", args, stderr.String())
		}
	}
}

func TestParseFlagsAcceptsOptionsOnly(t *testing.T) {
	opts, err := parseFlags([]string{"-machine", "stealth"}, io.Discard)
	if err != nil {
		t.Fatalf("オプションだけの指定がエラーになった: %v", err)
	}
	if opts.machineID != "stealth" {
		t.Errorf("機種 = %q, 期待は stealth", opts.machineID)
	}
}
