package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/kimimei/elevenlabs-adapter-go/internal/buildinfo"
)

// /healthz 必须能回答「线上跑的是哪一版」。
//
// 这不是洁癖：Python 版曾出现「代码已是 0.5.0、进程自报 0.4.0」的情况，
// 当时为了确认线上到底是哪版，只能去比对文件 md5。
// Go 版没有 /openapi.json，如果 healthz 也不报版本，同样的疑问会再出现一次，
// 而且这次连一个能查的地方都没有。
func TestHealthzReportsVersion(t *testing.T) {
	h := newHarness(t, nil, nil)
	rec := doReq(h, http.MethodGet, "/healthz", "", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("healthz 期望 200，得到 %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析 healthz 失败: %v（body=%s）", err, rec.Body.String())
	}
	if out["status"] != "ok" {
		t.Errorf("status 期望 ok，得到 %v", out["status"])
	}
	v, _ := out["version"].(string)
	if v != buildinfo.Version {
		t.Fatalf("version 期望 %q，得到 %q", buildinfo.Version, v)
	}
	if v == "" {
		t.Fatal("version 不能为空 —— 空版本等于没版本")
	}
}
