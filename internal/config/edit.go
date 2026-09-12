package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
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
	Scope     string
	Target    string
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
