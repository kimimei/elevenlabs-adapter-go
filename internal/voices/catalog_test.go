package voices

import (
	"reflect"
	"strings"
	"testing"
)

// lin 的 Voice Library 副本于 2026-09-09 被作者关闭共享（sharing.status=copied_disabled），
// 真合成返 403 voice_access_denied；重新添加、原始样本自建克隆两条恢复路径实测均失败。
// 2026-10-26 起 lin 映射到 xinghe，声音已变更。
//
// 这个映射在 voices.json 里横跨三处（voices.lin、packs.cn 的 zh-duihua 与 zh-对话），
// 漏改任何一处，那条通路就会 403，而报错文案是「音色被作者停用」——
// 看起来像上游故障或 Key 权限问题，很难联想到「改漏了一行」。
// 所以这里把三处钉在一个用例里，谁改了其中一处都会被立刻拦住。
func TestLinMappedToXinghe(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	const xinghe = "lt7GBaCoAHWbT7JSZ5Xs"

	if got := c.VoiceToID("lin"); got != xinghe {
		t.Errorf("lin 应映射到 xinghe %q，实际 %q", xinghe, got)
	}
	if got := c.VoiceToID("xinghe"); got != xinghe {
		t.Errorf("xinghe -> %q", got)
	}
	// 别名走 model 字段，不在 Voices 映射里，必须单独查
	for _, alias := range []string{"zh-duihua", "zh-对话"} {
		spec, ok := c.Aliases[alias]
		if !ok {
			t.Errorf("别名 %q 不存在", alias)
			continue
		}
		if spec.VoiceID != xinghe {
			t.Errorf("别名 %q 的 voice_id 应为 %q，实际 %q", alias, xinghe, spec.VoiceID)
		}
	}

	// 弃用段是说明性数据，不能被当成音色条目读进 Voices
	if _, ok := c.Voices["_deprecated_20261026"]; ok {
		t.Error("_deprecated_20261026 被误解析成音色条目")
	}
}

func TestParsePacks(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	t.Run("默认只启用标记了 enabled_by_default 的包", func(t *testing.T) {
		enabled, unknown := c.ParsePacks("none")
		if len(unknown) != 0 {
			t.Fatalf("unknown = %v", unknown)
		}
		for pid, p := range c.Packs {
			if p.EnabledByDefault != enabled[pid] {
				t.Fatalf("包 %s 启用状态 = %v，期望 %v", pid, enabled[pid], p.EnabledByDefault)
			}
		}
	})

	t.Run("all 全部启用", func(t *testing.T) {
		enabled, _ := c.ParsePacks("all")
		if len(enabled) != len(c.Packs) {
			t.Fatalf("启用 %d 个包，共 %d 个", len(enabled), len(c.Packs))
		}
	})

	t.Run("指定包与未知包", func(t *testing.T) {
		enabled, unknown := c.ParsePacks("cn,NOPE")
		if !enabled["cn"] {
			t.Fatal("cn 未启用")
		}
		if !reflect.DeepEqual(unknown, []string{"nope"}) {
			t.Fatalf("unknown = %v，期望 [nope]（小写且排序）", unknown)
		}
	})
}

// 软门禁：包没启用时只是「不在列表里」，别名仍然可以调用。
// 这是刻意的 —— 已注册的渠道不会因为收窄默认接入面而中断。
func TestSoftGate(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if _, ok := c.Aliases["zh-boyin"]; !ok {
		t.Fatal("zh-boyin 应在 aliases 里（解析始终包含全部包）")
	}
	models := c.ModelsFor("none")
	for _, m := range models {
		if m == "zh-boyin" {
			t.Fatal("包未启用时不应出现在 /v1/models 列表里")
		}
	}
}

func TestModelsForKeepsOrderAndDedupes(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	got := c.ModelsFor("none")
	if !reflect.DeepEqual(got, c.EngineIDs) {
		t.Fatalf("默认列表 = %v，期望等于引擎列表 %v", got, c.EngineIDs)
	}

	all := c.ModelsFor("all")
	seen := map[string]bool{}
	for _, m := range all {
		if seen[m] {
			t.Fatalf("模型 %s 重复出现", m)
		}
		seen[m] = true
	}
	if len(all) <= len(got) {
		t.Fatalf("all 应比默认多，got %d vs %d", len(all), len(got))
	}
}

func TestVoiceToID(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if got := c.VoiceToID("rachel"); got != "21m00Tcm4TlvDq8ikWAM" {
		t.Fatalf("rachel -> %q", got)
	}
	// 本身是合法 voice_id（长串无空格）则原样返回
	if got := c.VoiceToID("21m00Tcm4TlvDq8ikWAM"); got != "21m00Tcm4TlvDq8ikWAM" {
		t.Fatalf("长 id 应为原样返回，got %q", got)
	}
	if got := c.VoiceToID("nope"); got != "" {
		t.Fatalf("未知短名应为空，got %q", got)
	}
}

// 旧格式（顶层 aliases / model_alias）必须整体折叠成 legacy 包，行为与升级前一致。
func TestLegacyFormat(t *testing.T) {
	raw := `{
	  "voices": {"rachel": "21m00Tcm4TlvDq8ikWAM"},
	  "aliases": {"zh-boyin": {"voice_id": "21m00Tcm4TlvDq8ikWAM", "model": "eleven_v3"}},
	  "model_alias": {"tts-1": "eleven_v3"}
	}`
	c, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("解析旧格式失败: %v", err)
	}
	if len(c.PackOrder) != 1 || c.PackOrder[0] != "legacy" {
		t.Fatalf("PackOrder = %v", c.PackOrder)
	}
	if !c.Packs["legacy"].EnabledByDefault {
		t.Fatal("legacy 包应默认启用")
	}
	enabled, _ := c.ParsePacks("none")
	if !enabled["legacy"] {
		t.Fatal("默认状态下 legacy 包应已启用")
	}
	if _, ok := c.Aliases["zh-boyin"]; !ok {
		t.Fatal("aliases 未折叠进 legacy")
	}
	if c.ModelAlias["tts-1"] != "eleven_v3" {
		t.Fatalf("model_alias 未折叠: %v", c.ModelAlias)
	}
	// 旧格式没有 engines 段时用内置默认引擎
	if len(c.EngineIDs) != len(defaultEngines) {
		t.Fatalf("EngineIDs = %v", c.EngineIDs)
	}
}

func TestParseRejectsBrokenJSON(t *testing.T) {
	if _, err := Parse([]byte(`{"packs":`)); err == nil {
		t.Fatal("坏 JSON 应报错，不能静默返回空目录")
	} else if !strings.Contains(err.Error(), "解析失败") {
		t.Fatalf("错误信息不够具体: %v", err)
	}
}

// 内置 voices.json 必须能过 parse —— 它被打进二进制，坏了就没有任何补救路径。
func TestEmbeddedCatalogIsValid(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("内置 voices.json 解析失败: %v", err)
	}
	if len(c.EngineIDs) == 0 {
		t.Fatal("没有标准引擎")
	}
	if len(c.Aliases) == 0 {
		t.Fatal("没有音色别名")
	}
}
