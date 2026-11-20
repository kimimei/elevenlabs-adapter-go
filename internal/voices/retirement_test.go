package voices

import "testing"

// retired20261231 是 ElevenLabs Default 音色（2026-12-31 全部失效）的 voice_id。
//
// 官方公告：All our Default voices will expire on December 31, 2026.
// 替换音色（Darian/Talia/Eldrin/Alicia…）实测可直接 API 调用，无需先加入账号。
//
// 为什么要有这条测试：voices.json 是可编辑文件，任何人都可能"顺手补一个音色"时
// 把旧 ID 加回来。这种回归在开发期完全看不出来，只在 2027-01-01 之后表现为整条链路 400。
var retired20261231 = map[string]string{
	// 经典 Default（官方替换表未逐一列出，但同属 Default 集）
	"21m00Tcm4TlvDq8ikWAM": "Rachel",
	"pNInz6obpgDQGcFmaJgB": "Adam",
	"ErXwobaYiN019PkySvjV": "Antoni",
	"AZnzlk1XvdvUeBnXmlld": "Domi",
	"MF3mGyEYCl7XYWbV9V6O": "Elli",
	"TxGEqnHWrfWFTfGW9XjX": "Josh",
	"VR6AewLTigWG4xSOukaG": "Arnold",
	"yoZ06aMxZJJ28mfd3POQ": "Sam",
	// 官方替换表列出的一代 Default
	"EXAVITQu4vr4xnSDxMaL": "Sarah",
	"FGY2WhTYpPnrIDTdsKH5": "Laura",
	"JBFqnCBsd6RMkjVDRZzb": "George",
	"CwhRBWXzGAHq8TQ4Fs17": "Roger",
	"IKne3meq5aSn9XLyUdCD": "Charlie",
	"N2lVS1w4EtoT3dr4eOWO": "Callum",
	"SAz9YHcvj6GT2YYXdXww": "River",
	"SOYHLrjzK2X1ezoPC6cr": "Harry",
	"TX3LPaxmHKxFdv7VOQHJ": "Liam",
	"Xb7hH8MSUJpSbSDYk0k2": "Alice",
	"XrExE9yKIg1WjnnlVkGX": "Matilda",
	"nPczCjzI2devNBz1zQrb": "Brian",
	"pqHfZKP75CvOlQylNhV4": "Bill",
	"onwK4e9ZLuTAKqWW03F9": "Daniel",
	"pFZP5JQG7iQjIQuC4Bku": "Lily",
	"cgSgspJ2msm6clMCkdW9": "Jessica",
	"cjVigY5qzO86Huf0OWal": "Eric",
	"iP95p4xoKVk53GoZ742B": "Chris",
	"bIHbv24MWmeRgasZH58o": "Will",
}

// TestNoRetiredVoiceID 任何可调用位置都不得引用 2026-12-31 失效的音色。
func TestNoRetiredVoiceID(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("加载音色目录失败: %v", err)
	}

	for name, vid := range c.Voices {
		if old, bad := retired20261231[vid]; bad {
			t.Errorf("voices.%s 指向已退役音色 %s（%s），2027-01-01 起会 400", name, old, vid)
		}
	}
	for pid, p := range c.Packs {
		for alias, spec := range p.Models {
			if old, bad := retired20261231[spec.VoiceID]; bad {
				t.Errorf("packs.%s.%s 指向已退役音色 %s（%s）", pid, alias, old, spec.VoiceID)
			}
		}
	}
}

// TestCompatAliasesUsable OpenAI 兼容别名必须绑好音色，否则标准客户端开箱即挂。
func TestCompatAliasesUsable(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("加载音色目录失败: %v", err)
	}
	compat, ok := c.Packs["compat"]
	if !ok {
		t.Fatal("目录里缺少 compat 包")
	}
	for _, alias := range []string{"alloy", "echo", "fable", "onyx", "nova", "shimmer"} {
		spec, ok := compat.Models[alias]
		if !ok {
			t.Errorf("compat 包缺少 %s", alias)
			continue
		}
		if spec.VoiceID == "" {
			t.Errorf("compat.%s 没有绑定 voice_id", alias)
			continue
		}
		if old, bad := retired20261231[spec.VoiceID]; bad {
			t.Errorf("compat.%s 指向已退役音色 %s", alias, old)
		}
	}
}
