package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
)

var ErrConfigChanged = errors.New("config changed after preview")

type EditOperation string

const (
	EditSet    EditOperation = "set"
	EditAdd    EditOperation = "add"
	EditRemove EditOperation = "remove"
	EditReset  EditOperation = "reset"
)

// EditRequest は CLI と TUI が共有する1項目分の設定変更である。
// Scope は global・workspace・repository のいずれかで、個別 scope では Target が必要になる。
type EditRequest struct {
	Scope  string
	Target string
	// Repository は v2 の repository 編集で使う workspace 相対 membership path
	// である。system/default/workspace scope と legacy v1 では空にする。
	Repository string
	// V2 は raw document がまだ v2 へ更新されていなくても、workspace/repository
	// scope で明示的な v2 editor を使う指定である。
	V2        bool
	Key       string
	Value     string
	Operation EditOperation
}

// EditPreview は検証済みの変更と、確認画面に出す変更前後の値を保持する。
// raw と digest は CommitEdit だけが使い、呼び出し側が確認後の内容を書き換えられないよう非公開にする。
type EditPreview struct {
	Request EditRequest
	Before  string
	After   string
	raw     Config
	digest  [sha256.Size]byte
}

// PreviewEdit は現在の設定を複製して1件の変更を適用し、保存せずに全体検証まで行う。
func PreviewEdit(request EditRequest) (EditPreview, error) {
	if request.Scope == "" {
		request.Scope = "global"
	}
	beforeDigest, err := configDigest()
	if err != nil {
		return EditPreview{}, err
	}
	raw, err := LoadRaw()
	if err != nil {
		return EditPreview{}, err
	}
	afterReadDigest, err := configDigest()
	if err != nil {
		return EditPreview{}, err
	}
	if beforeDigest != afterReadDigest {
		return EditPreview{}, ErrConfigChanged
	}
	before, err := editValue(raw, request)
	if err != nil {
		return EditPreview{}, err
	}
	if err := applyEdit(&raw, request); err != nil {
		return EditPreview{}, err
	}
	effective := Merge(Defaults(), raw)
	if err := NormalizePaths(&effective); err != nil {
		return EditPreview{}, err
	}
	if err := Validate(&effective); err != nil {
		return EditPreview{}, err
	}
	after, err := editValue(raw, request)
	if err != nil {
		return EditPreview{}, err
	}
	return EditPreview{Request: request, Before: before, After: after, raw: raw, digest: beforeDigest}, nil
}

// CommitEdit は preview 後の外部変更を検出してから atomic save を行う。
func CommitEdit(preview EditPreview) error {
	digest, err := configDigest()
	if err != nil {
		return err
	}
	if digest != preview.digest {
		return ErrConfigChanged
	}
	return Save(preview.raw)
}

func applyEdit(raw *Config, request EditRequest) error {
	if isV2EditRequest(*raw, request) {
		switch request.Operation {
		case EditSet:
			return SetV2Field(raw, request.Scope, request.Target, request.Repository, request.Key, request.Value)
		case EditAdd:
			return AppendV2List(raw, request.Scope, request.Target, request.Repository, request.Key, request.Value)
		case EditRemove:
			return RemoveV2List(raw, request.Scope, request.Target, request.Repository, request.Key, request.Value)
		case EditReset:
			if isV2ListKey(request.Scope, request.Key) {
				return ResetV2List(raw, request.Scope, request.Target, request.Repository, request.Key)
			}
			return ResetV2Field(raw, request.Scope, request.Target, request.Repository, request.Key)
		default:
			return fmt.Errorf("unknown config edit operation %q", request.Operation)
		}
	}
	if request.Scope == "global" {
		switch request.Operation {
		case EditAdd:
			return AppendList(raw, request.Key, request.Value)
		case EditRemove:
			return RemoveList(raw, request.Key, request.Value)
		case EditReset:
			if IsListKey(request.Key) {
				return ResetList(raw, request.Key)
			}
			return ResetField(raw, request.Key)
		case EditSet:
			return SetField(raw, request.Key, request.Value)
		default:
			return fmt.Errorf("unknown config edit operation %q", request.Operation)
		}
	}
	scope, err := parseEditScope(request.Scope)
	if err != nil {
		return err
	}
	switch request.Operation {
	case EditAdd:
		return AppendScopeList(raw, scope, request.Target, request.Key, request.Value)
	case EditRemove:
		return RemoveScopeList(raw, scope, request.Target, request.Key, request.Value)
	case EditReset:
		if IsScopeListKey(scope, request.Key) {
			return ResetScopeList(raw, scope, request.Target, request.Key)
		}
		return ResetScopeField(raw, scope, request.Target, request.Key)
	case EditSet:
		return SetScopeField(raw, scope, request.Target, request.Key, request.Value)
	default:
		return fmt.Errorf("unknown config edit operation %q", request.Operation)
	}
}

func editValue(raw Config, request EditRequest) (string, error) {
	effective := Merge(Defaults(), raw)
	if err := NormalizePaths(&effective); err != nil {
		return "", err
	}
	if request.Scope == "global" {
		for _, field := range append(Fields(effective), Lists(effective)...) {
			if field.Key == request.Key {
				return field.Value, nil
			}
		}
		return "", fmt.Errorf("unknown config key %q", request.Key)
	}
	if isV2EditRequest(raw, request) {
		for _, field := range V2Fields(effective, raw, request.Scope, request.Target, request.Repository) {
			if field.Key == request.Key {
				return field.Value + " (source: " + field.Source + ")", nil
			}
		}
		return "", fmt.Errorf("unknown %s config key %q", request.Scope, request.Key)
	}
	scope, err := parseEditScope(request.Scope)
	if err != nil {
		return "", err
	}
	target, err := canonicalPath(request.Target)
	if err != nil {
		return "", err
	}
	for _, field := range ScopeFields(effective, scope, target) {
		if field.Key == request.Key {
			return field.Value + " (source: " + field.Source + ")", nil
		}
	}
	return "", unknownScopeKey(scope, request.Key)
}

func isV2EditScope(scope string) bool {
	switch scope {
	case V2ScopeSystem, V2ScopeWorkspaceDefaults, V2ScopeRepositoryDefaults, V2ScopeWorkspace, V2ScopeRepository:
		return true
	default:
		return false
	}
}

func isV2EditRequest(raw Config, request EditRequest) bool {
	if request.V2 {
		return true
	}
	if request.Scope == V2ScopeSystem || request.Scope == V2ScopeWorkspaceDefaults || request.Scope == V2ScopeRepositoryDefaults {
		return true
	}
	return raw.V2() && isV2EditScope(request.Scope)
}

func isV2ListKey(scope, key string) bool {
	entry := reflect.Value{}
	switch scope {
	case V2ScopeSystem:
		entry = reflect.ValueOf(SystemConfig{})
	case V2ScopeWorkspaceDefaults:
		entry = reflect.ValueOf(WorkspaceDefaults{})
	case V2ScopeRepositoryDefaults:
		entry = reflect.ValueOf(RepositoryDefaults{})
	case V2ScopeWorkspace:
		entry = reflect.ValueOf(Workspace{})
	case V2ScopeRepository:
		entry = reflect.ValueOf(Repository{})
	}
	if scope == V2ScopeWorkspace && strings.HasPrefix(key, "repository_defaults.") {
		entry = reflect.ValueOf(RepositoryDefaults{})
		key = strings.TrimPrefix(key, "repository_defaults.")
	}
	field := v2Field(entry, key)
	return field.IsValid() && field.Kind() == reflect.Slice
}

func parseEditScope(value string) (Scope, error) {
	switch value {
	case ScopeWorkspace.String():
		return ScopeWorkspace, nil
	case ScopeRepository.String():
		return ScopeRepository, nil
	default:
		return ScopeWorkspace, fmt.Errorf("unknown config scope %q", value)
	}
}

func configDigest() ([sha256.Size]byte, error) {
	path, err := Path()
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return sha256.Sum256(nil), nil
	}
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(data), nil
}
