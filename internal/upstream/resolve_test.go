package upstream

import (
	"net/http"
	"strings"
	"testing"

	"github.com/kimimei/elevenlabs-adapter-go/internal/voices"
)

func TestClampSpeed(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		name    string
		in      *float64
		want    *float64
		clamped bool
	}{
		{"未传不钳制", nil, nil, false},
		{"低于下限钳到 0.7", f(0.25), f(0.7), true},
		{"高于上限钳到 1.2", f(4.0), f(1.2), true},
		{"区间内原样", f(1.0), f(1.0), false},
		{"边界 0.7 不钳制", f(0.7), f(0.7), false},
		{"边界 1.2 不钳制", f(1.2), f(1.2), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, clamped := ClampSpeed(tc.in)
			if clamped != tc.clamped {
				t.Fatalf("clamped = %v, want %v", clamped, tc.clamped)
			}
			if tc.want == nil {
				if got != nil {
					t.Fatalf("got %v, want nil", *got)
				}
				return
			}
			if got == nil || *got != *tc.want {
				t.Fatalf("got %v, want %v", got, *tc.want)
			}
		})
	}
}

func mustCatalog(t *testing.T) *voices.Catalog {
	t.Helper()
	c, err := voices.Load("")
	if err != nil {
		t.Fatalf("加载 voices.json 失败: %v", err)
	}
	return c
}

func TestResolveVoiceAndModel(t *testing.T) {
	c := mustCatalog(t)

	t.Run("音色名解析成 voice_id", func(t *testing.T) {
		id, model, _, err := ResolveVoiceAndModel("eleven_multilingual_v2", "rachel", c, "eleven_multilingual_v2")
		if err != nil {
			t.Fatal(err)
		}
		if id != "21m00Tcm4TlvDq8ikWAM" {
			t.Fatalf("voice_id = %q", id)
		}
		if model != "eleven_multilingual_v2" {
			t.Fatalf("model = %q", model)
		}
	})

	t.Run("原始 voice_id 原样透传", func(t *testing.T) {
		id, _, _, err := ResolveVoiceAndModel("eleven_v3", "21m00Tcm4TlvDq8ikWAM", c, "eleven_multilingual_v2")
		if err != nil {
			t.Fatal(err)
		}
		if id != "21m00Tcm4TlvDq8ikWAM" {
			t.Fatalf("voice_id = %q", id)
		}
	})

	// voice_id 会被拼进上游 URL 路径，带斜杠就能把请求指到任意路径上。
	// 这类值会被当成「原始 voice_id」透传，必须在 checkVoiceID 处拦下。
	t.Run("带路径的 voice_id 被拒绝", func(t *testing.T) {
		for _, bad := range []string{
			"../v1/user/subscription",
			"abc/def/ghijklmn",
			"a.b.c.d.e.f.g.h.i",
		} {
			if _, _, _, err := ResolveVoiceAndModel("eleven_v3", bad, c, "eleven_multilingual_v2"); err == nil {
				t.Fatalf("%q 应被拒绝", bad)
			}
		}
	})

	// 带空格或太短的名字压根不会被当成 voice_id（VoiceToID 返回空），
	// 此时不报错，交给调用方按「无法确定 voice_id」回 400 —— 与 Python 版一致。
	t.Run("无法识别的名字不报错但解析为空", func(t *testing.T) {
		for _, name := range []string{"has space in it and is long enough", "short"} {
			id, _, _, err := ResolveVoiceAndModel("eleven_v3", name, c, "eleven_multilingual_v2")
			if err != nil {
				t.Fatalf("%q 不应报错: %v", name, err)
			}
			if id != "" {
				t.Fatalf("%q 应解析为空，got %q", name, id)
			}
		}
	})

	t.Run("模型别名携带 voice_id", func(t *testing.T) {
		id, model, _, err := ResolveVoiceAndModel("zh-boyin", "", c, "eleven_multilingual_v2")
		if err != nil {
			t.Fatal(err)
		}
		if id == "" {
			t.Fatal("zh-boyin 应解析出 voice_id")
		}
		if model == "" {
			t.Fatal("model 不应为空")
		}
	})

	t.Run("OpenAI 兼容名映射到引擎", func(t *testing.T) {
		_, model, _, err := ResolveVoiceAndModel("tts-1", "rachel", c, "eleven_multilingual_v2")
		if err != nil {
			t.Fatal(err)
		}
		if model == "tts-1" {
			t.Fatal("tts-1 应被映射成真实引擎名，不能直接发给上游")
		}
	})

	t.Run("引擎名直通", func(t *testing.T) {
		_, model, _, err := ResolveVoiceAndModel("eleven_turbo_v2_5", "rachel", c, "eleven_multilingual_v2")
		if err != nil {
			t.Fatal(err)
		}
		if model != "eleven_turbo_v2_5" {
			t.Fatalf("model = %q", model)
		}
	})

	t.Run("未知短名无法确定 voice_id", func(t *testing.T) {
		id, _, _, err := ResolveVoiceAndModel("eleven_v3", "nope", c, "eleven_multilingual_v2")
		if err != nil {
			t.Fatal(err)
		}
		if id != "" {
			t.Fatalf("voice_id = %q，期望空（由调用方报 400）", id)
		}
	})
}

func TestBuildPayload(t *testing.T) {
	speed := 2.0
	p := BuildPayload("你好", "eleven_v3", "mp3_44100_128", &speed, Extra{
		Lang:          "zh",
		VoiceSettings: map[string]any{"stability": 0.5},
	})
	if p["text"] != "你好" || p["model_id"] != "eleven_v3" || p["output_format"] != "mp3_44100_128" {
		t.Fatalf("基础字段不对: %#v", p)
	}
	if p["language"] != "zh" {
		t.Fatalf("language 未透传: %#v", p)
	}
	vs, ok := p["voice_settings"].(map[string]any)
	if !ok {
		t.Fatalf("voice_settings 缺失: %#v", p)
	}
	// speed 必须是被钳制后的值，不能把 2.0 原样发给上游（上游会报 400）
	if vs["speed"] != 1.2 {
		t.Fatalf("speed = %v, want 1.2", vs["speed"])
	}
	if vs["stability"] != 0.5 {
		t.Fatalf("预设 voice_settings 被吞了: %#v", vs)
	}
}

func TestExtractCharacterCost(t *testing.T) {
	h := http.Header{}
	h.Set("character-cost", "48")
	if got := ExtractCharacterCost(h); got == nil || *got != 48 {
		t.Fatalf("got %v, want 48", got)
	}
	h2 := http.Header{}
	h2.Set("character-cost", "48.0")
	if got := ExtractCharacterCost(h2); got == nil || *got != 48 {
		t.Fatalf("got %v, want 48 (float 形式也要能读)", got)
	}
	if got := ExtractCharacterCost(http.Header{}); got != nil {
		t.Fatalf("未回传时应为 nil，got %v", *got)
	}
	h3 := http.Header{}
	h3.Set("character-cost", "abc")
	if got := ExtractCharacterCost(h3); got != nil {
		t.Fatalf("非法值应为 nil，got %v", *got)
	}
}

func TestTTSURLEscapesVoiceID(t *testing.T) {
	got := TTSURL("https://api.elevenlabs.io/v1", "abc/def")
	want := "https://api.elevenlabs.io/v1/text-to-speech/abc%2Fdef"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// 未知 model 必须明确报错，绝不能静默回退到默认模型。
//
// 这条测试锁的是一个真实交付风险：原实现 `mid := defaultModel` 让任何拼错的模型名
// 都能"出声"，客户拿到的是默认引擎的音质与计费，不读响应头就永远发现不了。
// 反证方式：把 resolve.go 里的 model 分支改回 defaultModel，此测试立即变红。
func TestResolveRejectsUnknownModel(t *testing.T) {
	c := mustCatalog(t)
	def := "eleven_multilingual_v2"

	for _, bad := range []string{
		"no-such-model",
		"eleven_v33",          // 典型手误：多打一个 3
		"eleven_multilingual", // 少了 _v2
		"eleven_bogus_v9",     // 完全捏造的引擎名
	} {
		_, _, _, err := ResolveVoiceAndModel(bad, "rachel", c, def)
		if err == nil {
			t.Fatalf("模型 %q 应报错，实际被静默接受", bad)
		}
		// 错误信息必须能指导用户改对，否则等于没报错
		if !strings.Contains(err.Error(), "可用引擎") {
			t.Fatalf("模型 %q 的错误信息缺少可用引擎列表: %v", bad, err)
		}
	}

	// tts-1 等 OpenAI 兼容名是【有意注册】的别名，必须放行并映射到真实引擎。
	// 它不属于「未知模型」—— 这里单独断言，避免把兼容设计与静默回退混为一谈。
	if _, m, _, err := ResolveVoiceAndModel("tts-1", "rachel", c, def); err != nil {
		t.Fatalf("tts-1 是注册过的兼容名，不应被拒: %v", err)
	} else if m == "tts-1" {
		t.Fatalf("tts-1 应映射成真实引擎，不能原样发给上游")
	}

	// 反过来：合法引擎名必须放行，不能误伤
	for _, good := range []string{"eleven_v4", "eleven_v3", "eleven_multilingual_v2",
		"eleven_turbo_v2_5", "eleven_flash_v2_5"} {
		if _, _, _, err := ResolveVoiceAndModel(good, "rachel", c, def); err != nil {
			t.Fatalf("合法引擎 %q 被误拒: %v", good, err)
		}
	}

	// 空 model 仍走默认引擎：不少 OpenAI SDK 不传 model 字段
	_, mid, _, err := ResolveVoiceAndModel("", "rachel", c, def)
	if err != nil {
		t.Fatalf("空 model 不应报错: %v", err)
	}
	if mid != def {
		t.Fatalf("空 model 应回退默认 %q，实际 %q", def, mid)
	}
}

// elevenlabs-tts 必须映射到真实引擎，不能原样透传给上游。
//
// 这条锁的是一个真实缺陷：曾经有个分支 `if model == "elevenlabs-tts" { return model }`
// 把它当引擎名直接发给 ElevenLabs，上游返回
// "An invalid ID has been received: 'elevenlabs-tts'" → 整条兼容入口 400。
// 反证：把该直通分支加回去，此测试立即变红。
func TestElevenlabsTtsAliasResolvesToRealEngine(t *testing.T) {
	c := mustCatalog(t)
	_, model, _, err := ResolveVoiceAndModel("elevenlabs-tts", "yun", c, "eleven_v3")
	if err != nil {
		t.Fatalf("elevenlabs-tts 是注册过的兼容名，不应报错: %v", err)
	}
	if model == "elevenlabs-tts" {
		t.Fatalf("elevenlabs-tts 被原样透传给上游，上游会报 invalid ID")
	}
	if !strings.HasPrefix(model, "eleven_") {
		t.Fatalf("应映射到 eleven_* 引擎名，实际 %q", model)
	}
}
