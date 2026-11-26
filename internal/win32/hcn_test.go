package win32

import (
	"encoding/json"
	"errors"
	"io/fs"
	"strings"
	"testing"
)

func TestHCNNetworkID(t *testing.T) {
	const id = "0abcd012-1234-5678-90ab-bcdef0123456"
	want := GUID{0x0abcd012, 0x1234, 0x5678, [8]byte{0x90, 0xab, 0xbc, 0xde, 0xf0, 0x12, 0x34, 0x56}}
	for _, input := range []string{id, "{" + strings.ToUpper(id) + "}"} {
		got, err := parseHCNID(input)
		if err != nil || got != want || formatHCNID(got) != id {
			t.Errorf("parse %q: got %+v, error %v, want %+v", input, got, err, want)
		}
	}
	for _, input := range []string{"", id[1:], "z" + id[1:], "-" + id[1:]} {
		if _, err := parseHCNID(input); err == nil {
			t.Errorf("accepted invalid GUID %q", input)
		}
	}
}

func TestHCNNetworkProperties(t *testing.T) {
	for _, test := range []struct {
		raw    string
		driver string
	}{
		{`{"Name":"Sample","Type":"Private"}`, "private"},
		{`{"Name":"Sample"}`, "nat"},
		{`{"Name":"Sample","Type":"ConstrainedICS"}`, "constrainedics"},
	} {
		network, err := decodeHCNNetwork(GUID{}, test.raw)
		if err != nil || network.Name != "Sample" || network.Driver != test.driver {
			t.Errorf("decode %s: got %+v, error %v", test.raw, network, err)
		}
	}
}

func TestHCNNetworkDetails(t *testing.T) {
	const raw = `{"Name":"Sample","FutureField":[{"LargestID":18446744073709551615,"Enabled":true,"Optional":null}]}`
	id := GUID{Data1: 1}
	details, err := decodeHCNNetworkDetails(id, raw)
	if err != nil {
		t.Fatal(err)
	}
	if details.ID != "00000001-0000-0000-0000-000000000000" || details.Name != "Sample" || details.Driver != "nat" {
		t.Errorf("unexpected network summary: %+v", details.HCNNetwork)
	}
	got, err := json.Marshal(details.Properties)
	const want = `{"FutureField":[{"Enabled":true,"LargestID":18446744073709551615,"Optional":null}],"Name":"Sample"}`
	if err != nil || string(got) != want {
		t.Errorf("detailed properties = %s, error %v", got, err)
	}
	for _, invalid := range []string{"", "null", "[]", `{"Name":123}`, `{"Name":"Sample"}{}`} {
		if _, err := decodeHCNNetworkDetails(id, invalid); err == nil {
			t.Errorf("accepted invalid HCN properties %q", invalid)
		}
	}
}

func TestHCNResult(t *testing.T) {
	if err := hcnResult("HcnCreateNetwork", 0, nil); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		code uintptr
		want error
	}{
		{0x80070005, fs.ErrPermission},
		{0x803b0010, ErrHCNNetworkExists},
		{0x803b0001, ErrHCNNetworkNotFound},
		{0x80070490, ErrHCNNetworkNotFound},
		{0x803b0031, ErrHCNNetworkNotFound},
		{0x800706ba, ErrHCNUnavailable},
	} {
		err := hcnResult("HcnOpenNetwork", test.code, nil)
		if !errors.Is(err, test.want) || !strings.Contains(err.Error(), "HcnOpenNetwork") {
			t.Errorf("HRESULT 0x%08x: got %v, want %v", test.code, err, test.want)
		}
	}
}
