package explorer

import (
	"fmt"
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

func mkdir(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
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

func TestReadFile_ExactCapBoundary(t *testing.T) {
	// File equal to the cap: full contents returned, no truncation marker.
	exp, dir := newTestExplorer(t)
	exp.maxFileBytes = 10
	writeFile(t, dir, "exact.txt", strings.Repeat("y", 10))
	out, err := exp.ReadFile("exact.txt")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "truncated") {
		t.Errorf("file exactly at cap should not be marked truncated:\n%s", out)
	}
	if !strings.Contains(out, "10 bytes") {
		t.Errorf("expected size header for 10-byte file:\n%s", out)
	}
}

func TestReadFile_EmptyFile(t *testing.T) {
	exp, dir := newTestExplorer(t)
	writeFile(t, dir, "empty.txt", "")
	out, err := exp.ReadFile("empty.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "0 bytes") {
		t.Errorf("expected 0-byte size header:\n%s", out)
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

func TestSearchProject_ReportsUnreadableFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission semantics differ on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permission bits; cannot make a file unreadable")
	}
	exp, dir := newTestExplorer(t)
	writeFile(t, dir, "ok.go", "// MATCHME\n")
	denied := filepath.Join(dir, "denied.go")
	if err := os.WriteFile(denied, []byte("// MATCHME\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(denied, 0o644) })

	out, err := exp.SearchProject("MATCHME", "*.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ok.go") {
		t.Errorf("readable file should still match:\n%s", out)
	}
	if !strings.Contains(out, "unreadable") {
		t.Errorf("expected footer to mention unreadable files:\n%s", out)
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

func TestToolDefinitions_CoversAllTools(t *testing.T) {
	exp, _ := newTestExplorer(t)
	defs := exp.ToolDefinitions()
	want := map[string]bool{"list_directory": false, "read_file": false, "search_project": false, "project_tree": false}
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

func TestProjectTree_HonorsGitignore(t *testing.T) {
	exp, dir := newTestExplorer(t)
	writeFile(t, dir, ".gitignore", "ignored/\n*.log\n")
	writeFile(t, dir, "keep.go", "package x")
	writeFile(t, dir, "app.log", "noise")
	mkdir(t, dir, "ignored")
	writeFile(t, dir, "ignored/secret.go", "shh")
	mkdir(t, dir, "sub")
	writeFile(t, dir, "sub/keep.txt", "ok")

	out, err := exp.ProjectTree(".")
	if err != nil {
		t.Fatalf("ProjectTree: %v", err)
	}
	if !strings.Contains(out, "keep.go") || !strings.Contains(out, "sub/keep.txt") {
		t.Errorf("expected tracked files in tree, got:\n%s", out)
	}
	for _, gone := range []string{"secret.go", "app.log", "ignored/"} {
		if strings.Contains(out, gone) {
			t.Errorf("gitignored path %q leaked into tree:\n%s", gone, out)
		}
	}
}

func TestProjectTree_SkipsDefaultDirs(t *testing.T) {
	exp, dir := newTestExplorer(t)
	// No .gitignore: the built-in skip-set must still prune dependency/build
	// dirs so a Rust target/ or node_modules can't blow up the output.
	mkdir(t, dir, "node_modules")
	writeFile(t, dir, "node_modules/dep.js", "x")
	mkdir(t, dir, "target")
	writeFile(t, dir, "target/huge.bin", "x")
	writeFile(t, dir, "main.rs", "fn main(){}")

	out, err := exp.ProjectTree(".")
	if err != nil {
		t.Fatalf("ProjectTree: %v", err)
	}
	if !strings.Contains(out, "main.rs") {
		t.Errorf("expected source file in tree, got:\n%s", out)
	}
	for _, gone := range []string{"node_modules", "target", "dep.js", "huge.bin"} {
		if strings.Contains(out, gone) {
			t.Errorf("skip-set dir %q leaked into tree:\n%s", gone, out)
		}
	}
}

func TestProjectTree_CapsEntriesAndMarksTruncation(t *testing.T) {
	exp, dir := newTestExplorer(t)
	exp.maxTreeEntries = 3
	for i := range 50 {
		writeFile(t, dir, fmt.Sprintf("f%02d.txt", i), "x")
	}
	out, err := exp.ProjectTree(".")
	if err != nil {
		t.Fatalf("ProjectTree: %v", err)
	}
	if !strings.Contains(out, "truncat") {
		t.Errorf("expected a truncation marker when the entry cap is hit, got:\n%s", out)
	}
	// Count emitted file lines (those carrying a byte size); must not exceed cap.
	got := strings.Count(out, ".txt")
	if got > exp.maxTreeEntries {
		t.Errorf("emitted %d entries, want <= cap %d", got, exp.maxTreeEntries)
	}
}
