package httpapi

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// packSpec 查询参数优先于环境变量，便于接入时不重启预览。
func (a *App) packSpec(c *gin.Context) string {
	if v, ok := c.GetQuery("presets"); ok {
		return v
	}
	return a.Cfg.Presets
}

// unknownPresetsMsg 构造与 Python 版一致的报错文案。
func (a *App) unknownPresetsMsg(unknown []string) string {
	return "未知的 presets: " + strings.Join(unknown, ",") +
		"；可用值 all / none / " + strings.Join(a.Catalog.PackIDs(), ",")
}

// handleModels 默认只返回标准引擎；补充包需 ADAPTER_PRESETS 开启或本次请求指定。
// 例：/v1/models?presets=all 可预览全部（不改变服务端默认行为）。
func (a *App) handleModels(c *gin.Context) {
	spec := a.packSpec(c)
	if _, unknown := a.Catalog.ParsePacks(spec); len(unknown) > 0 {
		writeErr(c, http.StatusBadRequest, a.unknownPresetsMsg(unknown), "invalid_request_error", "", nil)
		return
	}
	ids := a.Catalog.ModelsFor(spec)
	data := make([]gin.H, 0, len(ids))
	for _, id := range ids {
		data = append(data, gin.H{"id": id, "object": "model", "created": 0, "owned_by": "elevenlabs"})
	}
	c.PureJSON(http.StatusOK, gin.H{"object": "list", "data": data})
}

// handlePresets 补充包清单：接入方据此决定要不要把某个包加进渠道模型列表。
func (a *App) handlePresets(c *gin.Context) {
	spec := a.packSpec(c)
	enabled, unknown := a.Catalog.ParsePacks(spec)
	if len(unknown) > 0 {
		writeErr(c, http.StatusBadRequest, a.unknownPresetsMsg(unknown), "invalid_request_error", "", nil)
		return
	}
	data := make([]gin.H, 0, len(a.Catalog.PackIDs()))
	for _, pid := range a.Catalog.PackIDs() {
		p := a.Catalog.Packs[pid]
		data = append(data, gin.H{
			"id":      pid,
			"label":   p.Label,
			"enabled": enabled[pid],
			"models":  p.ModelOrder,
		})
	}
	c.PureJSON(http.StatusOK, gin.H{
		"object":  "list",
		"engines": a.Catalog.EngineIDs,
		"data":    data,
	})
}

// handleVoices 音色目录。
//
// with_preview=1 时附带 preview_url（上游 CDN 试听链接，缓存 1 小时）。
// lang=zh 只返回声明了该语言的别名（基础音色无语言声明，会被过滤掉）。
func (a *App) handleVoices(c *gin.Context) {
	enabled, _ := a.Catalog.ParsePacks(a.packSpec(c))
	lang := c.Query("lang")
	withPreview := c.Query("with_preview")

	previews := map[string]string{}
	if withPreview != "" && withPreview != "0" && withPreview != "false" {
		key := a.resolveAPIKey(c.GetHeader("Authorization"), c.GetHeader("X-Api-Key"), c.GetHeader("Api-Key"))
		previews = a.Upstream.FetchPreviews(c.Request.Context(), key, a.Previews)
	}

	data := make([]gin.H, 0, len(a.Catalog.Voices)+8)
	if lang == "" {
		for _, name := range a.Catalog.VoiceOrder {
			vid := a.Catalog.Voices[name]
			data = append(data, gin.H{
				"voice_id":    vid,
				"name":        name,
				"kind":        "voice",
				"preview_url": nilIfEmpty(previews[vid]),
			})
		}
	}
	for _, name := range a.Catalog.AliasOrder {
		spec := a.Catalog.Aliases[name]
		pack := a.Catalog.AliasPack(name)
		if !enabled[pack] {
			continue
		}
		if lang != "" && !strings.HasPrefix(strings.ToLower(spec.Lang), strings.ToLower(lang)) {
			continue
		}
		data = append(data, gin.H{
			"voice_id":    spec.VoiceID,
			"name":        name,
			"kind":        "alias",
			"model":       nilIfEmpty(spec.Model),
			"pack":        pack,
			"lang":        nilIfEmpty(spec.Lang),
			"label":       nilIfEmpty(spec.Label),
			"preview_url": nilIfEmpty(previews[spec.VoiceID]),
		})
	}

	c.PureJSON(http.StatusOK, gin.H{
		"voices":  data,
		"count":   len(data),
		"preview": withPreview != "" && withPreview != "0" && withPreview != "false",
	})
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
