// Package version は CLI と daemon が共有する build 版情報を提供する。
package version

// Version は linker flag で埋め込む build 版である。
var Version = "undefined"

// BuildMeta は build 版に付加するメタデータである。
var BuildMeta = "dev"

// String は build 版とメタデータを表示用に結合する。
func String() string {
	if BuildMeta == "" {
		return Version
	}
	return Version + "-" + BuildMeta
}

// EmbeddedString は埋め込み版が設定されている場合だけ表示用の版を返す。
func EmbeddedString() (string, bool) {
	if Version == "" || Version == "undefined" {
		return "", false
	}
	return String(), true
}
