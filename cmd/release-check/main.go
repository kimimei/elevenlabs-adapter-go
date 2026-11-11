// release-check 是发布前门禁的命令行入口。
//
// 用法:
//
//	go run ./cmd/release-check              # 人类可读报告
//	go run ./cmd/release-check -json        # 机器可读
//	go run ./cmd/release-check -self-test   # 只跑规则反证自测
//
// 退出码: 0 = 可以发布；1 = 存在阻塞项。
// 提醒项（WARN）不影响退出码 —— 否则「有个 TODO」就会挡住发布，
// 挡久了大家会习惯性加 -skip，门禁就废了。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/kimimei/elevenlabs-adapter-go/internal/releasecheck"
)

func main() {
	var (
		root     = flag.String("root", ".", "仓库根目录")
		asJSON   = flag.Bool("json", false, "输出 JSON")
		selfTest = flag.Bool("self-test", false, "只跑规则反证自测")
	)
	flag.Parse()

	if *selfTest {
		os.Exit(releasecheck.SelfTest())
	}

	r, err := releasecheck.Run(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "门禁无法执行：", err)
		os.Exit(1)
	}

	if *asJSON {
		b, _ := json.MarshalIndent(map[string]any{
			"ok":    r.OK(),
			"items": r.Items,
		}, "", "  ")
		fmt.Println(string(b))
		if !r.OK() {
			os.Exit(1)
		}
		return
	}

	if len(r.Items) == 0 {
		fmt.Println("全部通过，无发现。")
		return
	}
	for _, i := range r.Items {
		mark := "BLOCK"
		if i.Level == releasecheck.Warn {
			mark = "WARN "
		}
		fmt.Printf("[%s] %-18s %-34s %s\n", mark, i.Rule, i.Where, i.Detail)
	}
	fmt.Printf("\n阻塞 %d 项，提醒 %d 项。\n", len(r.Blocked()), len(r.Warns()))
	if !r.OK() {
		fmt.Println("→ 存在阻塞项，暂不适合公开。")
		os.Exit(1)
	}
	fmt.Println("→ 无阻塞项；提醒项可在发布后跟进。")
}
