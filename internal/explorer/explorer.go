// Package explorer provides sandboxed read-only filesystem tools that
// dpal exposes to DeepSeek V4 via function calling so the model can
// inspect a project root on its own. Primarily driven by the explorer
// phase of consult_deepseek's two-phase flow. All paths are resolved
// relative to a configured root and validated against escape attempts.
package explorer

import (
	"bufio"
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
	defaultMaxBatchFiles        = 30         // cap on files per read_files call
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
	maxBatchFiles  int
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
		maxBatchFiles:  defaultMaxBatchFiles,
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
	b.WriteString(numberLines(data))
	if int64(len(data)) < info.Size() {
		fmt.Fprintf(&b, "--- (truncated; first %d of %d bytes shown)\n", len(data), info.Size())
	}
	return b.String(), nil
}

// ReadFiles reads several files in a single tool call and concatenates
// their blocks in request order. The tool-iteration cap counts round-trips,
// not files, so loading a working set this way (a diff's touched files plus
// the helpers they call) stretches the budget far further than one read_file
// per turn — the loading pattern DeepSeek's explorer otherwise defaults to.
//
// Each file is read from the top under the same head byte cap as ReadFile.
// A path that fails to read (missing, a directory, a sandbox escape) is
// reported INLINE as its own error block and the rest still load: a single
// bad path in a batch the model meant as a unit should not sink the whole
// call. The error is surfaced in the returned text, not swallowed — the
// model sees exactly which path failed and why.
func (e *Explorer) ReadFiles(paths []string) (string, error) {
	if len(paths) == 0 {
		return "", fmt.Errorf("read_files: no paths given")
	}
	if len(paths) > e.maxBatchFiles {
		return "", fmt.Errorf("read_files: %d paths exceeds the %d-file batch limit; split into multiple calls", len(paths), e.maxBatchFiles)
	}
	var b strings.Builder
	for i, p := range paths {
		if i > 0 {
			b.WriteByte('\n') // blank line between blocks
		}
		out, err := e.ReadFile(p)
		if err != nil {
			// Mirror ReadFile's header shape so the block is self-describing.
			fmt.Fprintf(&b, "file %s\n---\nerror: %v\n", p, err)
			continue
		}
		b.WriteString(out)
	}
	return b.String(), nil
}

// numberLines prefixes each line of data with its 1-based line number in
// cat -n style ("%6d\t"), so a reading model can cite real line numbers
// rather than counting by eye. A trailing newline does not produce a
// phantom final line. Returns "" for empty input.
func numberLines(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	s := string(data)
	lines := strings.Split(s, "\n")
	// strings.Split on a trailing newline yields a final "" element;
	// drop it so the EOF newline isn't numbered as its own line.
	if strings.HasSuffix(s, "\n") {
		lines = lines[:len(lines)-1]
	}
	var b strings.Builder
	for i, ln := range lines {
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, ln)
	}
	return b.String()
}

// ReadFileRange reads lines [startLine, endLine] (1-based, inclusive) of
// the file at path. startLine <= 0 defaults to 1; endLine <= 0 reads to
// EOF. Unlike ReadFile, which stops at the head byte cap, this streams
// line by line and can reach lines deep in a large file — it is how the
// curator (and the synthesizer) excerpt a precise window the head read
// can't reach. The returned window is still capped at maxFileBytes and
// truncated with a marker if it overflows. Each line keeps its true,
// absolute line number.
func (e *Explorer) ReadFileRange(path string, startLine, endLine int) (string, error) {
	if startLine <= 0 {
		startLine = 1
	}
	if endLine > 0 && endLine < startLine {
		return "", fmt.Errorf("read_file %q: end_line %d is before start_line %d", path, endLine, startLine)
	}

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

	f, err := os.Open(target)
	if err != nil {
		return "", fmt.Errorf("read_file %q: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// Allow lines far longer than the 64 KiB default token so a minified
	// or generated line doesn't abort the scan.
	sc.Buffer(make([]byte, 0, 64*1024), int(e.maxSearchBytes))

	var body strings.Builder
	var bodyBytes int64
	lineNo := 0
	lastWritten := 0 // highest line number actually included in body
	truncated := false
	for sc.Scan() {
		lineNo++
		if lineNo < startLine {
			continue
		}
		if endLine > 0 && lineNo > endLine {
			break
		}
		line := sc.Text()
		entry := fmt.Sprintf("%6d\t%s\n", lineNo, line)
		if bodyBytes+int64(len(entry)) > e.maxFileBytes {
			truncated = true
			break
		}
		body.WriteString(entry)
		bodyBytes += int64(len(entry))
		lastWritten = lineNo
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read_file %q: %w", path, err)
	}

	var b strings.Builder
	if body.Len() == 0 {
		if truncated {
			// The first in-range line was itself larger than the byte cap,
			// so nothing could be shown. Say so plainly rather than claim an
			// empty "lines N-N" window.
			fmt.Fprintf(&b, "file %s (line %d exceeds the %d-byte read cap; too large to show)\n---\n", e.rel(target), startLine, e.maxFileBytes)
		} else {
			// Range began past EOF. lineNo is the file's total line count.
			fmt.Fprintf(&b, "file %s (no lines in range; file has %d lines)\n---\n", e.rel(target), lineNo)
		}
		return b.String(), nil
	}
	// Header's upper bound is the last line actually included, not the line
	// that overflowed the cap (which is excluded).
	fmt.Fprintf(&b, "file %s (lines %d-%d)\n---\n", e.rel(target), startLine, lastWritten)
	b.WriteString(body.String())
	if truncated {
		fmt.Fprintf(&b, "--- (truncated at %d bytes; request a narrower range)\n", e.maxFileBytes)
	}
	return b.String(), nil
}

// rel returns target's path relative to the sandbox root for display.
func (e *Explorer) rel(target string) string {
	r, err := filepath.Rel(e.root, target)
	if err != nil {
		return target
	}
	return r
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
	intProp := func(desc string) map[string]any {
		return map[string]any{"type": "integer", "description": desc}
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
				Description: "Read a file under the project root. Each line is prefixed with its line number (cat -n style); cite those numbers directly. With no range, reads from the top and large files are truncated (see the trailing marker). Pass start_line/end_line to read a precise window — this reaches lines past the head cap, so it's how you excerpt deep into a large file.",
				Parameters: &deepseek.FunctionParameters{
					Type: "object",
					Properties: map[string]any{
						"path":       stringProp("File path, relative to the project root."),
						"start_line": intProp("Optional 1-based first line to read. Omit to start at the top."),
						"end_line":   intProp("Optional 1-based last line to read (inclusive). Omit to read to end of file."),
					},
					Required: []string{"path"},
				},
			},
		},
		{
			Type: "function",
			Function: deepseek.Function{
				Name:        "read_files",
				Description: "Read SEVERAL files in one call — much cheaper than one read_file per turn, because the tool-call budget is counted in round-trips, not files. Reach for this to pull a whole working set at once (a diff's touched files plus the helpers they call). Each file is read from the top under the same size cap as read_file; for a precise deep window in a single file, use read_file with start_line/end_line instead. A path that can't be read is reported inline and the remaining files still load.",
				Parameters: &deepseek.FunctionParameters{
					Type: "object",
					Properties: map[string]any{
						"paths": map[string]any{
							"type":        "array",
							"items":       map[string]any{"type": "string"},
							"description": "File paths, each relative to the project root, to read in one batch.",
						},
					},
					Required: []string{"paths"},
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
			Path      string `json:"path"`
			StartLine int    `json:"start_line"`
			EndLine   int    `json:"end_line"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments for read_file: %v", err)
		}
		// A line range (either bound) routes to the streaming ranged read,
		// which can reach past the head byte cap; otherwise read from the top.
		var (
			out string
			err error
		)
		if args.StartLine > 0 || args.EndLine > 0 {
			out, err = e.ReadFileRange(args.Path, args.StartLine, args.EndLine)
		} else {
			out, err = e.ReadFile(args.Path)
		}
		if err != nil {
			return "error: " + err.Error()
		}
		return out
	case "read_files":
		var args struct {
			Paths []string `json:"paths"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("error: invalid arguments for read_files: %v", err)
		}
		out, err := e.ReadFiles(args.Paths)
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
