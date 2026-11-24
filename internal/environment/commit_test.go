package environment

import "testing"

func TestImageUnsetBeforeRunOverrides(t *testing.T) {
	env, err := Build(nil, Spec{SystemRoot: `C:\Windows`, Vars: []Var{{Name: "REMOVE_ME", Value: "runtime"}}, Unset: []string{"path", "Remove_Me"}, Extra: []string{"REMOVE_ME=caller"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range env {
		if v.Name == "PATH" {
			t.Fatal("generated PATH escaped image unset")
		}
		if v.Name == "REMOVE_ME" && v.Value != "caller" {
			t.Fatalf("run override lost: %+v", v)
		}
	}
}
