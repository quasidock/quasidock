package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"
)

func containerValueOption(arg string) string {
	if len(arg) < 2 || arg[0] != '-' {
		return arg
	}
	var name string
	switch arg[1] {
	case 'p':
		name = "publish"
	case 'v':
		name = "volume"
	case 'e':
		name = "env"
	case 'w':
		name = "workdir"
	case 'u':
		name = "user"
	case 'l':
		name = "label"
	default:
		return arg
	}
	if len(arg) == 2 {
		return "--" + name
	}
	return "--" + name + "=" + strings.TrimPrefix(arg[2:], "=")
}

func validContainerKeyValue(entry string) bool {
	key, _, _ := strings.Cut(entry, "=")
	return key != "" && strings.IndexFunc(key, unicode.IsSpace) == -1 && !strings.ContainsRune(entry, 0)
}

func readContainerOptionFiles(option string, paths []string) ([]string, error) {
	var entries []string
	for _, path := range paths {
		fileEntries, err := readContainerOptionFile(option, path)
		if err != nil {
			return nil, err
		}
		entries = append(entries, fileEntries...)
	}
	return entries, nil
}

func readContainerOptionFile(option, path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("--%s: %w", option, err)
	}
	defer file.Close()

	var entries []string
	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()
		if lineNumber == 1 {
			line = strings.TrimPrefix(line, "\ufeff")
		}
		if !utf8.ValidString(line) {
			return nil, fmt.Errorf("--%s %q line %d: expected UTF-8", option, path, lineNumber)
		}
		line = strings.TrimLeftFunc(line, unicode.IsSpace)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !validContainerKeyValue(line) {
			return nil, fmt.Errorf("--%s %q line %d: expected KEY[=VALUE] with a nonempty key and no whitespace in the key or NUL characters", option, path, lineNumber)
		}
		entries = append(entries, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("--%s %q line %d: %w", option, path, lineNumber+1, err)
	}
	return entries, nil
}
