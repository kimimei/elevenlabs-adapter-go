// Package voices 模型目录：标准引擎（默认接入项）+ 可选补充包（音色/场景封装）。
//
// 与 Python 版的差异只有一处：JSON 用 //go:embed 打进二进制，不再依赖工作目录下的文件。
// 外部覆盖仍走 ELEVENLABS_VOICES_FILE，语义与 Python 版一致。
//
// 关键语义 —— 软门禁：包没启用时只是「不在 /v1/models 里列出」，别名仍可调用。
// 这是刻意的：已注册的渠道不会因为收窄默认接入面而中断。
package voices

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

//go:embed voices.json
var builtinJSON []byte

// 旧格式文件没有 engines 段时使用。
var defaultEngines = []string{
	"eleven_v4",
	"eleven_v3",
	"eleven_multilingual_v2",
	"eleven_turbo_v2_5",
	"eleven_flash_v2_5",
}

// ModelSpec 是 packs 里的一条模型别名定义。
type ModelSpec struct {
	VoiceID       string         `json:"voice_id"`
	Model         string         `json:"model"`
	Lang          string         `json:"lang"`
	Label         string         `json:"label"`
	VoiceSettings map[string]any `json:"voice_settings"`
}

// Pack 是一个补充包。
type Pack struct {
	ID               string
	Label            string
	EnabledByDefault bool
	ModelOrder       []string
	Models           map[string]ModelSpec
}

// Catalog 是解析后的模型目录。
type Catalog struct {
	Voices     map[string]string // 音色名 -> voice_id
	VoiceOrder []string          // 音色出现顺序（/v1/voices 要保持稳定）
	EngineIDs  []string
	Packs      map[string]*Pack
	PackOrder  []string
	Aliases    map[string]ModelSpec // 带 voice_id 的别名（模型名即音色）
	AliasOrder []string             // 别名出现顺序
	ModelAlias map[string]string    // 不带 voice_id 的兼容名 -> 引擎名
}

type fileShape struct {
	Engines    json.RawMessage      `json:"engines"`
	Packs      json.RawMessage      `json:"packs"`
	Voices     map[string]string    `json:"voices"`
	ModelAlias map[string]string    `json:"model_alias"`
	Aliases    map[string]ModelSpec `json:"aliases"`
}

type packShape struct {
	Label            string          `json:"label"`
	EnabledByDefault bool            `json:"enabled_by_default"`
	Models           json.RawMessage `json:"models"`
}

// Load 读取模型目录。
//
// 顺序与 Python 版一致：ELEVENLABS_VOICES_FILE 指向的文件存在则用它，
// 否则用内置的 voices.json。文件解析失败返回错误 —— 这条不能静默兜底，
// 否则服务会用一套空目录上线，所有请求都报「无法确定 voice_id」。
func Load(path string) (*Catalog, error) {
	if path == "" {
		path = os.Getenv("ELEVENLABS_VOICES_FILE")
	}
	data := builtinJSON
	if path != "" {
		if _, err := os.Stat(path); err == nil {
			b, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("读取 ELEVENLABS_VOICES_FILE 失败: %w", err)
			}
			data = b
		}
	}
	return Parse(data)
}

// Parse 解析模型目录 JSON。
func Parse(data []byte) (*Catalog, error) {
	var shape fileShape
	if err := json.Unmarshal(data, &shape); err != nil {
		return nil, fmt.Errorf("voices.json 解析失败: %w", err)
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("voices.json 解析失败: %w", err)
	}
	voiceOrder, _, err := decodeOrdered(top["voices"])
	if err != nil {
		return nil, fmt.Errorf("voices.json voices 段解析失败: %w", err)
	}

	c := &Catalog{
		Voices:     map[string]string{},
		VoiceOrder: voiceOrder,
		Packs:      map[string]*Pack{},
		Aliases:    map[string]ModelSpec{},
		ModelAlias: map[string]string{},
	}
	for k, v := range shape.Voices {
		c.Voices[k] = v
	}

	hasNewFormat := len(shape.Engines) > 0 || len(shape.Packs) > 0
	if hasNewFormat {
		engineOrder, _, err := decodeOrdered(shape.Engines)
		if err != nil {
			return nil, fmt.Errorf("voices.json engines 段解析失败: %w", err)
		}
		c.EngineIDs = engineOrder
		if len(c.EngineIDs) == 0 {
			c.EngineIDs = append([]string(nil), defaultEngines...)
		}

		packOrder, packRaw, err := decodeOrdered(shape.Packs)
		if err != nil {
			return nil, fmt.Errorf("voices.json packs 段解析失败: %w", err)
		}
		for _, pid := range packOrder {
			var ps packShape
			if err := json.Unmarshal(packRaw[pid], &ps); err != nil {
				return nil, fmt.Errorf("voices.json packs[%s] 解析失败: %w", pid, err)
			}
			modelOrder, modelRaw, err := decodeOrdered(ps.Models)
			if err != nil {
				return nil, fmt.Errorf("voices.json packs[%s].models 解析失败: %w", pid, err)
			}
			pack := &Pack{
				ID:               pid,
				Label:            ps.Label,
				EnabledByDefault: ps.EnabledByDefault,
				ModelOrder:       modelOrder,
				Models:           map[string]ModelSpec{},
			}
			if pack.Label == "" {
				pack.Label = pid
			}
			for _, name := range modelOrder {
				var spec ModelSpec
				if err := json.Unmarshal(modelRaw[name], &spec); err != nil {
					return nil, fmt.Errorf("voices.json packs[%s].models[%s] 解析失败: %w", pid, name, err)
				}
				pack.Models[name] = spec
			}
			c.Packs[pid] = pack
			c.PackOrder = append(c.PackOrder, pid)
		}
	} else {
		// 旧格式：aliases / model_alias 整体折叠成一个默认启用的 legacy 包
		c.EngineIDs = append([]string(nil), defaultEngines...)
		pack := &Pack{
			ID:               "legacy",
			Label:            "旧格式自定义音色表",
			EnabledByDefault: true,
			Models:           map[string]ModelSpec{},
		}
		for name, engine := range shape.ModelAlias {
			pack.Models[name] = ModelSpec{Model: engine}
			pack.ModelOrder = append(pack.ModelOrder, name)
		}
		for name, spec := range shape.Aliases {
			pack.Models[name] = spec
			pack.ModelOrder = append(pack.ModelOrder, name)
		}
		c.Packs["legacy"] = pack
		c.PackOrder = []string{"legacy"}
	}

	// 派生视图：包含全部包，不受启用状态影响（解析用）
	for _, pid := range c.PackOrder {
		pack := c.Packs[pid]
		for _, name := range pack.ModelOrder {
			spec := pack.Models[name]
			if spec.VoiceID != "" {
				c.Aliases[name] = spec
				c.AliasOrder = append(c.AliasOrder, name)
			} else if spec.Model != "" {
				c.ModelAlias[name] = spec.Model
			}
		}
	}
	return c, nil
}

// ParsePacks 解析 ADAPTER_PRESETS / ?presets= 的取值。
//
//	空 / "none" -> 只启用 enabled_by_default 的包（默认：没有）
//	"all"       -> 全部
//	"cn,tw"     -> 指定包
//
// 返回 (启用的包 id, 无法识别的包 id)。
func (c *Catalog) ParsePacks(spec string) (map[string]bool, []string) {
	enabled := map[string]bool{}
	unknown := []string{}
	seenUnknown := map[string]bool{}

	s := strings.ToLower(strings.TrimSpace(spec))
	if s == "" || s == "none" {
		for _, pid := range c.PackOrder {
			if c.Packs[pid].EnabledByDefault {
				enabled[pid] = true
			}
		}
		return enabled, unknown
	}
	if s == "all" {
		for _, pid := range c.PackOrder {
			enabled[pid] = true
		}
		return enabled, unknown
	}
	for _, part := range strings.Split(s, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" || part == "none" {
			continue
		}
		if _, ok := c.Packs[part]; ok {
			enabled[part] = true
		} else if !seenUnknown[part] {
			seenUnknown[part] = true
			unknown = append(unknown, part)
		}
	}
	sortStrings(unknown)
	return enabled, unknown
}

// ModelsFor 模型列表：标准引擎 + 已启用包内的别名（去重、保序）。
func (c *Catalog) ModelsFor(spec string) []string {
	enabled, _ := c.ParsePacks(spec)
	ids := make([]string, 0, len(c.EngineIDs)+8)
	seen := map[string]bool{}
	for _, e := range c.EngineIDs {
		if !seen[e] {
			seen[e] = true
			ids = append(ids, e)
		}
	}
	for _, pid := range c.PackOrder {
		if !enabled[pid] {
			continue
		}
		for _, name := range c.Packs[pid].ModelOrder {
			if !seen[name] {
				seen[name] = true
				ids = append(ids, name)
			}
		}
	}
	return ids
}

// AliasPack 返回包含该别名的包 id。
func (c *Catalog) AliasPack(name string) string {
	for _, pid := range c.PackOrder {
		if _, ok := c.Packs[pid].Models[name]; ok {
			return pid
		}
	}
	return ""
}

// VoiceToID 音色名 -> voice_id；若本身是合法 voice_id（长串无空格）则原样返回。
func (c *Catalog) VoiceToID(name string) string {
	if vid, ok := c.Voices[name]; ok && vid != "" {
		return vid
	}
	if utf8.RuneCountInString(name) >= 15 && !strings.Contains(name, " ") {
		return name
	}
	return ""
}

// PackIDs 返回包 id 顺序，供 /v1/presets 输出。
func (c *Catalog) PackIDs() []string {
	return append([]string(nil), c.PackOrder...)
}

// decodeOrdered 按 JSON 中的原始出现顺序解析 object。
//
// 为什么不用 map：Go 的 map 不保序，而 /v1/models 的返回顺序会被渠道注册
// 一次性抄进面板。顺序变了，diff 里全是噪声，也没法做「与 Python 版逐项相等」的验证。
func decodeOrdered(raw json.RawMessage) ([]string, map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, out, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, nil, fmt.Errorf("期望 JSON object")
	}
	var order []string
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		key, _ := kt.(string)
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, nil, err
		}
		if _, dup := out[key]; !dup {
			order = append(order, key)
		}
		out[key] = val
	}
	if _, err := dec.Token(); err != nil { // 收尾 }
		return nil, nil, err
	}
	return order, out, nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
