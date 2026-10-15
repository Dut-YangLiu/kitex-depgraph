package scan

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// goMod is the subset of go.mod the scanner needs.
type goMod struct {
	Module   string
	Go       string
	Requires map[string]string // module path -> version
	Replaces []replaceDirective
}

type replaceDirective struct {
	OldPath    string
	OldVersion string // empty: applies to every version
	NewPath    string // module path, or a filesystem path for local replacements
	NewVersion string // empty for local replacements
}

func (r replaceDirective) isLocal() bool {
	return r.NewVersion == "" && (strings.HasPrefix(r.NewPath, "./") || strings.HasPrefix(r.NewPath, "../") ||
		r.NewPath == "." || r.NewPath == ".." || filepath.IsAbs(r.NewPath))
}

// parseGoMod parses go.mod without external dependencies. It understands the
// module, go, require and replace directives, both single-line and block form.
func parseGoMod(data []byte) (*goMod, error) {
	m := &goMod{Requires: map[string]string{}}
	block := ""
	for _, raw := range strings.Split(string(data), "\n") {
		line := raw
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if block != "" {
			if line == ")" {
				block = ""
				continue
			}
			m.directive(block, line)
			continue
		}
		fields := strings.Fields(line)
		verb := fields[0]
		rest := strings.TrimSpace(strings.TrimPrefix(line, verb))
		if rest == "(" {
			block = verb
			continue
		}
		m.directive(verb, rest)
	}
	if m.Module == "" {
		return nil, errors.New("go.mod 缺少 module 声明")
	}
	return m, nil
}

func (m *goMod) directive(verb, args string) {
	switch verb {
	case "module":
		m.Module = unquoteModPath(args)
	case "go":
		m.Go = strings.TrimSpace(args)
	case "require":
		f := strings.Fields(args)
		if len(f) >= 2 {
			m.Requires[unquoteModPath(f[0])] = unquoteModPath(f[1])
		}
	case "replace":
		parts := strings.SplitN(args, "=>", 2)
		if len(parts) != 2 {
			return
		}
		left, right := strings.Fields(parts[0]), strings.Fields(parts[1])
		if len(left) == 0 || len(right) == 0 {
			return
		}
		r := replaceDirective{OldPath: unquoteModPath(left[0]), NewPath: unquoteModPath(right[0])}
		if len(left) > 1 {
			r.OldVersion = unquoteModPath(left[1])
		}
		if len(right) > 1 {
			r.NewVersion = unquoteModPath(right[1])
		}
		m.Replaces = append(m.Replaces, r)
	}
}

func unquoteModPath(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '`') {
		if u, err := strconv.Unquote(s); err == nil {
			return u
		}
	}
	return s
}

// escapeModPath applies the module cache case encoding: every upper-case
// letter becomes '!' followed by its lower-case form.
func escapeModPath(s string) string {
	var b strings.Builder
	for _, r := range s {
		if 'A' <= r && r <= 'Z' {
			b.WriteByte('!')
			b.WriteRune(r + ('a' - 'A'))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// findModCache locates the Go module cache.
func findModCache() string {
	if v := os.Getenv("GOMODCACHE"); v != "" {
		return v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "go", "env", "GOMODCACHE").Output(); err == nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			return s
		}
	}
	gopath := os.Getenv("GOPATH")
	if gopath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		gopath = filepath.Join(home, "go")
	}
	return filepath.Join(filepath.SplitList(gopath)[0], "pkg", "mod")
}
