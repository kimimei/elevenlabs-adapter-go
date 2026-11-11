package releasecheck

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SelfTest 是门禁的自我反证：在临时目录里造一份「故意违规」的仓库，
// 看每条规则是否真的会报警；再造一份「干净」的，看它是否放行。
//
// 为什么必须做这一步：一个永远返回 0 的门禁和一个有效的门禁，
// 从外部看没有任何区别。只有往里面塞一个已知的坏东西、看它变红，
// 才能证明它没在空转。
//
// 返回 0 表示反证与正向验证都通过。
func SelfTest() int {
	dir, err := os.MkdirTemp("", "releasecheck-selftest-")
	if err != nil {
		fmt.Println("自测失败：无法创建临时目录：", err)
		return 1
	}
	defer os.RemoveAll(dir)

	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		fmt.Println("自测失败：", err)
		return 1
	}

	// ---- 1. 造违规样本 -----------------------------------------------------
	must(os.WriteFile(filepath.Join(repo, "LICENSE"), []byte(licTemplate), 0o644))
	must(os.WriteFile(filepath.Join(repo, "README.md"), []byte("# demo\n正常内容\n"), 0o644))
	must(os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("node_modules/\n"), 0o644))
	must(os.WriteFile(filepath.Join(repo, ".env.example"),
		[]byte("ELEVENLABS_API_KEY=live_abcdefghijklmnop\n"), 0o644))
	must(os.WriteFile(filepath.Join(repo, "leak.txt"), []byte(strings.Join([]string{
		"host=198.18.0.1",
		"token=sk-abcdefghijklmnopqrstuvwx",
		"admin=https://git.corp.internal/login",
		`path=C:\Users\someone\work\adapter`,
	}, "\n")), 0o644))
	must(os.WriteFile(filepath.Join(repo, "clip.mp3"), []byte("x"), 0o644))

	e := envFromDir(repo)
	r := &Report{}
	contentRules(e, r)

	expect := []string{
		"真实 IP 泄露",
		"内部域名泄露",
		"密钥泄露",
		"示例文件被填了值",
		"本机路径泄露",
		"LICENSE 版权主体",
		".gitignore 覆盖",
		"音频入仓",
	}
	got := map[string]bool{}
	for _, i := range r.Items {
		got[i.Rule] = true
	}
	var missing []string
	for _, want := range expect {
		if !got[want] {
			missing = append(missing, want)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		fmt.Println("反证失败：以下规则对故意违规的样本没有报警：", strings.Join(missing, "、"))
		return 1
	}

	// ---- 2. 正向验证：全部改成合规内容，必须零阻塞 -------------------------
	must(os.WriteFile(filepath.Join(repo, "LICENSE"), []byte(licFilled), 0o644))
	must(os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(gitignoreOK), 0o644))
	must(os.WriteFile(filepath.Join(repo, ".env.example"),
		[]byte("# 留空，由部署时注入\nELEVENLABS_API_KEY=\nADAPTER_PORT=8123\n"), 0o644))
	must(os.WriteFile(filepath.Join(repo, "leak.txt"), []byte(strings.Join([]string{
		"host=203.0.113.9", // RFC 5737 文档网段：永不指向真实主机
		"bridge=172.18.0.1",
		"docs=https://api.elevenlabs.io/v1/voices",
		"local=http://127.0.0.1:8123/healthz",
	}, "\n")), 0o644))
	must(os.Remove(filepath.Join(repo, "clip.mp3")))

	e2 := envFromDir(repo)
	r2 := &Report{}
	contentRules(e2, r2)
	if b := r2.Blocked(); len(b) > 0 {
		fmt.Printf("正向验证失败：合规内容仍被阻塞 %d 项\n", len(b))
		for _, i := range b {
			fmt.Printf("  [%s] %s %s\n", i.Rule, i.Where, i.Detail)
		}
		return 1
	}

	// ---- 3. 演示音频：放行但不能变成后门 -----------------------------------
	demo := filepath.Join(repo, "examples", "demo")
	must(os.MkdirAll(demo, 0o755))
	must(os.WriteFile(filepath.Join(demo, "ok.mp3"), make([]byte, 64*1024), 0o644))
	must(os.WriteFile(filepath.Join(demo, "README.md"), []byte("| ok.mp3 | 演示 |\n"), 0o644))
	e3 := envFromDir(repo)
	r3 := &Report{}
	RuleNoMedia(e3, r3)
	if len(r3.Items) != 0 {
		fmt.Printf("正向验证失败：合格的演示音频仍被报警：%v\n", r3.Items)
		return 1
	}

	// 去掉登记：应降级为 WARN，而不是 BLOCK —— 分级错了会导致「为了过门禁乱删文件」
	must(os.WriteFile(filepath.Join(demo, "README.md"), []byte("这里故意不提文件名\n"), 0o644))
	e4 := envFromDir(repo)
	r4 := &Report{}
	RuleNoMedia(e4, r4)
	if len(r4.Blocked()) > 0 {
		fmt.Printf("分级失败：未登记的演示音频应只 WARN，不该 BLOCK：%v\n", r4.Blocked())
		return 1
	}
	if len(r4.Warns()) == 0 {
		fmt.Println("反证失败：未登记的演示音频没有提醒")
		return 1
	}

	// 超限：单文件 900 KB，必须 BLOCK
	must(os.WriteFile(filepath.Join(demo, "big.mp3"), make([]byte, 900*1024), 0o644))
	e5 := envFromDir(repo)
	r5 := &Report{}
	RuleNoMedia(e5, r5)
	found := false
	for _, i := range r5.Items {
		if i.Rule == "演示音频过大" {
			found = true
		}
	}
	if !found {
		fmt.Printf("反证失败：超限音频没有被拦：%v\n", r5.Items)
		return 1
	}

	fmt.Println("反证通过：8 类故意违规均被捕获；正向验证：合规内容零阻塞、")
	fmt.Println("          合格演示音频放行、未登记降级 WARN、超限仍 BLOCK。")
	return 0
}

// contentRules 是自测要跑的那批规则（跳过 git 历史与文档存在性，
// 因为临时目录不是 git 仓库、也不该为了自测造一份完整文档树）。
func contentRules(e *Env, r *Report) {
	RuleNoRealHosts(e, r)
	RuleNoInternalHosts(e, r)
	RuleNoSecrets(e, r)
	RuleEnvExampleEmpty(e, r)
	RuleLocalPaths(e, r)
	RuleLicense(e, r)
	RuleGitignore(e, r)
	RuleNoMedia(e, r)
}

// envFromDir 按目录树构造 Env，不依赖 git —— 自测目录里没有版本控制。
func envFromDir(root string) *Env {
	files, _ := walk(root)
	sort.Strings(files)
	e := &Env{Root: root, All: files}
	for _, rel := range files {
		if skipExt(rel) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil || isBinary(b) {
			continue
		}
		e.Files = append(e.Files, File{Rel: rel, Data: b})
	}
	return e
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

const licTemplate = `                                 Apache License
   Copyright [yyyy] [name of copyright owner]

   Licensed under the Apache License, Version 2.0
`

const licFilled = `                                 Apache License
   Copyright 2026 Example Owner

   Licensed under the Apache License, Version 2.0
`

const gitignoreOK = `.env
/logs/
*.mp3
*.wav
!examples/demo/*.mp3
!examples/demo/*.wav
`
