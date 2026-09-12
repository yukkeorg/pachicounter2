// Package web は既定のフロントをバイナリに埋め込む。
//
// ビルドに Node を要らなくするため、素の HTML と CSS と ES module にしてある。
// これで go build 一発で配布物が完成し、単一ファイルを配信機や Raspberry Pi に
// 置くだけで動く。React などを使いたい場合は --front-dir で外部ディレクトリを
// 指定すれば、こちらより優先される。詳細は
// docs/adr/0007-buildless-embedded-front-with-override.md を参照。
package web

import (
	"embed"
	"io/fs"
)

//go:embed index.html style.css app.js
var files embed.FS

// FS は埋め込んだフロントのファイルを返す。
func FS() fs.FS {
	return files
}
