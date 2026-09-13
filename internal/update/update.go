// Package update は GitHub Releases から最新のリリースタグを読み、
// 手元の版と比べて更新の有無を決める。更新そのものは Release 添付の install.sh に委ねるため、
// この package はバイナリの取得・検証・置換を持たない。
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/HappyOnigiri/WX/internal/version"
)

// 配布元は固定である。scripts/install.sh も同じ repository のリリース資産だけを取得する。
const (
	ownerRepository = "HappyOnigiri/WX"
	// LatestReleaseAPI は draft と prerelease を除いた最新リリースを返す。
	// 公開に失敗して draft のまま残ったリリースを拾わないため、tags API ではなくこれを使う。
	LatestReleaseAPI = "https://api.github.com/repos/" + ownerRepository + "/releases/latest"
	// ReleasesPage は案内に載せる人間向けの一覧である。
	ReleasesPage = "https://github.com/" + ownerRepository + "/releases"
)

// ErrRateLimited は未認証のレート制限に触れたことを表す。
// 案内の文面と再試行の扱いをネットワーク障害と分けるため、他の失敗と区別する。
var ErrRateLimited = errors.New("github rate limit reached")

// ErrUnavailable は最新リリースをまだ1つも公開していない状態である。
var ErrUnavailable = errors.New("no published release")

// Release は更新の判断に使うリリースの要点である。
type Release struct {
	// Tag は vX.Y.Z 形式のリリースタグ。
	Tag string
	// URL はリリースの人間向け page で、案内に載せる。
	URL string
}

// InstallScriptURL は指定タグに添付された install.sh の取得先を返す。
func InstallScriptURL(tag string) string {
	return "https://github.com/" + ownerRepository + "/releases/download/" + tag + "/install.sh"
}

// ReleaseBuild は配布用ビルドかどうかを返す。
// 開発ビルドは BuildMeta が dev のままで、これは Go の既定値でもあるためテストバイナリも必ず false になる。
// 自動確認と更新の実行はこれが true のときだけ行う。
func ReleaseBuild() bool { return version.BuildMeta == "" }

// CurrentVersion は手元のバイナリの表示版である。開発ビルドでは -dev が付き、Newer の比較対象にならない。
func CurrentVersion() string { return version.String() }

// Checker は最新リリースの取得口である。Endpoint と Client は試験で差し替える。
type Checker struct {
	// Endpoint は空なら LatestReleaseAPI を使う。
	Endpoint string
	// Client は空なら http.DefaultClient を使う。上限は呼び出し側が context に付ける。
	Client *http.Client
}

// maxResponseBytes は応答の読み取り上限である。リリース説明文は長くなりうるので、必要な field を読める範囲で切る。
const maxResponseBytes = 1 << 20

// Latest は公開済みの最新リリースを返す。tag_name が vX.Y.Z でない応答は ErrUnavailable にする。
func (c Checker) Latest(ctx context.Context) (Release, error) {
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = LatestReleaseAPI
	}
	client := c.Client
	if client == nil {
		// client 側に上限を持たせると、呼び出し側が宣言した context の期限より先に切れて
		// 宣言が効かなくなる。呼び出しはいずれも期限付きの context を渡す。
		client = http.DefaultClient
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Release{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "wx/"+version.String())
	response, err := client.Do(request)
	if err != nil {
		return Release{}, err
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusOK:
	case response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusTooManyRequests:
		return Release{}, ErrRateLimited
	case response.StatusCode == http.StatusNotFound:
		return Release{}, ErrUnavailable
	default:
		return Release{}, fmt.Errorf("github responded %s", response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return Release{}, err
	}
	var payload struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Release{}, err
	}
	if _, ok := ParseVersion(payload.TagName); !ok {
		return Release{}, ErrUnavailable
	}
	release := Release{Tag: payload.TagName, URL: payload.HTMLURL}
	if release.URL == "" {
		release.URL = ReleasesPage
	}
	return release, nil
}

// Version はリリースタグの3整数である。タグは vX.Y.Z 固定で prerelease を許さないため、
// golang.org/x/mod を直接依存へ昇格させず、この形だけを扱う。
type Version struct{ Major, Minor, Patch int }

// ParseVersion は vX.Y.Z を読む。先行ゼロ・prerelease・-dev 付きの開発ビルド表示は受け付けない。
func ParseVersion(value string) (Version, bool) {
	rest, found := strings.CutPrefix(value, "v")
	if !found {
		return Version{}, false
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	numbers := make([]int, 0, 3)
	for _, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return Version{}, false
		}
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			return Version{}, false
		}
		numbers = append(numbers, number)
	}
	return Version{Major: numbers[0], Minor: numbers[1], Patch: numbers[2]}, true
}

// Newer は candidate が current より新しいリリースかを返す。
// どちらかが vX.Y.Z でないときは false を返し、開発ビルドや git describe 由来の表示では案内を出さない。
func Newer(current, candidate string) bool {
	base, ok := ParseVersion(current)
	if !ok {
		return false
	}
	next, ok := ParseVersion(candidate)
	if !ok {
		return false
	}
	switch {
	case next.Major != base.Major:
		return next.Major > base.Major
	case next.Minor != base.Minor:
		return next.Minor > base.Minor
	default:
		return next.Patch > base.Patch
	}
}
