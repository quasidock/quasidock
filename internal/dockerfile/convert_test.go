package dockerfile

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/recipe"
)

type fakeResolver struct {
	calls []string
	fail  bool
}

func (r *fakeResolver) Resolve(image, version, override string) (Dependency, error) {
	r.calls = append(r.calls, image+":"+version+":"+override)
	if r.fail {
		return Dependency{}, fmt.Errorf("no matching version; select --base")
	}
	return Dependency{"localhost:5000/" + image + "@sha256:" + strings.Repeat("a", 64), oci.AppConfig{ID: image, Version: "24.1.0", Permissions: oci.Permissions{Sandbox: oci.SandboxSoft}}}, nil
}
func options(t *testing.T) Options {
	t.Helper()
	dir := t.TempDir()
	return Options{Context: dir, Dockerfile: filepath.Join(dir, "Dockerfile"), Output: filepath.Join(dir, "quasidock.json"), ID: "example"}
}
func convertOK(t *testing.T, text string) *recipe.Recipe {
	t.Helper()
	result, err := Convert([]byte(text), options(t), &fakeResolver{})
	if err != nil {
		t.Fatalf("%v: %+v", err, result.Diagnostics)
	}
	return result.Recipe
}
func TestLanguageConversions(t *testing.T) {
	for _, tc := range []struct{ name, text, base, exe, build string }{
		{"node", "FROM node:24-alpine\nWORKDIR /app\nCOPY package*.json ./\nRUN npm ci\nCOPY . .\nENV MODE=production\nCMD [\"node\",\"server.js\"]\n", "node", "${node}/node.exe", "npm 'ci'"},
		{"go", "FROM golang:1.27-bookworm\nWORKDIR /src\nCOPY . .\nRUN go mod download && go build -o /app/server .\nWORKDIR /app\nCMD [\"./server\"]\n", "go", "${app}/app/server.exe", "server.exe"},
		{"python", "FROM python:3.14-slim\nWORKDIR /app\nCOPY requirements.txt .\nRUN pip install --no-cache-dir -r requirements.txt\nCOPY . .\nCMD [\"python3\",\"main.py\"]\n", "python", "${python}/python.exe", "--only-binary=:all:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := convertOK(t, tc.text)
			if r.From[tc.base] == "" || r.Entrypoint[0] != tc.exe {
				t.Fatalf("%+v", r)
			}
			b, _ := json.Marshal(r)
			if !strings.Contains(string(b), tc.build) {
				t.Fatalf("missing %s: %s", tc.build, b)
			}
			if r.Workdir != "app" {
				t.Fatalf("workdir %q", r.Workdir)
			}
		})
	}
}
func TestArgAndEnvScope(t *testing.T) {
	opts := options(t)
	opts.BuildArgs = map[string]string{"BASE": "24", "VALUE": "override"}
	resolver := &fakeResolver{}
	r, err := Convert([]byte("ARG BASE=22\nFROM node:${BASE}\nARG BASE\nARG VALUE=default\nENV FIRST=${VALUE} SECOND=${BASE}\nWORKDIR /app\nRUN node -e 'console.log(process.env.FIRST)'\nENV FIRST=later\nENTRYPOINT [\"node\",\"app.js\"]\nCMD [\"--serve\"]"), opts, resolver)
	if err != nil {
		t.Fatalf("%v %+v", err, r.Diagnostics)
	}
	if resolver.calls[0] != "node:24:" || r.Recipe.Env["FIRST"] != "later" || r.Recipe.Env["SECOND"] != "24" {
		t.Fatalf("%+v %+v", resolver.calls, r.Recipe.Env)
	}
	if _, ok := r.Recipe.Env["VALUE"]; ok {
		t.Fatal("ARG leaked into runtime env")
	}
	if r.Recipe.Stages[1].Env["FIRST"] != "override" {
		t.Fatal("stage env changed retroactively")
	}
	if len(r.Recipe.Cmd) != 1 || r.Recipe.Cmd[0] != "--serve" {
		t.Fatal(r.Recipe.Cmd)
	}
	bad, err := Convert([]byte("ARG BASE=24\nFROM node:${BASE}\nENV VALUE=${BASE}\nCMD [\"node\"]"), opts, resolver)
	if err == nil || !strings.Contains(fmt.Sprint(bad.Diagnostics), "unresolved") {
		t.Fatal("global ARG leaked into stage")
	}
}
func TestUnsupportedInstructionsHaveLocations(t *testing.T) {
	for _, line := range []string{"RUN apt-get upgrade", "RUN apk add curl", "USER node", "VOLUME /data", "HEALTHCHECK CMD node -v", "SHELL [\"bash\",\"-c\"]", "COPY --from=build /app /app", "RUN --mount=type=cache,target=/root/.npm npm ci", "RUN node $(uname)", "RUN GOOS=linux go build -o app .", "ADD . /app"} {
		t.Run(line, func(t *testing.T) {
			r, err := Convert([]byte("FROM node:24\n"+line+"\nCMD [\"node\"]"), options(t), &fakeResolver{})
			if err == nil || r.Recipe != nil {
				t.Fatalf("unexpected success: %s", line)
			}
			if len(r.Diagnostics) == 0 || r.Diagnostics[0].Line != 2 {
				t.Fatalf("%+v", r.Diagnostics)
			}
		})
	}
}
func TestShellStartupAndEntrypointReset(t *testing.T) {
	r := convertOK(t, "FROM node\nWORKDIR /app\nCMD node server.js --port \"$PORT\"")
	if r.From["busybox"] == "" || r.SDK["busybox"] != "" || r.Entrypoint[2] != "-c" || !strings.Contains(r.Entrypoint[3], `"$PORT"`) {
		t.Fatalf("%+v", r)
	}
	r = convertOK(t, "FROM node\nCMD [\"--old\"]\nENTRYPOINT [\"node\",\"app.js\"]")
	if len(r.Cmd) != 0 {
		t.Fatal("ENTRYPOINT did not reset earlier CMD")
	}
}
func TestVersionOverrideAndAdvisoryMetadata(t *testing.T) {
	opts := options(t)
	opts.Base = "node:24@sha256:" + strings.Repeat("b", 64)
	resolver := &fakeResolver{}
	r, err := Convert([]byte("FROM node:22-alpine\nEXPOSE 3000\nLABEL example=value\nCMD [\"node\"]"), opts, resolver)
	if err != nil || len(r.Diagnostics) != 3 || !strings.Contains(resolver.calls[0], opts.Base) {
		t.Fatalf("%v %+v %+v", err, r.Diagnostics, resolver.calls)
	}
	resolver.fail = true
	if _, err := Convert([]byte("FROM node:22\nCMD [\"node\"]"), opts, resolver); err == nil {
		t.Fatal("accepted missing base")
	}
}

func TestPathEnvironmentAndShellExpansion(t *testing.T) {
	r := convertOK(t, "FROM node\nENV APP_DIR=/app OPTIONS='one two'\nWORKDIR $APP_DIR\nRUN OPTIONS=other node app.js $OPTIONS\nCMD node app.js \"$APP_DIR\"")
	if r.Workdir != "app" || r.Env["APP_DIR"] != "${app}/app" {
		t.Fatalf("%+v", r)
	}
	build := r.Stages[1].Build[0]
	if !strings.Contains(build, "OPTIONS='other'") || !strings.Contains(build, "$OPTIONS") {
		t.Fatal(build)
	}
	if !strings.Contains(r.Entrypoint[3], `"$APP_DIR"`) {
		t.Fatal(r.Entrypoint)
	}
	r = convertOK(t, "FROM golang\nWORKDIR /app\nCOPY . .\nRUN go build -o app .\nCMD ./app")
	if !strings.Contains(r.Entrypoint[3], "app.exe") {
		t.Fatal(r.Entrypoint)
	}
}

func TestStartupDiagnosticsAndLiteralPlaceholders(t *testing.T) {
	for _, text := range []string{"FROM node\nWORKDIR /app\nCMD [\"./start.sh\"]", "FROM node\nWORKDIR /app\nCMD [\"node\", \"${data}\"]", "FROM node\nWORKDIR /app\nCMD node \"${app}\""} {
		result, err := Convert([]byte(text), options(t), &fakeResolver{})
		if err == nil {
			t.Fatal("unsupported startup succeeded")
		}
		found := false
		for _, d := range result.Diagnostics {
			if !d.Warning && d.Line == 3 {
				found = true
			}
		}
		if !found {
			t.Fatal(result.Diagnostics)
		}
	}
	if _, err := Convert([]byte("# syntax=example.com/custom:1\nFROM node"), options(t), &fakeResolver{}); err == nil {
		t.Fatal("accepted custom frontend")
	}
	convertOK(t, "# syntax=docker/dockerfile:1\nFROM node\nCMD [\"node\"]")
}
