package dockerfile

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/recipe"
	"mvdan.cc/sh/v3/syntax"
)

func buildCommands(r *recipe.Recipe) string {
	var commands []string
	for _, step := range r.Stages {
		commands = append(commands, step.Build...)
	}
	return strings.Join(commands, "\n")
}

func TestRunPortableCommandsAndShell(t *testing.T) {
	r := convertOK(t, `FROM node
WORKDIR /app
RUN mkdir -p /out && printf '%s\n' /literal | grep -e /literal > /out/result; false || true
RUN touch /out/one /out/two && cat /out/* | sort | uniq | head -n 2 | tail -n 1 | wc -l | tee /out/count
RUN node -e "console.log('/literal')" 2>/out/errors 2>&1 >>/out/result < /out/result
RUN echo done >/dev/null
CMD ["node","app.js"]`)
	commands := buildCommands(r)
	for _, want := range []string{
		`cd "$PREFIX"/'app' && {`,
		`printf '%s\n' '/literal' | grep '-e' '/literal' >"$PREFIX"/'out/result'; false || true`,
		`cat "$PREFIX"/'out/'*`,
		`tee "$PREFIX"/'out/count'`,
		quote("console.log('/literal')"),
		`2>"$PREFIX"/'out/errors' 2>&1 >>"$PREFIX"/'out/result' <"$PREFIX"/'out/result'`,
		`echo 'done' >NUL`,
	} {
		if !strings.Contains(commands, want) {
			t.Fatalf("missing %s in\n%s", want, commands)
		}
	}
	for _, step := range r.Stages {
		for _, command := range step.Build {
			if _, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(command), "generated"); err != nil {
				t.Fatalf("invalid generated shell: %v\n%s", err, command)
			}
		}
	}
}

func TestRunExecArgumentsRemainLiteral(t *testing.T) {
	args := []string{"echo", "$VALUE", "a*b", "[ab]", "a; false", "a|b", "one two", "'quoted'", "/literal"}
	encoded, _ := json.Marshal(args)
	r := convertOK(t, "FROM node\nENV VALUE=expanded\nRUN "+string(encoded)+"\nCMD [\"node\"]")
	command := r.Stages[0].Build[0]
	for _, arg := range args[1:] {
		if !strings.Contains(command, " "+quote(arg)) {
			t.Fatalf("argument %q lost its literal representation: %s", arg, command)
		}
	}
}

func TestRunPythonAndPackagePatterns(t *testing.T) {
	r := convertOK(t, `FROM python:3.14
WORKDIR /app
RUN pip install flask==3.0.*
RUN python -B -mpip install --no-index --find-links=/wheels -r/requirements.txt
RUN python -c "print('/text')" /opaque
RUN python /app/build.py /opaque
CMD ["flask","run","--host","0.0.0.0","--port","8501"]`)
	commands := buildCommands(r)
	for _, want := range []string{"flask==3.0.*", "--only-binary=:all:", `--target' "$PREFIX"/'.python'`, `--find-links="$PREFIX"/'wheels'`, `-r"$PREFIX"/'requirements.txt'`, `python "$PREFIX"/'app/build.py' '/opaque'`} {
		if !strings.Contains(commands, want) {
			t.Fatalf("missing %s in %s", want, commands)
		}
	}
	if !reflect.DeepEqual(r.Entrypoint, recipe.Entrypoint{"${python}/python.exe", "-c", consoleDispatch, "flask"}) || !reflect.DeepEqual(r.Cmd, []string{"run", "--host", "0.0.0.0", "--port", "8501"}) {
		t.Fatalf("console startup: %+v %+v", r.Entrypoint, r.Cmd)
	}
	last := r.Stages[len(r.Stages)-1]
	if !strings.Contains(last.Build[0], quote(consoleLookup)+" 'flask'") || last.Network || last.Env["PYTHONPATH"] != "${app}/.python" {
		t.Fatalf("missing final metadata validation: %+v", last)
	}
}

func TestRunWorkingDirectoriesAndExecutableOperations(t *testing.T) {
	r := convertOK(t, `FROM golang AS build
WORKDIR /src
RUN mkdir -p /out /package && cd /out && go build -o server /src
RUN /out/server --check /opaque
RUN cp /out/server /package/copied && mv /package/copied /package/final && rm /out/server
FROM scratch
COPY --from=build /package/final /server
CMD ["/server"]`)
	unit := r.Builds[0]
	if !reflect.DeepEqual(unit.Executables, []string{"package/final"}) || r.Entrypoint[0] != "${app}/server.exe" {
		t.Fatalf("lost executable operations: %+v %+v", unit.Executables, r.Entrypoint)
	}
	commands := buildCommands(&recipe.Recipe{Stages: unit.Stages})
	for _, want := range []string{`cd "$PREFIX"/'out' && go 'build' '-o' "$PREFIX"/'out/server.exe'`, `"$PREFIX"/'out/server.exe' '--check' '/opaque'`, `cp "$PREFIX"/'out/server.exe' "$PREFIX"/'package/copied.exe'`, `mv "$PREFIX"/'package/copied.exe' "$PREFIX"/'package/final.exe'`, `rm "$PREFIX"/'out/server.exe'`} {
		if !strings.Contains(commands, want) {
			t.Fatalf("missing %s in %s", want, commands)
		}
	}
}

func TestRunQuotedGlobsAndNativeToolNames(t *testing.T) {
	r := convertOK(t, `FROM scratch
RUN node.exe /opaque
RUN printf '%s' bracket > '/a[1].txt' && cat '/a['?'].txt' > /result.txt
RUN cat /[a\-c].txt > /class.txt
WORKDIR /literal[dir]
RUN cat *.txt
CMD ["/node.exe"]`)
	commands := buildCommands(r)
	for _, want := range []string{`"$PREFIX"/'node.exe' '/opaque'`, `cat "$PREFIX"/'a''['?']''.txt'`, `[a'-'c]`, `cat "$PREFIX"/'literal''[''dir'']''/'*'.txt'`} {
		if !strings.Contains(commands, want) {
			t.Fatalf("missing %s in %s", want, commands)
		}
	}
	if r.Entrypoint[0] != "${app}/node.exe" {
		t.Fatal(r.Entrypoint)
	}
}

func TestRunDirectoryContentsPreserveExecutableMapping(t *testing.T) {
	r := convertOK(t, `FROM golang
RUN go build -o /out/server .
RUN mkdir -p /package && cp -r /out/. /package
CMD ["/package/server"]`)
	if r.Entrypoint[0] != "${app}/package/server.exe" || !strings.Contains(buildCommands(r), `cp '-r' "$PREFIX"/'out/.' "$PREFIX"/'package'`) {
		t.Fatalf("directory contents changed: %+v\n%s", r.Entrypoint, buildCommands(r))
	}
}

func TestRunUnsupportedAndAmbiguousCommands(t *testing.T) {
	for _, command := range []string{
		"curl example.com", "apt-get upgrade", "for x in a; do echo x; done", "echo $(pwd)", "echo $((1+1))", "echo x &", "echo ~", "cat ../../escape",
		"mkdir -m", "cp -t /out /input", "echo x >$UNKNOWN", "echo x >*.txt", "echo x 2>&$FD", "cd /one; cat relative.txt", "cd /one || true; cat relative.txt", "mkdir /out || true", "mkdir /out | cat",
	} {
		t.Run(command, func(t *testing.T) {
			result, err := Convert([]byte("FROM node\nRUN "+command+"\nCMD [\"node\"]"), options(t), &fakeResolver{})
			if err == nil || result.Recipe != nil || !strings.Contains(fmt.Sprint(result.Diagnostics), ":2: RUN") {
				t.Fatalf("missing conversion diagnostic: %v %v", err, result.Diagnostics)
			}
		})
	}
	for _, command := range []string{"go build -o /server . || true", "go build -o /server . | cat"} {
		result, err := Convert([]byte("FROM golang\nRUN "+command+"\nCMD [\"/server\"]"), options(t), &fakeResolver{})
		if err == nil || !strings.Contains(fmt.Sprint(result.Diagnostics), "ambiguous") {
			t.Fatalf("conditional mapping accepted: %v %v", err, result.Diagnostics)
		}
	}
	for _, command := range []string{"pip install --target=/elsewhere flask", "python -B -mpip install -t/elsewhere flask", "python -m pip uninstall flask", "apt-get upgrade"} {
		if result, err := Convert([]byte("FROM python\nRUN "+command), options(t), &fakeResolver{}); err == nil {
			t.Fatalf("unsupported Python command accepted: %s %+v", command, result)
		}
	}
}

func TestRunVariablePathQuoting(t *testing.T) {
	r := convertOK(t, "FROM node\nENV DIR='/with space' FILE=file\nRUN mkdir -p \"$DIR\" && touch \"$DIR\"/$FILE\nCMD [\"node\"]")
	if !strings.Contains(buildCommands(r), `touch "$PREFIX"/'with space/file'`) {
		t.Fatal(buildCommands(r))
	}
	result, err := Convert([]byte("FROM node\nENV DIR='/with space'\nRUN touch $DIR/file\nCMD [\"node\"]"), options(t), &fakeResolver{})
	if err == nil || !strings.Contains(fmt.Sprint(result.Diagnostics), "split a path") {
		t.Fatalf("ambiguous field splitting accepted: %v %v", err, result.Diagnostics)
	}
	for _, tc := range []struct {
		pattern, name string
		match         bool
	}{
		{"out/[!a]*", "out/server", true},
		{"out/[!a]*", "out/app", false},
		{`out/\[?\].txt`, "out/[1].txt", true},
		{`out/[a\-c].txt`, "out/b.txt", false},
		{`out/[a\-c].txt`, "out/-.txt", true},
	} {
		if match, err := shellMatch(tc.pattern, tc.name); err != nil || match != tc.match {
			t.Fatalf("%s matches %s = %v, %v", tc.pattern, tc.name, match, err)
		}
	}
}

func TestPythonConsoleStartupCompositionAndStageIsolation(t *testing.T) {
	opts := options(t)
	opts.Target = "target"
	result, err := Convert([]byte(`FROM python AS base
RUN pip install local-package
ENTRYPOINT ["local-command","fixed"]
FROM base AS target
CMD ["one two","/data","$VALUE"]
FROM python AS unused
CMD ["missing-console"]`), opts, &fakeResolver{})
	if err != nil {
		t.Fatalf("%v %v", err, result.Diagnostics)
	}
	r := result.Recipe
	if !reflect.DeepEqual(r.Entrypoint, recipe.Entrypoint{"${python}/python.exe", "-c", consoleDispatch, "local-command", "fixed"}) || !reflect.DeepEqual(r.Cmd, []string{"one two", "/data", "$VALUE"}) {
		t.Fatalf("composition: %+v %+v", r.Entrypoint, r.Cmd)
	}
	if strings.Contains(buildCommands(r), "missing-console") || strings.Contains(buildCommands(&recipe.Recipe{Stages: r.Builds[0].Stages}), consoleLookup) {
		t.Fatal("startup validation leaked into a builder or unused stage")
	}
}
