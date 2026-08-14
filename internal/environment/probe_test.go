package environment

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ── firstAbsoluteExecutable ──

func TestFirstAbsoluteExecutable(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "tool")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "subdir")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		out  string
		want string
	}{
		{"empty", "", ""},
		{"not-found-notice", "INFO: Could not find files for the given pattern(s).", ""},
		{"relative-path-ignored", "relative/path\n" + exe, exe},
		{"directory-ignored", sub + "\n" + exe, exe},
		{"multi-line", "junk\n" + exe + "\n" + filepath.Join(dir, "missing"), exe},
		{"absolute-only", "/nonexistent/tool", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := firstAbsoluteExecutable(c.out); got != c.want {
				t.Fatalf("firstAbsoluteExecutable(%q) = %q, want %q", c.out, got, c.want)
			}
		})
	}
}

// ── shellQuote ──

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"node":       "'node'",
		"a b":        "'a b'",
		"it's":       `'it'\''s'`,
		"`rm -rf ~`": "'`rm -rf ~`'", // 反引号在单引号内是字面量
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Fatalf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// ── lookupLoginShell(注入 fake shell,不依赖本机 zsh)──

func TestLookupLoginShellFinds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX login-shell fallback is not used on windows")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "node")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// fake shell: 忽略参数,直接输出 bin 路径 —— 模拟登录 shell 在完整 PATH 下
	// 解析出的绝对路径。
	fakeShell := filepath.Join(dir, "fake-shell")
	if err := os.WriteFile(fakeShell, []byte("#!/bin/sh\necho \""+bin+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := lookupLoginShell(context.Background(), "node", []string{fakeShell}, ProbeOptions{})
	if got != bin {
		t.Fatalf("lookupLoginShell = %q, want %q", got, bin)
	}
}

func TestLookupLoginShellNotFound(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX login-shell fallback is not used on windows")
	}
	dir := t.TempDir()
	// fake shell 输出一条"找不到"提示(相对路径/诊断文本),应返回空。
	fakeShell := filepath.Join(dir, "fake-shell")
	if err := os.WriteFile(fakeShell, []byte("#!/bin/sh\necho 'not found'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := lookupLoginShell(context.Background(), "node", []string{fakeShell}, ProbeOptions{}); got != "" {
		t.Fatalf("lookupLoginShell = %q, want empty", got)
	}
}

func TestLookupLoginShellSkipsMissingShell(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-shell")
	// 不存在的 shell 被跳过,不会报错。
	if got := lookupLoginShell(context.Background(), "node", []string{missing}, ProbeOptions{}); got != "" {
		t.Fatalf("expected empty for missing shell, got %q", got)
	}
}

// ── runOne 集成:LookPath 失败时回退登录 shell ──

func TestRunOneFallsBackToLoginShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("integration fallback is POSIX-only")
	}
	bin := "node"
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("node not installed on this machine: %v", err)
	}
	// 精简 PATH 使直接 LookPath 失败,回退登录 shell(zsh 加载 .zprofile 拿到
	// Homebrew 等路径)应仍能解析。
	t.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")
	if _, err := exec.LookPath(bin); err == nil {
		t.Fatal("test setup failed: node still on minimal PATH")
	}
	res := runOne(context.Background(), bin+" --version", ProbeOptions{})
	if !res.Found {
		t.Fatalf("fallback did not resolve %s: %s", bin, res.Error)
	}
	if res.Binary != bin {
		t.Fatalf("Binary = %q, want %q", res.Binary, bin)
	}
	if !strings.Contains(res.Output, "v") && res.Output == "" {
		t.Fatalf("unexpected output %q", res.Output)
	}
}

func TestRunOneDirectLookupStillWins(t *testing.T) {
	bin := "git"
	if _, err := exec.LookPath(bin); err != nil {
		t.Skip("git not installed on this machine")
	}
	res := runOne(context.Background(), bin+" version", ProbeOptions{})
	if !res.Found {
		t.Fatalf("direct lookup should succeed for %s: %s", bin, res.Error)
	}
}
