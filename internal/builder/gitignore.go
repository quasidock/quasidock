package builder

import (
	"strings"
)

type GitignorePattern struct {
	Glob     string
	Negate   bool
	DirOnly  bool
	Anchored bool
}

func (p GitignorePattern) Matches(rel string, isDir bool) bool {
	if p.DirOnly && !isDir {
		return false
	}
	if p.Anchored {
		return Wildmatch(p.Glob, rel)
	}
	name := rel
	if idx := strings.LastIndexByte(rel, '/'); idx >= 0 {
		name = rel[idx+1:]
	}
	return Wildmatch(p.Glob, name)
}

func ParseGitignore(bytes []byte) []GitignorePattern {
	text := strings.TrimPrefix(string(bytes), "\xEF\xBB\xBF")
	var patterns []GitignorePattern
	lines := strings.Split(text, "\n")
	for _, raw := range lines {
		line := trimTrailingSpaces(strings.TrimRight(raw, "\r"))
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		p := GitignorePattern{}
		if line[0] == '!' {
			p.Negate = true
			line = line[1:]
		}
		if strings.HasSuffix(line, "/") {
			p.DirOnly = true
			line = line[:len(line)-1]
		}
		if idx := strings.IndexByte(line, '/'); idx >= 0 {
			p.Anchored = true
			if idx == 0 {
				line = line[1:]
			}
		}
		if len(line) == 0 {
			continue
		}
		p.Glob = line
		patterns = append(patterns, p)
	}
	return patterns
}

func trimTrailingSpaces(line string) string {
	end := -1
	i := 0
	for i < len(line) {
		switch line[i] {
		case ' ':
			if end == -1 {
				end = i
			}
			i++
		case '\\':
			i += 2
			end = -1
		default:
			i++
			end = -1
		}
	}
	if end != -1 {
		return line[:end]
	}
	return line
}

type GitignoreRules struct {
	levels []gitignoreLevel
}

type gitignoreLevel struct {
	base     string
	patterns []GitignorePattern
}

func (r *GitignoreRules) Push(base string, patterns []GitignorePattern) {
	r.levels = append(r.levels, gitignoreLevel{base: base, patterns: patterns})
}

func (r *GitignoreRules) Pop() {
	if len(r.levels) > 0 {
		r.levels = r.levels[:len(r.levels)-1]
	}
}

func (r *GitignoreRules) Ignored(path string, isDir bool) bool {
	for i := len(r.levels) - 1; i >= 0; i-- {
		level := r.levels[i]
		rel, ok := below(path, level.base)
		if !ok {
			continue
		}
		for j := len(level.patterns) - 1; j >= 0; j-- {
			p := level.patterns[j]
			if p.Matches(rel, isDir) {
				return !p.Negate
			}
		}
	}
	return false
}

func below(path, base string) (string, bool) {
	if base == "" {
		return path, true
	}
	if len(path) <= len(base) || path[len(base)] != '/' || !strings.EqualFold(path[:len(base)], base) {
		return "", false
	}
	return path[len(base)+1:], true
}

func Wildmatch(pattern, text string) bool {
	p := 0
	t := 0
	for p < len(pattern) {
		switch pattern[p] {
		case '*':
			end := p
			for end < len(pattern) && pattern[end] == '*' {
				end++
			}
			rest := pattern[end:]
			if end-p >= 2 && (p == 0 || pattern[p-1] == '/') && (len(rest) == 0 || rest[0] == '/') {
				if len(rest) == 0 {
					return true
				}
				k := t
				for {
					if Wildmatch(rest[1:], text[k:]) {
						return true
					}
					nextSlash := strings.IndexByte(text[k:], '/')
					if nextSlash < 0 {
						return false
					}
					k += nextSlash + 1
				}
			}
			k := t
			for {
				if Wildmatch(rest, text[k:]) {
					return true
				}
				if k == len(text) || text[k] == '/' {
					return false
				}
				k++
			}
		case '?':
			if t == len(text) || text[t] == '/' {
				return false
			}
			p++
			t++
		case '[':
			if t == len(text) || text[t] == '/' {
				return false
			}
			matched, length, ok := matchClass(pattern[p:], text[t])
			if !ok || !matched {
				return false
			}
			p += length
			t++
		default:
			c := pattern[p]
			want := c
			if c == '\\' && p+1 < len(pattern) {
				p++
				want = pattern[p]
			}
			if t == len(text) || toLowerByte(text[t]) != toLowerByte(want) {
				return false
			}
			p++
			t++
		}
	}
	return t == len(text)
}

func matchClass(pattern string, c byte) (bool, int, bool) {
	i := 1
	negate := false
	if i < len(pattern) && (pattern[i] == '!' || pattern[i] == '^') {
		negate = true
		i++
	}
	matched := false
	first := i
	for i < len(pattern) {
		lo := pattern[i]
		if lo == ']' && i > first {
			return matched != negate, i + 1, true
		}
		if lo == '[' && i+1 < len(pattern) && pattern[i+1] == ':' {
			closeIdx := strings.Index(pattern[i+2:], ":]")
			if closeIdx < 0 {
				return false, 0, false
			}
			name := pattern[i+2 : i+2+closeIdx]
			if matchPosixClass(name, c) {
				matched = true
			}
			i += 2 + closeIdx + 2
			continue
		}
		if i+2 < len(pattern) && pattern[i+1] == '-' && pattern[i+2] != ']' {
			hi := pattern[i+2]
			if toLowerByte(c) >= toLowerByte(lo) && toLowerByte(c) <= toLowerByte(hi) {
				matched = true
			}
			i += 3
			continue
		}
		if toLowerByte(c) == toLowerByte(lo) {
			matched = true
		}
		i++
	}
	return false, 0, false
}

func matchPosixClass(name string, c byte) bool {
	switch name {
	case "alnum":
		return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
	case "alpha":
		return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
	case "digit":
		return c >= '0' && c <= '9'
	case "lower":
		return c >= 'a' && c <= 'z'
	case "upper":
		return c >= 'A' && c <= 'Z'
	case "space":
		return c == ' ' || c == '\t' || c == '\r' || c == '\n'
	}
	return false
}

func toLowerByte(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}
