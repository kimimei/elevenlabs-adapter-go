// Package releasecheck 是发布前门禁：把「人为记得别上传」变成「不删就报错」。
//
// 与 Python 版 scripts/release_check.py 规则同源，但有三处按 Go 仓库的实际情况做了改动：
//
//  1. **扫描对象是「会被提交的内容」**（已跟踪 + 未忽略的新文件），不再遍历工作区。
//     门禁要拦的是「会上传的东西」；`.smoke.log`、`logs/billing/*.jsonl`、
//     编译出来的 bin/ 本来就不会上传，扫它们只会制造噪声，噪声多了门禁就没人看了。
//  2. **额外检查 .env.example 的值**：泄露最常见的形态不是藏着密钥，
//     而是「示例文件被谁顺手填了真值然后提交」。
//  3. **历史扫描覆盖 IP 与内部域名**，不只密钥。
//     `git rm` 删文件不等于删历史——公开仓库的第一天，历史就是公开的。
//
// 设计原则（与仓库其余门禁一致）：门禁不能自证，只能反证。
// 所以每条规则都用「找出违规」实现，并由 releasecheck_test.go 注入违规样本验证它真的会红。
package releasecheck

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Level 是问题等级。BLOCK 会拦发布，WARN 只提醒。
type Level string

const (
	Block Level = "BLOCK"
	Warn  Level = "WARN"
)

// Item 是一条发现。
type Item struct {
	Level  Level  `json:"level"`
	Rule   string `json:"rule"`
	Where  string `json:"where"`
	Detail string `json:"detail"`
}

// Report 收集全部发现。
type Report struct {
	Items []Item `json:"items"`
}

// Add 记一条发现。
func (r *Report) Add(l Level, rule, where, detail string) {
	r.Items = append(r.Items, Item{Level: l, Rule: rule, Where: where, Detail: detail})
}

// Blocked 返回阻塞项。
func (r *Report) Blocked() []Item { return r.filter(Block) }

// Warns 返回提醒项。
func (r *Report) Warns() []Item { return r.filter(Warn) }

func (r *Report) filter(l Level) []Item {
	var out []Item
	for _, i := range r.Items {
		if i.Level == l {
			out = append(out, i)
		}
	}
	return out
}

// OK 表示没有阻塞项（提醒项不影响发布）。
func (r *Report) OK() bool { return len(r.Blocked()) == 0 }

// File 是一个待检查的文本文件。
type File struct {
	Rel  string // 相对仓库根，统一用 / 分隔，报告里直接给人看
	Data []byte
}

// Text 返回文件内容。
func (f File) Text() string { return string(f.Data) }

// Env 是门禁的输入。
type Env struct {
	Root  string
	Files []File   // 已跟踪的文本文件
	All   []string // 已跟踪的全部文件（含二进制），相对路径
}

// ---------------------------------------------------------------------------
// 载入
// ---------------------------------------------------------------------------

// selfPkg 是本门禁自己的源码目录。
//
// 它必须从内容扫描里剔除：这些文件按定义包含「违规样例」（假 IP、假令牌、
// 假私钥、内部域名正则），否则规则没法被反证。让门禁扫自己，
// 结果就是它永远在报自己——没人会长期忍受一个永远红的门禁。
//
// 剔除留下的口子由 RuleGatePackageIntegrity 兜：该目录只允许出现已知的
// 三个文件，多一个就报 BLOCK，避免有人把真东西塞进这个「免检区」。
const selfPkg = "internal/releasecheck/"

// selfPkgFiles 是 selfPkg 目录下允许存在的文件。
var selfPkgFiles = map[string]bool{
	selfPkg + "releasecheck.go":      true,
	selfPkg + "releasecheck_test.go": true,
	selfPkg + "selftest.go":          true,
}

// Load 载入 root 下会被提交的文件。
// 不是 git 仓库时退回遍历目录——门禁要能跑在还没 init 的目录上，否则第一次发布反而没人查。
func Load(root string) (*Env, error) {
	all, err := tracked(root)
	if err != nil {
		all, err = walk(root)
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(all)

	e := &Env{Root: root, All: all}
	for _, rel := range all {
		if skipExt(rel) || strings.HasPrefix(rel, selfPkg) {
			continue // 自身含有故意违规的样例，见 selfPkg 的说明
		}
		b, rerr := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if rerr != nil {
			continue // 已被删除但仍在索引里，跳过即可，不当成发现
		}
		if isBinary(b) {
			continue
		}
		e.Files = append(e.Files, File{Rel: rel, Data: b})
	}
	return e, nil
}

// tracked 返回「会被提交的内容」= 已跟踪 ∪ 未跟踪但未被 .gitignore 排除。
//
// 只取已跟踪文件是不够的：门禁最该生效的时刻是**提交之前**，
// 那时候新写的文件还没 git add，`git ls-files` 根本看不见它 ——
// 而漏一条 IP 到公开仓库，通常就发生在这一次「还没 add」。
//
// 被 .gitignore 排除的文件确实不进仓库，扫它们只会制造噪声。
func tracked(root string) ([]string, error) {
	cmd := exec.Command("git", "-C", root, "ls-files", "-z",
		"--cached", "--others", "--exclude-standard")
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git ls-files: %v %s", err, strings.TrimSpace(errb.String()))
	}
	var files []string
	for _, p := range strings.Split(out.String(), "\x00") {
		if p == "" {
			continue
		}
		files = append(files, filepath.ToSlash(p))
	}
	return files, nil
}

func walk(root string) ([]string, error) {
	var files []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		name := info.Name()
		if info.IsDir() {
			if name == ".git" || strings.HasPrefix(name, ".venv") || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	return files, err
}

// skipExt 跳过大文件与二进制扩展名：它们不承载「文案泄漏」，
// 但会把扫描时间拖长，也会让报告里出现无法定位的乱码。
var skipExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".ico": true,
	".zip": true, ".gz": true, ".tgz": true, ".pdf": true,
	".exe": true, ".so": true, ".dll": true, ".dylib": true,
	".pyc": true, ".woff": true, ".woff2": true, ".ttf": true,
	".mp3": true, ".wav": true, // 音频由 RuleNoMedia 单独处理（要查体积与出处）
}

func skipExt(rel string) bool {
	return skipExts[strings.ToLower(filepath.Ext(rel))]
}

func isBinary(b []byte) bool {
	n := len(b)
	if n > 8000 {
		n = 8000
	}
	return bytes.IndexByte(b[:n], 0) >= 0
}

// ---------------------------------------------------------------------------
// 白名单
// ---------------------------------------------------------------------------

// allowedIPs 是「已知的示例地址」。往里加之前先问一句：这个地址出现在公开仓库里，
// 陌生人能不能据此定位到某台真实机器？
//
// 允许：回环、未指定、RFC 5737 文档网段（不可能分配给真实主机）、
// docker 默认网桥（172.17/172.18 是 docker 的出厂网段，写在文档里是通用写法）。
var allowedIPs = map[string]bool{
	"127.0.0.1": true,
	"0.0.0.0":   true,

	"172.17.0.1": true, // docker 默认网桥网关
	"172.17.0.2": true,
	"172.18.0.1": true, // docker 自定义网桥（compose 默认起的就是这个网段）
	"172.18.0.2": true,
	"10.0.0.1":   true, // XFF 测试样本
	"10.9.9.9":   true, // 私网地址说明
}

// docNets 是 RFC 5737 保留给文档的三个网段。
// 用它们做测试是刻意的：这些地址永远连不上真实主机，
// 换成 127.0.0.1 反而会因为本机直连恰好成功而测不出问题。
var docNets = []string{"192.0.2.", "198.51.100.", "203.0.113."}

func ipAllowed(ip string) bool {
	if allowedIPs[ip] {
		return true
	}
	for _, p := range docNets {
		if strings.HasPrefix(ip, p) {
			return true
		}
	}
	return false
}

// defaultInternalSuffixes 是「通用」内部后缀：IANA 保留给私有网络，
// 不指向任何具体组织。
//
// 项目自己的域名**不能写在这里**——把 `panel.你的公司域名` 写进公开仓库的源码，
// 就是本门禁要防的那种泄露，只不过换了个位置发生。
// 项目专属后缀从仓库外读（见 Env.internalSuffixes）：
//
//	RELEASE_CHECK_INTERNAL_HOSTS=your-corp.example,another.example   # 环境变量，CI 里走 secrets
//	.release-check.local                                             # 仓库根，已在 .gitignore 里
var defaultInternalSuffixes = []string{
	".internal", ".intranet", ".corp", ".lan", ".home.arpa",
}

// allowedHosts 是文档里教用户填的占位域名，以及官方公开域名。
var allowedHosts = map[string]bool{
	"example.com": true, "example.org": true, "example.net": true,
	"your-domain.com": true, "localhost": true,
	"api.elevenlabs.io": true, "elevenlabs.io": true,
	"github.com": true, "raw.githubusercontent.com": true,
	"go.dev": true, "golang.org": true, "pkg.go.dev": true,
	"apache.org": true, "www.apache.org": true,
}

// localConfigFile 是项目专属内部后缀的本地清单（不进仓库，一行一个后缀）。
const localConfigFile = ".release-check.local"

// internalSuffixes 汇总生效的内部后缀：默认 + 本地文件 + 环境变量。
func (e *Env) internalSuffixes() []string {
	out := append([]string{}, defaultInternalSuffixes...)

	if b, err := os.ReadFile(filepath.Join(e.Root, localConfigFile)); err == nil {
		out = append(out, splitHosts(string(b))...)
	}
	if v := os.Getenv("RELEASE_CHECK_INTERNAL_HOSTS"); v != "" {
		out = append(out, splitHosts(v)...)
	}

	for i, s := range out {
		s = strings.ToLower(strings.TrimSpace(s))
		s = strings.TrimPrefix(s, "*")
		if s != "" && !strings.HasPrefix(s, ".") {
			s = "." + s
		}
		out[i] = s
	}
	return uniq(out)
}

// splitHosts 按行/逗号拆分，忽略空行与 # 注释。
func splitHosts(s string) []string {
	var out []string
	for _, ln := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == ',' }) {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		out = append(out, ln)
	}
	return out
}

// allowedLocalPaths 是本机绝对路径的白名单形态。真正的规则是「不许出现具体用户名」，
// 这里只放行明显通用的写法。
var allowedPlaceholders = map[string]bool{
	"sk-xxx": true, "sk_xxx": true, "sk-xxxx": true,
	"<token>": true, "<your-key>": true,
	"your-api-key": true, "your-key": true,
}

// ---------------------------------------------------------------------------
// 规则
// ---------------------------------------------------------------------------

var (
	ipRe    = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	skRe    = regexp.MustCompile(`\bsk[-_][A-Za-z0-9]{16,}\b`)
	awsRe   = regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)
	pemRe   = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)
	hostRe  = regexp.MustCompile(`(?i)\b[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)+`)
	xiKeyRe = regexp.MustCompile(`(?i)xi-api-key["'\s:=]+([A-Za-z0-9]{24,})`)
	// 本机路径必须带上**真实的用户名**才算泄露。
	// 只写到 `C:\Users\` 或 `/home/<user>/` 是文档在讲「别写这种路径」，
	// 那是占位写法，不是泄露 —— 判据放这么宽的门禁，会先把自己写进黑名单。
	userPath   = regexp.MustCompile(`(?i)(?:[A-Z]:[\\/]Users[\\/][A-Za-z0-9._-]+|/home/[A-Za-z0-9._-]+|/Users/[A-Za-z0-9._-]+)`)
	codeSpanRe = regexp.MustCompile("`[^`]*`")
)

// RuleNoRealHosts 拦截真实 IP。
func RuleNoRealHosts(e *Env, r *Report) {
	for _, f := range e.Files {
		for _, ip := range uniq(ipRe.FindAllString(f.Text(), -1)) {
			if ipAllowed(ip) {
				continue
			}
			r.Add(Block, "真实 IP 泄露", where(f, ip), ip+" 不在白名单内（回环 / RFC5737 文档网段 / docker 默认网桥）")
		}
	}
}

// RuleNoInternalHosts 拦截内部域名。
func RuleNoInternalHosts(e *Env, r *Report) {
	suffixes := e.internalSuffixes()
	for _, f := range e.Files {
		for _, h := range uniq(hostRe.FindAllString(f.Text(), -1)) {
			low := strings.ToLower(h)
			if allowedHosts[low] {
				continue
			}
			for _, suf := range suffixes {
				if strings.HasSuffix(low, suf) {
					r.Add(Block, "内部域名泄露", where(f, h), h+" 命中内部后缀 "+suf)
					break
				}
			}
		}
	}
}

// RuleNoSecrets 拦截令牌与私钥。
func RuleNoSecrets(e *Env, r *Report) {
	for _, f := range e.Files {
		s := f.Text()
		for _, k := range uniq(skRe.FindAllString(s, -1)) {
			if allowedPlaceholders[k] || strings.EqualFold(k, "sk-xxx") {
				continue
			}
			r.Add(Block, "密钥泄露", where(f, k), "疑似真实令牌 "+mask(k))
		}
		for _, k := range uniq(awsRe.FindAllString(s, -1)) {
			r.Add(Block, "密钥泄露", where(f, k), "疑似 AWS AccessKey "+mask(k))
		}
		if pemRe.MatchString(s) {
			r.Add(Block, "私钥入仓", f.Rel, "出现 PEM 私钥块——任何情况下都不该进仓库，连测试用也不行")
		}
		for _, m := range xiKeyRe.FindAllStringSubmatch(s, -1) {
			// xi-api-key 后面跟的一长串就是 ElevenLabs 的 Key 本体。
			// 文档里会写 xi-api-key: $ELEVENLABS_API_KEY，那种带 $ 的不会命中这条正则。
			r.Add(Block, "密钥泄露", where(f, m[1]), "xi-api-key 后出现疑似明文 Key "+mask(m[1]))
		}
	}
}

// RuleEnvExampleEmpty 检查示例环境变量文件里没有填真值。
//
// 这条规则来自最容易被忽视的一种泄露：示例文件本身该是空的，
// 但有人为了「本地能跑」把自己那把 Key 填进去提交了。文件名叫 .env.example
// 会让人觉得它无关紧要，实际上它照样公开。
func RuleEnvExampleEmpty(e *Env, r *Report) {
	assign := regexp.MustCompile(`^\s*([A-Z0-9_]*(?:KEY|TOKEN|SECRET|PASSWORD|PASSWD)[A-Z0-9_]*)\s*=\s*(.*)$`)
	for _, f := range e.Files {
		base := strings.ToLower(filepath.Base(f.Rel))
		if base != ".env.example" && !strings.HasSuffix(base, ".env.example") {
			continue
		}
		for i, line := range strings.Split(f.Text(), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			m := assign.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			val := strings.TrimSpace(strings.SplitN(m[2], "#", 2)[0])
			val = strings.Trim(val, `"'`)
			if val == "" {
				continue
			}
			// 变量名里带 KEY 不等于它是密钥：ADAPTER_ALLOW_LOCAL_KEY=0 是开关，
			// 这类布尔值必须放行，否则规则会把配置文件读成泄密，第一次跑就误报。
			if isBoolLiteral(val) {
				continue
			}
			r.Add(Block, "示例文件被填了值", fmt.Sprintf("%s:%d", f.Rel, i+1),
				m[1]+" 有值（"+mask(val)+"）——示例文件必须留空，否则等于把 Key 一起发布")
		}
	}
}

// isBoolLiteral 判断值是不是开关类字面量。这类值出现在示例文件里是正常的。
func isBoolLiteral(v string) bool {
	switch strings.ToLower(v) {
	case "0", "1", "true", "false", "yes", "no", "on", "off":
		return true
	}
	return false
}

// RuleNoMedia 拦截测试音频入仓，但放行「演示音频」——放行是有条件的。
//
// 这条规则的动机是「测试产物误入」：体积大、版权不明、半年后没人知道它是谁。
// 演示音频恰好相反，它是给访问者听的资产。所以不是加白名单了事，
// 而是把当初担心的两件事变成可检查的约束：体积、出处。
func RuleNoMedia(e *Env, r *Report) {
	const (
		demoDir    = "examples/demo/"
		manifest   = "examples/demo/README.md"
		maxKB      = 800      // 单文件；128kbps 单声道约 1 分钟
		totalMaxKB = 4 * 1024 // 合计
	)
	manifestText := ""
	for _, f := range e.Files {
		if f.Rel == manifest {
			manifestText = f.Text()
		}
	}
	total := 0.0
	for _, rel := range e.All {
		ext := strings.ToLower(filepath.Ext(rel))
		if ext != ".mp3" && ext != ".wav" {
			continue
		}
		st, err := os.Stat(filepath.Join(e.Root, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		kb := float64(st.Size()) / 1024
		if !strings.HasPrefix(rel, demoDir) {
			r.Add(Block, "音频入仓", rel, "发布前删除；演示音频只允许放在 "+demoDir)
			continue
		}
		total += kb
		if kb > maxKB {
			r.Add(Block, "演示音频过大", rel, fmt.Sprintf("%.0f KB 超过 %d KB，请降码率或裁短", kb, maxKB))
		}
		if manifestText == "" || !strings.Contains(manifestText, filepath.Base(rel)) {
			r.Add(Warn, "演示音频未登记", rel, "写进 "+manifest+" 的表格：场景、音色、文案")
		}
	}
	if total > totalMaxKB {
		r.Add(Block, "演示音频总量超限", demoDir, fmt.Sprintf("合计 %.1f MB，超过 %d MB", total/1024, totalMaxKB/1024))
	}
}

// RuleGitignore 要求忽略关键产物。
//
// 账本（logs/）里含客户 IP 与 token 前缀，.env 含 Key —— 这两个漏一个就够出事。
func RuleGitignore(e *Env, r *Report) {
	var s string
	for _, f := range e.Files {
		if f.Rel == ".gitignore" {
			s = f.Text()
		}
	}
	if s == "" {
		r.Add(Block, ".gitignore 缺失", ".gitignore", "没有 .gitignore，Key 与账本迟早被提交")
		return
	}
	for _, must := range []string{".env", "/logs/", "*.mp3", "*.wav"} {
		if !strings.Contains(s, must) {
			r.Add(Block, ".gitignore 覆盖", ".gitignore", "未忽略 "+must)
		}
	}
}

// RuleLicense 检查 LICENSE 有具体版权主体。
//
// Apache-2.0 的官方模板自带 `Copyright [yyyy] [name of copyright owner]` 两处占位符，
// 直接复制不改的人很多 —— 那份许可证在法律上是空的。
func RuleLicense(e *Env, r *Report) {
	const rel = "LICENSE"
	var s string
	for _, f := range e.Files {
		if f.Rel == rel {
			s = f.Text()
		}
	}
	if s == "" {
		r.Add(Block, "LICENSE 存在", rel, "文件缺失")
		return
	}
	line := regexp.MustCompile(`(?m)^\s*Copyright\s+(?:\(c\)\s*)?(\d{4})?\s*(\[[^\]]*\]|[^\n]+)$`)
	m := line.FindStringSubmatch(s)
	if m == nil {
		r.Add(Block, "LICENSE 版权主体", rel, "找不到 Copyright 行")
		return
	}
	holder := strings.TrimSpace(m[2])
	if holder == "" || strings.Contains(holder, "[") {
		r.Add(Block, "LICENSE 版权主体", rel, "仍是模板占位符「"+holder+"」，发布前必须填真实主体")
		return
	}
	low := strings.ToLower(holder)
	if low == "contributors" || low == "the contributors" || low == "none" {
		r.Add(Block, "LICENSE 版权主体", rel, "署名「"+holder+"」过于模糊，建议填具体主体")
	}
	// ElevenLabs 是第三方公司名，写进版权声明会造成「这是官方项目」的误认。
	if strings.Contains(low, "elevenlabs") {
		r.Add(Block, "LICENSE 版权主体", rel, "署名含 ElevenLabs——那是第三方公司，会造成误认")
	}
}

// RuleLocalPaths 拦截本机绝对路径与用户名。
//
// 这类内容不算密钥，但它暴露「谁在哪台机器上开发」，而且对读者毫无价值。
func RuleLocalPaths(e *Env, r *Report) {
	for _, f := range e.Files {
		for _, m := range uniq(userPath.FindAllString(f.Text(), -1)) {
			r.Add(Block, "本机路径泄露", where(f, m), m+" —— 换成相对路径或 /path/to/xxx")
		}
	}
}

// RulePlaceholders 提醒未清理的 TODO 类标记。
//
// 只 WARN：Go 代码里的 TODO 往往是真待办，不像「发布前必删」那种东西。
// 但它出现在文档里通常意味着有一句没写完。
func RulePlaceholders(e *Env, r *Report) {
	todo := regexp.MustCompile(`\b(TODO|FIXME|CHANGEME)\b`)
	for _, f := range e.Files {
		if strings.HasSuffix(f.Rel, "_test.go") {
			continue // 测试里写 TODO 描述待补场景是正常的
		}
		for i, ln := range strings.Split(f.Text(), "\n") {
			if strings.HasPrefix(strings.TrimSpace(ln), "//") && strings.Contains(ln, "TODO") {
				continue // 注释里的 TODO 是待办，不是未填项
			}
			// Markdown 里的反引号是「在谈论这个词」，不是「留下了这个标记」。
			// 不区分的话，写文档说明本规则的人会被自己的文档拦住。
			match := ln
			if strings.HasSuffix(f.Rel, ".md") {
				match = codeSpanRe.ReplaceAllString(match, "")
			}
			for _, m := range uniq(todo.FindAllString(match, -1)) {
				r.Add(Warn, "未清理的占位符", fmt.Sprintf("%s:%d", f.Rel, i+1), m)
			}
		}
	}
}

// RuleDocs 检查公开仓库该有的文档。
func RuleDocs(e *Env, r *Report) {
	has := map[string]bool{}
	for _, rel := range e.All {
		has[rel] = true
	}
	for _, d := range []struct {
		name  string
		level Level
		why   string
	}{
		{"README.md", Block, "首屏，没有它别人不知道这是什么"},
		{"LICENSE", Block, "没有许可证等于默认保留一切权利"},
		{"SECURITY.md", Block, "漏洞报告渠道；缺了别人只能在公开 issue 里披露漏洞细节"},
		{"docs/CONTRACT.md", Warn, "对外契约；两个实现靠它对齐"},
	} {
		if !has[d.name] {
			r.Add(d.level, "文档缺失", d.name, "缺少 "+d.name+"（"+d.why+"）")
		}
	}
	RuleSecurityContact(e, r)
}

// placeholderDomains 是「一看就没填」的域名。example.com 不在此列：
// 它是 RFC 保留域名，但也可能是真实在用的域名。
var placeholderDomains = map[string]bool{
	"your-domain.com": true, "yourdomain.com": true, "domain.com": true,
	"mydomain.com": true, "example-domain.com": true, "changeme.com": true,
}

// RuleSecurityContact 要求 SECURITY.md 给出**能用的**联系方式。
//
// 有文件但写着 security@your-domain.com，比没有更糟：它让人以为渠道是通的，
// 报告发出去没人收，漏洞就一直躺在那里。
func RuleSecurityContact(e *Env, r *Report) {
	var s string
	for _, f := range e.Files {
		if f.Rel == "SECURITY.md" {
			s = f.Text()
		}
	}
	if s == "" {
		return // 文件缺失已由 RuleDocs 报过，不重复
	}

	// Markdown 引用块常用来写「发布前记得换成真实邮箱」这类提示语，
	// 里面的占位符是故意留的，不算没填。
	var body strings.Builder
	for _, ln := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), ">") {
			continue
		}
		body.WriteString(ln)
		body.WriteString("\n")
	}

	emailRe := regexp.MustCompile(`[\w.+-]+@[\w.-]+\.[A-Za-z]{2,}`)
	emails := uniq(emailRe.FindAllString(body.String(), -1))

	// 两种合法渠道，具其一即可：
	//   1) 一个真实可收信的邮箱
	//   2) GitHub 的私密漏洞报告（Security → Report a vulnerability）
	// 后者对开源仓库更合适：报告不进个人信箱，能直接在 advisory 里协作、出私密补丁，
	// 也不要求报告者暴露自己的邮箱。两者都没有才是问题 —— 那等于漏洞无处可报。
	hasAdvisory := strings.Contains(s, "Report a vulnerability") || strings.Contains(s, "Security Advisory")
	if len(emails) == 0 && !hasAdvisory {
		r.Add(Block, "漏洞报告邮箱", "SECURITY.md",
			"既没有邮箱，也没有 GitHub 私密报告入口 —— 漏洞无处可报")
		return
	}
	for _, em := range emails {
		dom := strings.ToLower(em[strings.IndexByte(em, '@')+1:])
		if placeholderDomains[dom] {
			r.Add(Block, "漏洞报告邮箱", "SECURITY.md", em+" 是占位符，换成真实可收信的邮箱")
		}
	}
	// 只靠邮箱仍有风险：换人、离职、被墙都可能失效，建议同时给出 advisory 入口。
	if len(emails) > 0 && !hasAdvisory {
		r.Add(Warn, "私密报告通道", "SECURITY.md",
			"未提 GitHub 的 Security Advisory 入口，建议补上作为邮箱之外的备份通道")
	}
}

// RuleGatePackageIntegrity 守住 selfPkg 这个免检区。
//
// 门禁给自己开了一个口子（不扫自己的源码），口子必须可审计：
// 该目录里只允许出现已知的三个文件。多出任何文件，都不是「顺手放的」，
// 得有人明确把文件名加进白名单并说明理由。
func RuleGatePackageIntegrity(e *Env, r *Report) {
	for _, rel := range e.All {
		if !strings.HasPrefix(rel, selfPkg) {
			continue
		}
		if !selfPkgFiles[rel] {
			r.Add(Block, "门禁包出现未知文件", rel,
				"该目录不参与内容扫描，新增文件需人工确认并登记到 selfPkgFiles")
		}
	}
}

// RuleGitHistory 扫历史里的密钥与真实 IP。
//
// 这是最重要的一条：删文件只是把文件从工作区拿走，历史里那条记录还在。
// 公开仓库的第一天，历史就是公开的 —— 所以必须在 push 之前查。
func RuleGitHistory(e *Env, r *Report) {
	if _, err := os.Stat(filepath.Join(e.Root, ".git")); err != nil {
		r.Add(Warn, "git 历史", ".git", "尚未初始化 git —— 首次提交前确认 .gitignore 生效")
		return
	}
	// 排除本包：它必然包含违规样例（正则、示例 Key、注入用的假地址），
	// 历史扫描会永远报自己。门禁不能把自己判成罪犯。
	cmd := exec.Command("git", "-C", e.Root, "log", "-p", "--all", "--", ".",
		":(exclude)"+selfPkg)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		r.Add(Warn, "git 历史", ".git", "扫描失败："+strings.TrimSpace(errb.String()))
		return
	}
	diff := out.String()
	for _, k := range uniq(skRe.FindAllString(diff, -1)) {
		if allowedPlaceholders[k] {
			continue
		}
		r.Add(Block, "git 历史密钥", ".git", "历史提交里出现疑似令牌 "+mask(k)+" —— 删文件不够，需重建仓库后再推")
	}
	for _, ip := range uniq(ipRe.FindAllString(diff, -1)) {
		if ipAllowed(ip) {
			continue
		}
		r.Add(Block, "git 历史 IP", ".git", "历史提交里出现真实 IP "+ip+" —— 用 git filter-repo 清理后再公开")
	}
	for _, h := range uniq(hostRe.FindAllString(diff, -1)) {
		low := strings.ToLower(h)
		for _, suf := range e.internalSuffixes() {
			if strings.HasSuffix(low, suf) {
				r.Add(Block, "git 历史域名", ".git", "历史提交里出现内部域名 "+h)
				break
			}
		}
	}
}

// Rules 是全部规则，按「先扫内容，再查结构，最后翻历史」的顺序执行。
var Rules = []struct {
	Name string
	Run  func(*Env, *Report)
}{
	{"no-real-hosts", RuleNoRealHosts},
	{"no-internal-hosts", RuleNoInternalHosts},
	{"no-secrets", RuleNoSecrets},
	{"env-example-empty", RuleEnvExampleEmpty},
	{"no-local-paths", RuleLocalPaths},
	{"license", RuleLicense},
	{"gitignore", RuleGitignore},
	{"docs", RuleDocs},
	{"gate-integrity", RuleGatePackageIntegrity},
	{"no-media", RuleNoMedia},
	{"placeholders", RulePlaceholders},
	{"git-history", RuleGitHistory},
}

// Run 跑完全部规则。规则自身 panic 也算发现——静默跳过等于门禁失效。
func Run(root string) (*Report, error) {
	e, err := Load(root)
	if err != nil {
		return nil, err
	}
	r := &Report{}
	for _, rule := range Rules {
		func() {
			defer func() {
				if p := recover(); p != nil {
					r.Add(Block, "规则执行异常", rule.Name, fmt.Sprint(p))
				}
			}()
			rule.Run(e, r)
		}()
	}
	return r, nil
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func uniq(in []string) []string {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// where 让报告能直接跳到行号：只给文件名，等于让人自己去 grep 整个仓库。
func where(f File, needle string) string {
	for i, ln := range strings.Split(f.Text(), "\n") {
		if strings.Contains(ln, needle) {
			return fmt.Sprintf("%s:%d", f.Rel, i+1)
		}
	}
	return f.Rel
}

// mask 保留前 6 位：够用来判断「这是哪把 Key」，又不足以复现它。
// 报告会被贴进 issue、CI 日志，明文回显等于二次泄露。
func mask(s string) string {
	if len(s) <= 8 {
		return strings.Repeat("*", len(s))
	}
	return s[:6] + "…(" + strconv.Itoa(len(s)) + " 字符)"
}
