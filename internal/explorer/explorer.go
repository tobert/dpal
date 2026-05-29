// Package explorer provides sandboxed read-only filesystem tools that
// dpal exposes to DeepSeek V4 via function calling so the model can
// inspect a project root on its own. Primarily driven by the explorer
// phase of consult_deepseek's two-phase flow. All paths are resolved
// relative to a configured root and validated against escape attempts.
package explorer

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	deepseek "github.com/cohesion-org/deepseek-go"
	gitignore "github.com/denormal/go-gitignore"
)

const (
	defaultMaxFileBytes   int64 = 100 * 1024 // 100 KiB cap on read_file
	defaultMaxSearchHits        = 200
	defaultMaxListEntries       = 1000
	defaultMaxSearchBytes int64 = 512 * 1024 // skip files larger than this in search
	defaultMaxTreeEntries       = 2000       // cap on project_tree lines
	defaultMaxTreeBytes   int64 = 64 * 1024  // cap on project_tree output size
)

// defaultTreeSkipDirs are directory names project_tree always prunes,
// independent of .gitignore — near-universal VCS/dependency/build/cache dirs
// whose contents would bloat the listing (and can break a session) without
// informing a review. Project-specific ignores layer on top via .gitignore.
var defaultTreeSkipDirs = map[string]bool{
	".git":          true,
	"node_modules":  true,
	"vendor":        true,
	"target":        true,
	"__pycache__":   true,
	".venv":         true,
	".tox":          true,
	".mypy_cache":   true,
	".pytest_cache": true,
	".idea":         true,
}

// Explorer is the sandbox + dispatcher for the project-inspection tools.
type Explorer struct {
	root           string // absolute, cleaned, symlinks resolved
	maxFileBytes   int64
	maxSearchHits  int
	maxListEntries int
	maxSearchBytes int64
	maxTreeEntries int
	maxTreeBytes   int64
}

// New constructs an Explorer rooted at root. The path is made absolute
// and dereferenced, and must point at an existing directory.
func New(root string) (*Explorer, error) {
	if root == "" {
		return nil, fmt.Errorf("explorer: root path is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("explorer: resolve root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("explorer: stat root: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("explorer: stat root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("explorer: root is not a directory: %s", resolved)
	}
	return &Explorer{
		root:           resolved,
		maxFileBytes:   defaultMaxFileBytes,
		maxSearchHits:  defaultMaxSearchHits,
		maxListEntries: defaultMaxListEntries,
		maxSearchBytes: defaultMaxSearchBytes,
		maxTreeEntries: defaultMaxTreeEntries,
		maxTreeBytes:   defaultMaxTreeBytes,
	}, nil
}

// Root returns the resolved sandbox root.
func (e *Explorer) Root() string { return e.root }

// resolve maps a user-supplied path to an absolute one inside the
// sandbox, rejecting any path that would escape the root either
// directly or via symlinks.
func (e *Explorer) resolve(path string) (string, error) {
	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) {
		cleaned = filepath.Join(e.root, cleaned)
	}
	cleaned = filepath.Clean(cleaned)

	if !pathInRoot(cleaned, e.root) {
		return "", fmt.Errorf("path escapes sandbox root: %s", path)
	}

	// Resolve symlinks if the target exists; if it doesn't, surface a
	// useful error rather than a misleading "escape".
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		if !pathInRoot(resolved, e.root) {
			return "", fmt.Errorf("path's symlink target is outside sandbox: %s", path)
		}
		return resolved, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("resolve %q: %w", path, err)
	}
	return cleaned, nil
}

func pathInRoot(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// ListDirectory returns a directory listing as a single text blob
// suitable for feeding back to the model.
func (e *Explorer) ListDirectory(path string) (string, error) {
	target, err := e.resolve(path)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return "", fmt.Errorf("list_directory %q: %w", path, err)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	rel, _ := filepath.Rel(e.root, target)
	if rel == "" {
		rel = "."
	}

	var b strings.Builder
	fmt.Fprintf(&b, "directory %s (%d entries)\n", rel, len(entries))
	shown := 0
	for _, ent := range entries {
		if shown >= e.maxListEntries {
			fmt.Fprintf(&b, "  ... (%d more entries truncated)\n", len(entries)-shown)
			break
		}
		name := ent.Name()
		if ent.IsDir() {
			fmt.Fprintf(&b, "  %s/\n", name)
		} else {
			info, err := ent.Info()
			if err != nil {
				fmt.Fprintf(&b, "  %s (stat error: %v)\n", name, err)
			} else {
				fmt.Fprintf(&b, "  %s  %d bytes\n", name, info.Size())
			}
		}
		shown++
	}
	return b.String(), nil
}

// ReadFile returns the file's contents (UTF-8 text expected). Files
// larger than maxFileBytes are truncated with a trailing marker.
func (e *Explorer) ReadFile(path string) (string, error) {
	target, err := e.resolve(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(target)
	if err != nil {
		return "", fmt.Errorf("read_file %q: %w", path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("read_file %q: is a directory", path)
	}

	data, err := readCapped(target, e.maxFileBytes)
	if err != nil {
		return "", fmt.Errorf("read_file %q: %w", path, err)
	}

	rel, _ := filepath.Rel(e.root, target)
	var b strings.Builder
	fmt.Fprintf(&b, "file %s (%d bytes)\n---\n", rel, info.Size())
	b.Write(data)
	if int64(len(data)) < info.Size() {
		fmt.Fprintf(&b, "\n--- (truncated; first %d of %d bytes shown)\n", len(data), info.Size())
	}
	return b.String(), nil
}

// readCapped returns up to max bytes from path, surfacing every read
// error io.ReadAll surfaces — partial-read failures must not be hidden.
func readCapped(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, max))
}

// SearchProject scans files under the sandbox root for lines matching
// the given Go regexp pattern. glob filters file basenames using
// filepath.Match semantics (e.g. "*.go"); an empty glob matches all.
func (e *Explorer) SearchProject(pattern, glob string) (string, error) {
	if pattern == "" {
		return "", fmt.Errorf("search_project: pattern is required")
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("search_project: bad pattern: %w", err)
	}
	if glob != "" {
		if _, err := filepath.Match(glob, "test"); err != nil {
			return "", fmt.Errorf("search_project: bad glob: %w", err)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "search pattern=%q glob=%q\n", pattern, glob)
	hits := 0
	skipped := 0
	truncated := false

	walkErr := filepath.WalkDir(e.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			skipped++
			return nil
		}
		if d.IsDir() {
			if defaultTreeSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if hits >= e.maxSearchHits {
			truncated = true
			return filepath.SkipAll
		}
		if glob != "" {
			ok, _ := filepath.Match(glob, d.Name())
			if !ok {
				return nil
			}
		}
		info, err := d.Info()
		if err != nil {
			skipped++
			return nil
		}
		if info.Size() > e.maxSearchBytes {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			skipped++
			return nil
		}
		rel, _ := filepath.Rel(e.root, p)
		for lineNo, line := range strings.Split(string(data), "\n") {
			if !re.MatchString(line) {
				continue
			}
			fmt.Fprintf(&b, "%s:%d: %s\n", rel, lineNo+1, strings.TrimSpace(line))
			hits++
			if hits >= e.maxSearchHits {
				truncated = true
				return filepath.SkipAll
			}
		}
		return nil
	})
	if walkErr != nil {
		return "", fmt.Errorf("search_project: walk: %w", walkErr)
	}
	fmt.Fprintf(&b, "(%d hit(s)%s", hits, ternary(truncated, ", capped", ""))
	if skipped > 0 {
		fmt.Fprintf(&b, "; %d file(s) unreadable", skipped)
	}
	fmt.Fprintf(&b, ")\n")
	return b.String(), nil
}

func ternary(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// ProjectTree returns a compact, flat listing of the files and directories
// under path (relative to the sandbox root; "." for the root itself) so the
// model can orient itself in one call instead of walking the tree directory
// by directory. The output is bounded: directories in defaultTreeSkipDirs
// and paths matched by the project's .gitignore are pruned, and the listing
// is capped by entry count and total bytes with a trailing truncation
// marker. Directories carry a trailing slash; files carry their size.
func (e *Explorer) ProjectTree(path string) (string, error) {
	root, err := e.resolve(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("project_tree %q: %w", path, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("project_tree %q: is not a directory", path)
	}

	// .gitignore matcher rooted at the sandbox root; honors nested
	// .gitignore files. A missing/unreadable ignore set is surfaced in the
	// output (not silently swallowed) but does not fail the walk — the
	// built-in skip-set still applies.
	var ignore gitignore.GitIgnore
	var ignoreNote string
	if gi, gerr := gitignore.NewRepository(e.root); gerr != nil {
		ignoreNote = fmt.Sprintf("(note: .gitignore not applied: %v)\n", gerr)
	} else {
		ignore = gi
	}

	rel, _ := filepath.Rel(e.root, root)
	if rel == "" {
		rel = "."
	}

	var b strings.Builder
	fmt.Fprintf(&b, "project tree %s\n", rel)
	b.WriteString(ignoreNote)

	entries := 0
	skipped := 0
	truncated := false

	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			skipped++
			return nil
		}
		if p == root {
			return nil // the tree root itself is the header, not an entry
		}
		isDir := d.IsDir()
		if isDir && defaultTreeSkipDirs[d.Name()] {
			return filepath.SkipDir
		}
		if ignore != nil {
			if m := ignore.Absolute(p, isDir); m != nil && m.Ignore() {
				if isDir {
					return filepath.SkipDir
				}
				return nil
			}
		}

		if entries >= e.maxTreeEntries || int64(b.Len()) >= e.maxTreeBytes {
			truncated = true
			return filepath.SkipAll
		}

		rp, _ := filepath.Rel(e.root, p)
		if isDir {
			fmt.Fprintf(&b, "%s/\n", rp)
		} else if fi, ierr := d.Info(); ierr == nil {
			fmt.Fprintf(&b, "%s  %d\n", rp, fi.Size())
		} else {
			fmt.Fprintf(&b, "%s\n", rp)
		}
		entries++
		return nil
	})
	if walkErr != nil {
		return "", fmt.Errorf("project_tree: walk: %w", walkErr)
	}

	fmt.Fprintf(&b, "(%d entries", entries)
	if truncated {
		fmt.Fprintf(&b, "; truncated at cap (max %d entries / %d bytes)", e.maxTreeEntries, e.maxTreeBytes)
	}
	if skipped > 0 {
		fmt.Fprintf(&b, "; %d unreadable", skipped)
	}
	b.WriteString(")\n")
	return b.String(), nil
}

// ToolDefinitions returns the DeepSeek tool schemas for the exploration
// tools, suitable for inclusion in a ChatCompletionRequest.
func (e *Explorer) ToolDefinitions() []deepseek.Tool {
	stringProp := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}
	return []deepseek.Tool{
		{
			Type: "function",
			Function: deepseek.Function{
				Name:        "list_directory",
				Description: "List entries in a directory under the project root. Returns directory listing as text.",
				Parameters: &deepseek.FunctionParameters{
					Type: "object",
					Properties: map[string]any{
						"path": stringProp("Directory path, relative to the project root. Use '.' for the root itself."),
					},
					Required: []string{"path"},
				},
			},
		},
		{
			Type: "function",
			Function: deepseek.Function{
				Name:        "read_file",
				Description: "Read the contents of a file under the project root. Large files are truncated; see the trailing marker.",
				Parameters: &deepseek.FunctionParameters{
					Type: "object",
					Properties: map[string]any{
						"path": stringProp("File path, relative to the project root."),
					},
					Required: []string{"path"},
				},
			},
		},
		{
			Type: "function",
			Function: deepseek.Function{
				Name:        "search_project",
				Description: "Search for lines matching a Go regexp across files under the project root. Returns matches grouped by file with line numbers. Skips dependency/build dirs (.git, node_modules, vendor, target, __pycache__, etc.).",
				Parameters: &deepseek.FunctionParameters{
					Type: "object",
					Properties: map[string]any{
						"pattern": stringProp("A Go regexp (RE2) pattern to search for line by line."),
						"glob":    stringProp("Optional basename glob like '*.go' to restrict the file set. Empty means all files."),
					},
					Required: []string{"pattern"},
				},
			},
		},
		{
			Type: "function",
			Function: deepseek.Function{
				Name:        "project_tree",
				Description: "List the project's file layout in one call to orient yourself before reading files. Returns relative paths (directories end in '/', files show their byte size). Honors .gitignore and skips dependency/build dirs (.git, node_modules, vendor, target, __pycache__, etc.). Output is capped, so very large trees are truncated with a marker.",
				Parameters: &deepseek.FunctionParameters{
					Type: "object",
					Properties: map[string]any{
						"path": stringProp("Directory to list, relative to the project root. Use '.' for the whole project."),
					},
					Required: []string{"path"},
				},
			},
		},
	}
}

// Dispatch executes the tool named name with the JSON arguments string
// argsJSON. The return value is a single text blob suitable for feeding
// back to the model as a tool result message. Errors are formatted into
// the return text rather than returned separately so the model receives
// a self-describing result.
func (e *Explorer) Dispatch(name string, argsJSON string) string {
	switch name {
	case "list_directory":
		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments for list_directory: %v", err)
		}
		out, err := e.ListDirectory(args.Path)
		if err != nil {
			return "error: " + err.Error()
		}
		return out
	case "read_file":
		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments for read_file: %v", err)
		}
		out, err := e.ReadFile(args.Path)
		if err != nil {
			return "error: " + err.Error()
		}
		return out
	case "search_project":
		var args struct {
			Pattern string `json:"pattern"`
			Glob    string `json:"glob"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments for search_project: %v", err)
		}
		out, err := e.SearchProject(args.Pattern, args.Glob)
		if err != nil {
			return "error: " + err.Error()
		}
		return out
	case "project_tree":
		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments for project_tree: %v", err)
		}
		if args.Path == "" {
			args.Path = "."
		}
		out, err := e.ProjectTree(args.Path)
		if err != nil {
			return "error: " + err.Error()
		}
		return out
	default:
		return fmt.Sprintf("error: unknown tool %q", name)
	}
}
