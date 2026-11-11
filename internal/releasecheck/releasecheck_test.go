package releasecheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 这些测试本身就是门禁的一部分：它们保证「规则真的会报」，
// 而不是像注释那样只做声明。任何一个规则被改坏成永远返回空，
// 这里会先红。

func write(t *testing.T, root, name, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", name, err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func rulesOf(r *Report) map[string]bool {
	m := map[string]bool{}
	for _, i := range r.Items {
		m[i.Rule] = true
	}
	return m
}

func TestDetectsRealIPButAllowsDocNetworks(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "note.md", "prod=198.18.0.1\n")
	e := envFromDir(dir)
	r := &Report{}
	RuleNoRealHosts(e, r)
	if !rulesOf(r)["真实 IP 泄露"] {
		t.Fatalf("真实 IP 没被拦下：%v", r.Items)
	}

	// 反向：RFC 5737 文档网段与 docker 网桥必须放行，
	// 否则测试同学每写一个样例就要来改门禁，最后大家会绕开它。
	dir2 := t.TempDir()
	write(t, dir2, "note.md", strings.Join([]string{
		"xff=203.0.113.7, 10.0.0.1",
		"bridge=172.17.0.1",
		"local=127.0.0.1:8123",
		"bind=0.0.0.0",
	}, "\n"))
	e2 := envFromDir(dir2)
	r2 := &Report{}
	RuleNoRealHosts(e2, r2)
	if len(r2.Items) != 0 {
		t.Fatalf("示例地址被误判：%v", r2.Items)
	}
}

func TestDetectsInternalDomain(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "doc.md", "面板地址 https://git.corp.internal/admin\n")
	e := envFromDir(dir)
	r := &Report{}
	RuleNoInternalHosts(e, r)
	if !rulesOf(r)["内部域名泄露"] {
		t.Fatalf("内部域名没被拦下：%v", r.Items)
	}

	// 公开域名不能误伤：文档里到处是 api.elevenlabs.io / github.com
	dir2 := t.TempDir()
	write(t, dir2, "doc.md", "见 https://api.elevenlabs.io/v1/voices 与 https://go.dev/doc\n")
	e2 := envFromDir(dir2)
	r2 := &Report{}
	RuleNoInternalHosts(e2, r2)
	if len(r2.Items) != 0 {
		t.Fatalf("公开域名被误判：%v", r2.Items)
	}
}

func TestDetectsSecretsAndMasksThem(t *testing.T) {
	dir := t.TempDir()
	secret := "sk-abcdefghijklmnopqrstuvwx"
	write(t, dir, "cfg.txt", "token="+secret+"\n")
	e := envFromDir(dir)
	r := &Report{}
	RuleNoSecrets(e, r)
	if !rulesOf(r)["密钥泄露"] {
		t.Fatalf("令牌没被拦下：%v", r.Items)
	}
	// 报告会被贴进 CI 日志和 issue，明文回显等于二次泄露。
	for _, i := range r.Items {
		if strings.Contains(i.Detail, secret) {
			t.Fatalf("报告里回显了完整密钥：%s", i.Detail)
		}
	}
}

func TestDetectsPEMPrivateKey(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "testdata/key.pem", "-----BEGIN RSA PRIVATE KEY-----\nAAAA\n-----END RSA PRIVATE KEY-----\n")
	e := envFromDir(dir)
	r := &Report{}
	RuleNoSecrets(e, r)
	if !rulesOf(r)["私钥入仓"] {
		t.Fatalf("私钥没被拦下：%v", r.Items)
	}
}

func TestEnvExampleMustStayEmpty(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".env.example", "ELEVENLABS_API_KEY=livevalue123456\nADAPTER_PORT=8123\n")
	e := envFromDir(dir)
	r := &Report{}
	RuleEnvExampleEmpty(e, r)
	if !rulesOf(r)["示例文件被填了值"] {
		t.Fatalf("示例文件填了真值没被拦下：%v", r.Items)
	}

	// 端口这类非敏感项不该被误判：只查 KEY/TOKEN/SECRET/PASSWORD 命名的变量
	dir2 := t.TempDir()
	write(t, dir2, ".env.example", "ELEVENLABS_API_KEY=\nADAPTER_PORT=8123\n# KEY 留空，部署时注入\n")
	e2 := envFromDir(dir2)
	r2 := &Report{}
	RuleEnvExampleEmpty(e2, r2)
	if len(r2.Items) != 0 {
		t.Fatalf("空的示例文件被误判：%v", r2.Items)
	}

	// 变量名带 KEY 但值是开关（ADAPTER_ALLOW_LOCAL_KEY=0），不是密钥。
	// 这条来自真实误报：门禁第一次跑就被自己的示例文件绊住，会直接失去信任。
	dir3 := t.TempDir()
	write(t, dir3, ".env.example", "ADAPTER_ALLOW_LOCAL_KEY=0\nADAPTER_MOCK=true\n")
	e3 := envFromDir(dir3)
	r3 := &Report{}
	RuleEnvExampleEmpty(e3, r3)
	if len(r3.Items) != 0 {
		t.Fatalf("布尔开关被误判成密钥：%v", r3.Items)
	}
}

func TestDetectsUnfilledLicenseHolder(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "LICENSE", licTemplate)
	e := envFromDir(dir)
	r := &Report{}
	RuleLicense(e, r)
	if !rulesOf(r)["LICENSE 版权主体"] {
		t.Fatalf("模板占位符没被拦下：%v", r.Items)
	}

	dir2 := t.TempDir()
	write(t, dir2, "LICENSE", licFilled)
	e2 := envFromDir(dir2)
	r2 := &Report{}
	RuleLicense(e2, r2)
	if len(r2.Items) != 0 {
		t.Fatalf("合规 LICENSE 被误判：%v", r2.Items)
	}
}

func TestDetectsLocalUserPath(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "doc.md", `路径 C:\Users\someone\work\repo`+"\n")
	e := envFromDir(dir)
	r := &Report{}
	RuleLocalPaths(e, r)
	if !rulesOf(r)["本机路径泄露"] {
		t.Fatalf("本机路径没被拦下：%v", r.Items)
	}

	// 反向：文档里讲「别写这种路径」时用的是占位写法，不能算泄露。
	// 这条是真实踩到的：门禁第一次跑就拦了自己的 README。
	dir2 := t.TempDir()
	write(t, dir2, "doc.md", "别提交 `C:\\Users\\<name>` 这类路径，Linux 上是 `/home/<user>`\n")
	e2 := envFromDir(dir2)
	r2 := &Report{}
	RuleLocalPaths(e2, r2)
	if len(r2.Items) != 0 {
		t.Fatalf("占位写法被误判成本机路径：%v", r2.Items)
	}
}

// Markdown 里用反引号引用一个词，不等于留下了一个待办标记。
func TestPlaceholderInCodeSpanIsNotATodo(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "doc.md", "| `TODO` 类占位符 | WARN |\n")
	e := envFromDir(dir)
	r := &Report{}
	RulePlaceholders(e, r)
	if len(r.Items) != 0 {
		t.Fatalf("反引号里的 TODO 被当成未完成标记：%v", r.Items)
	}

	// 真的写在正文里的待办仍然要提醒
	dir2 := t.TempDir()
	write(t, dir2, "doc.md", "TODO: 补一段部署说明\n")
	e2 := envFromDir(dir2)
	r2 := &Report{}
	RulePlaceholders(e2, r2)
	if !rulesOf(r2)["未清理的占位符"] {
		t.Fatalf("正文里的 TODO 没被提醒：%v", r2.Items)
	}
}

func TestGitignoreMustCoverLedgerAndMedia(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".gitignore", "node_modules/\n")
	e := envFromDir(dir)
	r := &Report{}
	RuleGitignore(e, r)
	blocks := len(r.Blocked())
	if blocks < 4 {
		t.Fatalf("应至少报 4 项缺失（.env / logs / mp3 / wav），实际 %d：%v", blocks, r.Items)
	}

	dir2 := t.TempDir()
	write(t, dir2, ".gitignore", gitignoreOK)
	e2 := envFromDir(dir2)
	r2 := &Report{}
	RuleGitignore(e2, r2)
	if len(r2.Items) != 0 {
		t.Fatalf("合规 .gitignore 被误判：%v", r2.Items)
	}
}

// 端到端：整个自测流程必须返回 0。这条保证 SelfTest 不会因为后续改动而静默失效。
func TestSelfTestPasses(t *testing.T) {
	if code := SelfTest(); code != 0 {
		t.Fatalf("SelfTest 返回 %d，门禁自身的反证不成立", code)
	}
}

// Run 在真实仓库上必须能跑通且不 panic。
func TestRunOnRealRepo(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("定位仓库根失败: %v", err)
	}
	r, err := Run(root)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if r == nil {
		t.Fatal("Run 返回 nil 报告")
	}
	// 注意：这里不断言「必须零阻塞」。真实仓库当前若存在阻塞项，
	// 那正是门禁该说的话——把它改成断言通过，等于让门禁替仓库背书。
	t.Logf("真实仓库：阻塞 %d 项，提醒 %d 项", len(r.Blocked()), len(r.Warns()))
	for _, i := range r.Blocked() {
		t.Logf("  [%s] %s %s", i.Rule, i.Where, i.Detail)
	}
}

// 历史扫描必须真的能抓到「已经提交过的」泄露。
//
// 这条单独证明，因为它是唯一一条「删文件也救不回来」的规则：
// 工作区扫得再干净，只要历史里有，推上去就是公开的。
// 用临时 git 仓库验证，绝不碰真实仓库的历史。
func TestGitHistoryCatchesCommittedLeak(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("环境里没有 git，跳过历史扫描验证")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		base := []string{"-C", dir, "-c", "user.name=gate-test", "-c", "user.email=gate@example.com"}
		cmd := exec.Command("git", append(base, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	write(t, dir, "ok.txt", "nothing here\n")
	git("add", ".")
	git("commit", "-q", "-m", "init")

	// 提交一个带真实 IP 的文件，然后把它删掉 —— 这正是最常见的翻车姿势：
	// 「我删了呀」，但历史里还在。
	write(t, dir, "leak.txt", "prod=198.18.0.1\n")
	git("add", "leak.txt")
	git("commit", "-q", "-m", "oops")
	if err := os.Remove(filepath.Join(dir, "leak.txt")); err != nil {
		t.Fatalf("删除 leak.txt: %v", err)
	}

	e := &Env{Root: dir}
	r := &Report{}
	RuleGitHistory(e, r)
	if !rulesOf(r)["git 历史 IP"] {
		t.Fatalf("历史里的真实 IP 没被拦下（工作区已删也不行）：%v", r.Items)
	}
}

// 门禁包是免检区，所以必须能挡住「往免检区里塞新文件」。
func TestGatePackageIntegrity(t *testing.T) {
	e := &Env{All: []string{
		selfPkg + "releasecheck.go",
		selfPkg + "selftest.go",
		selfPkg + "releasecheck_test.go",
		"README.md",
	}}
	r := &Report{}
	RuleGatePackageIntegrity(e, r)
	if len(r.Items) != 0 {
		t.Fatalf("已知三文件被误判：%v", r.Items)
	}

	e.All = append(e.All, selfPkg+"leak.txt")
	r2 := &Report{}
	RuleGatePackageIntegrity(e2With(e, selfPkg+"leak.txt"), r2)
	if !rulesOf(r2)["门禁包出现未知文件"] {
		t.Fatalf("免检区里多出文件没被拦下：%v", r2.Items)
	}
}

func e2With(e *Env, extra string) *Env {
	all := append(append([]string{}, e.All...), extra)
	return &Env{Root: e.Root, All: all}
}

// SECURITY.md 里写一个占位邮箱，比没有文件更糟：
// 它让人以为渠道是通的，报告发出去没人收。
func TestSecurityContactMustBeReal(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "SECURITY.md", "报告漏洞：security@your-domain.com\n")
	e := envFromDir(dir)
	r := &Report{}
	RuleSecurityContact(e, r)
	if !rulesOf(r)["漏洞报告邮箱"] {
		t.Fatalf("占位邮箱没被拦下：%v", r.Items)
	}

	dir2 := t.TempDir()
	write(t, dir2, "SECURITY.md", "有问题请提 issue\n")
	e2 := envFromDir(dir2)
	r2 := &Report{}
	RuleSecurityContact(e2, r2)
	if !rulesOf(r2)["漏洞报告邮箱"] {
		t.Fatalf("没有邮箱的 SECURITY.md 没被拦下：%v", r2.Items)
	}

	// 反向：真实邮箱 + GitHub 私密通道，应完全放行
	dir3 := t.TempDir()
	write(t, dir3, "SECURITY.md",
		"发到 someone@real-domain.net，或用 GitHub 的 Security → Report a vulnerability\n")
	e3 := envFromDir(dir3)
	r3 := &Report{}
	RuleSecurityContact(e3, r3)
	if len(r3.Items) != 0 {
		t.Fatalf("合规 SECURITY.md 被误判：%v", r3.Items)
	}

	// 反向：完全不写邮箱、只给 GitHub 私密报告入口，也必须放行。
	// 开源仓库本来就该优先走 advisory —— 报告不进个人信箱，也不要求报告者留邮箱。
	dir5 := t.TempDir()
	write(t, dir5, "SECURITY.md",
		"唯一入口：本仓库 Security → Report a vulnerability\n")
	e5 := envFromDir(dir5)
	r5 := &Report{}
	RuleSecurityContact(e5, r5)
	if len(r5.Items) != 0 {
		t.Fatalf("只用 advisory 入口被误判：%v", r5.Items)
	}

	// 引用块里的占位符是「提示语」，不算没填
	dir4 := t.TempDir()
	write(t, dir4, "SECURITY.md",
		"> 发布前把下面的邮箱换成真实地址\n\n联系人：ops@real-domain.net\n使用 GitHub 的 Report a vulnerability\n")
	e4 := envFromDir(dir4)
	r4 := &Report{}
	RuleSecurityContact(e4, r4)
	if len(r4.Items) != 0 {
		t.Fatalf("提示语被误判成未填：%v", r4.Items)
	}
}
