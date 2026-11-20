package dockerfile

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/recipe"
)

func TestPHPBases(t *testing.T) {
	for _, tc := range []struct{ base, version string }{
		{"php", ""}, {"php:latest", ""}, {"php:cli", ""}, {"php:cli-alpine", ""},
		{"php:8", "8"}, {"php:8.5.12", "8.5.12"}, {"php:8.5-cli", "8.5"},
		{"php:8.5-cli-bookworm", "8.5"}, {"php:8.5-alpine3.23", "8.5"},
		{"docker.io/library/php:8.5-cli-trixie", "8.5"},
	} {
		t.Run(tc.base, func(t *testing.T) {
			resolver := &fakeResolver{}
			result, err := Convert([]byte("FROM "+tc.base), options(t), resolver)
			if err != nil {
				t.Fatalf("%v: %v", err, result.Diagnostics)
			}
			if !reflect.DeepEqual(resolver.calls, []string{"php:" + tc.version + ":", "busybox::"}) || !reflect.DeepEqual(result.Recipe.Entrypoint, recipe.Entrypoint{"${php}/php.exe"}) {
				t.Fatalf("calls %v, recipe %+v", resolver.calls, result.Recipe)
			}
		})
	}
}

func TestPHPCommandsAndStartupPaths(t *testing.T) {
	r := convertOK(t, `FROM php:8.5-cli-bookworm
WORKDIR /app
COPY . .
RUN /usr/local/bin/php -c /app/php.ini -d memory_limit=128M -l /app/main.php
RUN php -r 'echo "/literal";' -- /literal '$VALUE' 'a*b'
RUN ["php", "-f", "/app/main.php", "--", "/literal", "$VALUE", "a*b"]
ENTRYPOINT ["php", "-c/app/php.ini", "-d", "memory_limit=128M", "-S", "0.0.0.0:8080", "-t", "/app/public", "/app/router.php"]
CMD ["/literal", "$VALUE", "a*b"]`)
	if !reflect.DeepEqual(r.Entrypoint, recipe.Entrypoint{"${php}/php.exe", "-c${app}/app/php.ini", "-d", "memory_limit=128M", "-S", "0.0.0.0:8080", "-t", "${app}/app/public", "${app}/app/router.php"}) || !reflect.DeepEqual(r.Cmd, []string{"/literal", "$VALUE", "a*b"}) {
		t.Fatalf("startup paths or literal arguments changed: %v %v", r.Entrypoint, r.Cmd)
	}
	build := fmt.Sprint(r.Stages)
	for _, want := range []string{`php '-c' "$PREFIX"/'app/php.ini' '-d' 'memory_limit=128M' '-l' "$PREFIX"/'app/main.php'`, `'--' '/literal' '$VALUE' 'a*b'`} {
		if !strings.Contains(build, want) {
			t.Fatalf("missing %q in %s", want, build)
		}
	}
	r = convertOK(t, "FROM php\nWORKDIR /app\nCMD php -S 127.0.0.1:8080 -t /app/public")
	if r.From["busybox"] == "" || !strings.Contains(r.Entrypoint[3], "${php}/php.exe") || !strings.Contains(r.Entrypoint[3], "${app}/app/public") {
		t.Fatal(r.Entrypoint)
	}
}

func TestPHPArgumentPaths(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want map[int]string
	}{
		{[]string{"-r", `echo "/literal";`, "/literal"}, map[int]string{}},
		{[]string{"-r", "echo 1;", "-c", "/php.ini", "--", "/literal"}, map[int]string{3: ""}},
		{[]string{"-f", "/main.php", "-c/php.ini", "/literal"}, map[int]string{1: "", 2: "-c"}},
		{[]string{"main.php", "-c", "/literal"}, map[int]string{0: ""}},
		{[]string{"-c", "/php.ini", "-d", "include_path=/literal", "/main.php", "/literal"}, map[int]string{1: "", 4: ""}},
		{[]string{"--php-ini=/php.ini", "--file=/main.php", "--", "/literal"}, map[int]string{0: "--php-ini=", 1: "--file="}},
		{[]string{"-l", "/a.php", "/b.php"}, map[int]string{1: "", 2: ""}},
		{[]string{"-B", "echo 1;", "-F/a.php", "-E", "echo 2;"}, map[int]string{2: "-F"}},
		{[]string{"--server=0.0.0.0:8080", "--docroot=/public", "/router.php"}, map[int]string{1: "--docroot=", 2: ""}},
	} {
		words := make([]commandWord, len(tc.args))
		for i, arg := range tc.args {
			words[i] = literalWord(arg)
		}
		got, err := phpArgumentPaths(words)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%v: paths %v, error %v; want %v", tc.args, got, err, tc.want)
		}
	}
}

func TestPHPAptAndOverrides(t *testing.T) {
	opts := options(t)
	opts.Tools = map[string]string{"php": "php:8.5.12-1"}
	resolver := &fakeResolver{}
	result, err := Convert([]byte("FROM debian:bookworm\nRUN apt install -y php php-cli\nRUN php -v\nCMD [\"php\",\"/main.php\"]"), opts, resolver)
	if err != nil || !reflect.DeepEqual(resolver.calls, []string{"php::php:8.5.12-1", "busybox::"}) || result.Recipe.Cmd[0] != "${app}/main.php" {
		t.Fatalf("%v: %v; calls %v", err, result.Diagnostics, resolver.calls)
	}
	opts.Tools = nil
	opts.Base = "php:8.5.12-1"
	resolver.calls = nil
	result, err = Convert([]byte("FROM php:8.4-cli"), opts, resolver)
	if err != nil || resolver.calls[0] != "php:8.4:php:8.5.12-1" {
		t.Fatalf("%v: %v; calls %v", err, result.Diagnostics, resolver.calls)
	}
}

func TestComposerStageIsolationAndInheritance(t *testing.T) {
	text := `FROM composer:2 AS vendor
WORKDIR /app
COPY . .
RUN composer install --no-dev --no-interaction
FROM vendor AS autoload
RUN composer --working-dir=/app dumpautoload --optimize
FROM php:8.5-cli AS runtime
WORKDIR /app
COPY --from=autoload /app /app
CMD ["php", "/app/main.php"]`
	resolver := &fakeResolver{}
	result, err := Convert([]byte(text), options(t), resolver)
	if err != nil {
		t.Fatalf("%v: %v", err, result.Diagnostics)
	}
	r := result.Recipe
	if len(r.Builds) != 2 || len(r.From) != 1 || r.From["php"] == "" || !reflect.DeepEqual(resolver.calls, []string{"composer:2:", "php::", "busybox::", "php:8.5:"}) {
		t.Fatalf("calls %v, recipe %+v", resolver.calls, r)
	}
	if r.Builds[0].Stages[0].Name != "composer" || !strings.Contains(*r.Builds[0].Stages[0].Sources[0].Image, "/composer@") || *r.Builds[0].Stages[0].Sources[0].Dest != ".composer" || r.Builds[0].From["composer"] != "" || r.Builds[1].Stages[0].Sources[0].Build == nil {
		t.Fatalf("Composer must be imported once and inherited as app files: %+v", r.Builds)
	}
	if !strings.Contains(fmt.Sprint(r.Builds[0].Stages), `php "$PREFIX"/'.composer/composer.phar' 'install'`) || !strings.Contains(fmt.Sprint(r.Builds[1].Stages), `--working-dir="$PREFIX"/'app'`) {
		t.Fatal(r.Builds)
	}
	r = convertOK(t, "FROM composer:2")
	if !reflect.DeepEqual(r.Entrypoint, recipe.Entrypoint{"${php}/php.exe", "${app}/.composer/composer.phar"}) {
		t.Fatal(r.Entrypoint)
	}
	r = convertOK(t, "FROM php:8.5\nRUN apt install composer\nRUN composer install\nCMD [\"php\",\"main.php\"]")
	if len(r.From) != 1 || r.Stages[0].Name != "composer" {
		t.Fatalf("Composer must use the selected PHP: %+v", r)
	}
}

func TestUnsupportedPHPWorkflows(t *testing.T) {
	for _, text := range []string{
		"FROM php:8.5-apache-bookworm", "FROM php:fpm", "FROM php:8.5-zts-alpine",
		"FROM php\nRUN docker-php-ext-install gd", "FROM php\nCMD [\"apache2-foreground\"]",
		"FROM php\nRUN php -c", "FROM php\nCMD [\"php\",\"--file\"]",
		"FROM php\nRUN php --unknown /file", "FROM php:8.5-alpine\nRUN apt install composer",
		"FROM php\nRUN composer install", "FROM composer\nRUN composer global require example/tool",
		"FROM composer\nRUN composer self-update", "FROM composer\nCMD [\"composer\",\"config\",\"--global\",\"foo\",\"bar\"]",
		"FROM node\nCMD [\"php\"]", "FROM php\nCMD [\"php\",\"${php}\"]",
		"FROM php\nRUN apt install python3\nRUN docker-php-ext-install gd",
	} {
		t.Run(text, func(t *testing.T) {
			resolver := &fakeResolver{}
			result, err := Convert([]byte(text), options(t), resolver)
			if err == nil || result.Recipe != nil || len(resolver.calls) != 0 {
				t.Fatalf("unsupported PHP workflow accepted or resolved dependencies: %v %v", err, result.Diagnostics)
			}
		})
	}
}
