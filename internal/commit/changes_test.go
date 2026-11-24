package commit

import (
	"reflect"
	"strings"
	"testing"

	"github.com/quasidock/quasidock/internal/oci"
)

func TestChanges(t *testing.T) {
	cfg := oci.AppConfig{FormatVersion: 4, ID: "fixture", Version: "1", Entrypoint: []string{"app.exe"}, Cmd: []string{"old"}, Env: map[string]string{"foo": "old"}, UnsetEnv: []string{"BAR"}}
	err := ApplyChanges(&cfg, []string{`ENV FOO="two words" BAR=${data}/file`, `LABEL owner="a person"`, `WORKDIR ${data}`, `WORKDIR home`, `ENTRYPOINT []`, `CMD ["cmd.exe","/c","echo hello"]`})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Entrypoint) != 0 || !reflect.DeepEqual(cfg.Cmd, []string{"cmd.exe", "/c", "echo hello"}) || cfg.Workdir != "${data}/home" || cfg.Env["FOO"] != "two words" || cfg.Env["BAR"] != "${data}/file" || len(cfg.Env) != 2 || len(cfg.UnsetEnv) != 0 || cfg.Labels["owner"] != "a person" {
		t.Fatalf("changes: %+v", cfg)
	}
	if err := ApplyChanges(&cfg, []string{`ENTRYPOINT ["app.exe"]`}); err != nil || len(cfg.Cmd) != 0 {
		t.Fatalf("entrypoint did not clear cmd: %+v %v", cfg, err)
	}
}

func TestInvalidChanges(t *testing.T) {
	for _, change := range []string{"", "RUN echo hello", "USER test", "VOLUME /data", "EXPOSE 80", "CMD echo hello", "ENTRYPOINT echo hello", "WORKDIR ../outside", "WORKDIR C:/outside", "WORKDIR ${unknown}/dir", "ENV =bad", "CMD [42]", "ENV X=x\nLABEL Y=y", "ENTRYPOINT []"} {
		t.Run(change, func(t *testing.T) {
			cfg := oci.AppConfig{FormatVersion: 4, ID: "fixture", Version: "1", Entrypoint: []string{"app.exe"}, Cmd: []string{"default"}}
			if err := ApplyChanges(&cfg, []string{change}); err == nil {
				t.Fatalf("accepted %q", change)
			} else if !strings.Contains(err.Error(), "--change") && change != "ENTRYPOINT []" {
				t.Fatalf("missing change context: %v", err)
			}
		})
	}
}
