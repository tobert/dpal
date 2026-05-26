package explorer

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func newTestExplorer(t *testing.T) (*Explorer, string) {
	t.Helper()
	dir := t.TempDir()
	exp, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return exp, dir
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNew_RejectsMissingRoot(t *testing.T) {
	_, err := New(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("expected error for missing root")
	}
}

func TestNew_RejectsNonDirRoot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "afile")
	writeFile(t, dir, "afile", "x")
	if _, err := New(path); err == nil {
		t.Fatal("expected error when root is a file")
	}
}

func TestResolve_RejectsParentEscape(t *testing.T) {
	exp, _ := newTestExplorer(t)
	if _, err := exp.resolve("../etc/passwd"); err == nil {
		t.Fatal("expected error for ../ escape")
	}
	if _, err := exp.resolve("a/b/../../../escape"); err == nil {
		t.Fatal("expected error for nested ../ escape")
	}
}

func TestResolve_RejectsAbsoluteOutsideRoot(t *testing.T) {
	exp, _ := newTestExplorer(t)
	target := "/etc/passwd"
	if runtime.GOOS == "windows" {
		target = `C:\Windows\System32\drivers\etc\hosts`
	}
	if _, err := exp.resolve(target); err == nil {
		t.Fatalf("expected error for absolute path outside root: %s", target)
	}
}

func TestResolve_AllowsValidRelativePath(t *testing.T) {
	exp, dir := newTestExplorer(t)
	writeFile(t, dir, "ok.txt", "hello")
	got, err := exp.resolve("ok.txt")
	if err != nil {
		t.Fatalf("expected resolve to succeed, got %v", err)
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(dir, "ok.txt"))
	if got != want {
		t.Errorf("resolve = %q, want %q", got, want)
	}
}

func TestResolve_RejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink behavior differs on Windows")
	}
	exp, dir := newTestExplorer(t)
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret")
	if err := os.WriteFile(outsideFile, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "escape")
	if err := os.Symlink(outsideFile, link); err != nil {
		t.Fatal(err)
	}
	if _, err := exp.resolve("escape"); err == nil {
		t.Fatal("expected resolve to reject symlink pointing outside the sandbox")
	}
}

func TestListDirectory_FormatsEntries(t *testing.T) {
	exp, dir := newTestExplorer(t)
	writeFile(t, dir, "alpha.txt", "abc")
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exp.ListDirectory(".")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "alpha.txt") {
		t.Errorf("missing file in listing:\n%s", out)
	}
	if !strings.Contains(out, "sub/") {
		t.Errorf("missing dir in listing:\n%s", out)
	}
	if !strings.Contains(out, "3 bytes") {
		t.Errorf("missing file size in listing:\n%s", out)
	}
}

func TestReadFile_ReturnsContents(t *testing.T) {
	exp, dir := newTestExplorer(t)
	writeFile(t, dir, "hello.txt", "world")
	out, err := exp.ReadFile("hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "world") {
		t.Errorf("ReadFile did not include contents:\n%s", out)
	}
	if !strings.Contains(out, "hello.txt") {
		t.Errorf("ReadFile missing path header:\n%s", out)
	}
}

func TestReadFile_TruncatesLargeFile(t *testing.T) {
	exp, dir := newTestExplorer(t)
	exp.maxFileBytes = 10
	writeFile(t, dir, "big.txt", strings.Repeat("x", 1000))
	out, err := exp.ReadFile("big.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "truncated") {
		t.Errorf("expected truncation marker:\n%s", out)
	}
}

func TestReadFile_RejectsDirectory(t *testing.T) {
	exp, dir := newTestExplorer(t)
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := exp.ReadFile("d"); err == nil {
		t.Fatal("expected error reading a directory")
	}
}

func TestSearchProject_FindsMatchesAcrossFiles(t *testing.T) {
	exp, dir := newTestExplorer(t)
	writeFile(t, dir, "a.go", "package a\n// TODO: fix later\nvar X = 1\n")
	writeFile(t, dir, "b.go", "package b\n// nothing here\n")
	writeFile(t, dir, "c.txt", "TODO: outside go files\n")

	out, err := exp.SearchProject("TODO", "*.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.go:2") {
		t.Errorf("expected match in a.go, got:\n%s", out)
	}
	if strings.Contains(out, "c.txt") {
		t.Errorf("c.txt should be filtered out by *.go glob:\n%s", out)
	}
	if !strings.Contains(out, "1 hit") {
		t.Errorf("expected hit count, got:\n%s", out)
	}
}

func TestSearchProject_SkipsGitDir(t *testing.T) {
	exp, dir := newTestExplorer(t)
	gitDir := filepath.Join(dir, ".git")
	if err := os.Mkdir(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte("MATCHME"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "src.go", "// MATCHME\n")

	out, err := exp.SearchProject("MATCHME", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, ".git/") {
		t.Errorf(".git contents leaked into search:\n%s", out)
	}
	if !strings.Contains(out, "src.go") {
		t.Errorf("real source not searched:\n%s", out)
	}
}

func TestSearchProject_RejectsBadRegex(t *testing.T) {
	exp, _ := newTestExplorer(t)
	if _, err := exp.SearchProject("[unclosed", ""); err == nil {
		t.Fatal("expected error for invalid regex")
	}
}

func TestDispatch_ListDirectory(t *testing.T) {
	exp, dir := newTestExplorer(t)
	writeFile(t, dir, "x.txt", "y")
	out := exp.Dispatch("list_directory", `{"path":"."}`)
	if !strings.Contains(out, "x.txt") {
		t.Errorf("dispatch missed file: %s", out)
	}
}

func TestDispatch_BadJSONIsReportedAsError(t *testing.T) {
	exp, _ := newTestExplorer(t)
	out := exp.Dispatch("read_file", `not-json`)
	if !strings.HasPrefix(out, "error:") {
		t.Errorf("expected error string in dispatch result, got: %s", out)
	}
}

func TestDispatch_UnknownToolErrors(t *testing.T) {
	exp, _ := newTestExplorer(t)
	out := exp.Dispatch("ssh_into_prod", `{}`)
	if !strings.Contains(out, "unknown tool") {
		t.Errorf("expected unknown-tool error, got: %s", out)
	}
}

func TestToolDefinitions_CoversAllThreeTools(t *testing.T) {
	exp, _ := newTestExplorer(t)
	defs := exp.ToolDefinitions()
	want := map[string]bool{"list_directory": false, "read_file": false, "search_project": false}
	for _, d := range defs {
		if d.Type != "function" {
			t.Errorf("tool type = %q, want %q", d.Type, "function")
		}
		want[d.Function.Name] = true
	}
	for name, found := range want {
		if !found {
			t.Errorf("missing tool definition for %q", name)
		}
	}
}
