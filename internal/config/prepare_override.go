package config

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// PrepareOverrideCopyMode と PrepareOverrideCOWMinSizeKiB は上書きできる設定の key で、
// storage の同名設定と同じ値を取る。
const (
	PrepareOverrideCopyMode      = "copy_mode"
	PrepareOverrideCOWMinSizeKiB = "cow_min_size_kib"
)

// PrepareOverride は貸出1回に付随する準備設定の上書きである。
// 設定ファイルにも daemon の実効設定にも触れず、その貸出で準備する slot の計算だけへ適用する。
// COWMinSizeKiB は 0 が「下限なし」を意味するため、未指定と区別できるよう pointer で持つ。
type PrepareOverride struct {
	CopyMode      string `json:"copy_mode,omitempty"`
	COWMinSizeKiB *int   `json:"cow_min_size_kib,omitempty"`
}

// IsZero は上書きが何も指定されていないかを返す。
func (o PrepareOverride) IsZero() bool {
	return o.CopyMode == "" && o.COWMinSizeKiB == nil
}

// Validate は Config.Validate の storage 節と同じ条件で上書きの値を検査する。
func (o PrepareOverride) Validate() error {
	switch o.CopyMode {
	case "", CopyModeAuto, CopyModeCOW, CopyModeCopy:
	default:
		return fmt.Errorf("%s must be %s, %s, or %s", PrepareOverrideCopyMode, CopyModeAuto, CopyModeCOW, CopyModeCopy)
	}
	if o.COWMinSizeKiB != nil && (*o.COWMinSizeKiB < 0 || *o.COWMinSizeKiB > MaxCOWMinSizeKiB) {
		return fmt.Errorf("%s must be between 0 and %d", PrepareOverrideCOWMinSizeKiB, MaxCOWMinSizeKiB)
	}
	return nil
}

// Apply は上書きを反映した設定を返す。受け取った設定は変更しない。
func (o PrepareOverride) Apply(c Config) Config {
	if o.CopyMode != "" {
		c.Storage.CopyMode = o.CopyMode
	}
	if o.COWMinSizeKiB != nil {
		c.Storage.COWMinSizeKiB = *o.COWMinSizeKiB
	}
	return c
}

// String は指定した key だけを `key=value` で並べた表記を返す。上書きが無い場合は空文字を返す。
// 同じ上書きが同じ文字列になるよう key の順で並べ、表の行と slot の記録で同じ表記を使う。
func (o PrepareOverride) String() string {
	var parts []string
	if o.CopyMode != "" {
		parts = append(parts, PrepareOverrideCopyMode+"="+o.CopyMode)
	}
	if o.COWMinSizeKiB != nil {
		parts = append(parts, PrepareOverrideCOWMinSizeKiB+"="+strconv.Itoa(*o.COWMinSizeKiB))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// Encode は slot 行へ記録する JSON を返す。上書きが無い場合は空文字を返し、列を空のままにする。
func (o PrepareOverride) Encode() (string, error) {
	if o.IsZero() {
		return "", nil
	}
	data, err := json.Marshal(o)
	if err != nil {
		return "", fmt.Errorf("encode prepare override: %w", err)
	}
	return string(data), nil
}

// DecodePrepareOverride は Encode の逆で、空文字は上書き無しとして扱う。
func DecodePrepareOverride(raw string) (PrepareOverride, error) {
	if strings.TrimSpace(raw) == "" {
		return PrepareOverride{}, nil
	}
	var o PrepareOverride
	if err := json.Unmarshal([]byte(raw), &o); err != nil {
		return PrepareOverride{}, fmt.Errorf("decode prepare override %q: %w", raw, err)
	}
	return o, o.Validate()
}

// ParsePrepareOverride は `copy_mode=copy,cow_min_size_kib=64` 形式の指定を解釈する。
// 指定しなかった key は上書きせず、要求を受けた側の実効設定のままになる。
func ParsePrepareOverride(spec string) (PrepareOverride, error) {
	var o PrepareOverride
	if strings.TrimSpace(spec) == "" {
		return PrepareOverride{}, fmt.Errorf("empty configuration; specify %s and/or %s", PrepareOverrideCopyMode, PrepareOverrideCOWMinSizeKiB)
	}
	for _, field := range strings.Split(spec, ",") {
		key, value, found := strings.Cut(field, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !found || key == "" || value == "" {
			return PrepareOverride{}, fmt.Errorf("configuration field %q must be <key>=<value>", field)
		}
		if err := o.setField(key, value); err != nil {
			return PrepareOverride{}, err
		}
	}
	return o, o.Validate()
}

// setField は1つの key を上書きへ載せる。同じ key の二重指定は、どちらが効くかを黙って決めないため拒否する。
func (o *PrepareOverride) setField(key, value string) error {
	switch key {
	case PrepareOverrideCopyMode:
		if o.CopyMode != "" {
			return fmt.Errorf("configuration key %s is set twice", key)
		}
		o.CopyMode = value
	case PrepareOverrideCOWMinSizeKiB:
		if o.COWMinSizeKiB != nil {
			return fmt.Errorf("configuration key %s is set twice", key)
		}
		kib, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("configuration key %s must be an integer: %w", key, err)
		}
		o.COWMinSizeKiB = &kib
	default:
		return fmt.Errorf("unknown configuration key %q; use %s or %s", key, PrepareOverrideCopyMode, PrepareOverrideCOWMinSizeKiB)
	}
	return nil
}
