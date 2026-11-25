package cli

import (
	"bytes"
	stdcontext "context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/quasidock/quasidock/internal/context"
	"github.com/quasidock/quasidock/internal/imagearchive"
	"github.com/quasidock/quasidock/internal/oci"
	"github.com/quasidock/quasidock/internal/store"
	"github.com/quasidock/quasidock/internal/testregistry"
)

func TestParseHistoryOptions(t *testing.T) {
	for _, args := range [][]string{
		{"--human=false", "--no-trunc", "--format", "json", "app", "-q"},
		{"app", "-H=false", "--no-trunc=true", "--quiet=true", "--format=json"},
		{"-q", "--human=false", "--no-trunc", "--format=json", "--", "app"},
	} {
		opts, err := parseHistoryOptions(args)
		if err != nil || opts.image != "app" || opts.human || !opts.quiet || !opts.noTrunc || opts.format != "json" {
			t.Fatalf("%v: %+v %v", args, opts, err)
		}
	}
	if opts, err := parseHistoryOptions([]string{"-H", "--quiet=false", "--no-trunc=false", "app"}); err != nil || !opts.human || opts.quiet || opts.noTrunc {
		t.Fatalf("defaults: %+v %v", opts, err)
	}
	for _, args := range [][]string{
		nil, {""}, {"one", "two"}, {"--format"}, {"--format="}, {"--format", ""},
		{"--human=maybe", "app"}, {"-q=bad", "app"}, {"--no-trunc=bad", "app"},
		{"--platform=windows/amd64", "app"}, {"-x", "app"}, {"-", "app"}, {"--", "one", "two"},
	} {
		if _, err := parseHistoryOptions(args); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

func historyDigest(n int) string { return fmt.Sprintf("sha256:%064x", n) }

func historyFixture() *store.LoadedImage {
	return &store.LoadedImage{
		Config: oci.AppConfig{ID: "history-fixture", Version: "1", Entrypoint: []string{"app.exe"}, From: map[string]oci.Runtime{
			"alpha": {ID: "runtime-a", Version: "1", Image: historyDigest(11), Layer: historyDigest(1)},
			"zulu":  {ID: "runtime-z", Version: "2", Image: historyDigest(12), Layer: historyDigest(2)},
		}},
		Manifest: oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeManifest, Layers: []oci.Descriptor{
			{MediaType: oci.MediaTypeLayerTar, Digest: historyDigest(1), Size: 1024},
			{MediaType: oci.MediaTypeLayerTarGzip, Digest: historyDigest(2), Size: 2048},
			{MediaType: oci.MediaTypeLayerTarGzip, Digest: historyDigest(3), Size: 4096},
		}, Annotations: map[string]string{"org.opencontainers.image.created": "2020-01-02T10:04:05.123456789+07:00"}},
	}
}

func TestImageHistoryRows(t *testing.T) {
	now := time.Date(2020, 1, 2, 4, 4, 5, 123456789, time.UTC)
	opts := historyOptions{noTrunc: true}
	img := historyFixture()
	// The historical descriptor order need not match sorted aliases.
	img.Manifest.Layers[0], img.Manifest.Layers[1] = img.Manifest.Layers[1], img.Manifest.Layers[0]
	if err := oci.ValidateLayers("fixture", &img.Manifest, &img.Config); err != nil {
		t.Fatal(err)
	}
	rows, err := imageHistoryRows(img, historyDigest(99), opts, now)
	if err != nil {
		t.Fatal(err)
	}
	want := []historyRow{
		{ID: historyDigest(99), CreatedAt: "2020-01-02T03:04:05.123456789Z", CreatedSince: "2020-01-02T03:04:05.123456789Z", CreatedBy: "APP history-fixture:1", Size: "4096", LayerDigest: historyDigest(3)},
		{ID: historyDigest(11), CreatedAt: "--", CreatedSince: "--", CreatedBy: "FROM alpha runtime-a:1", Size: "1024", LayerDigest: historyDigest(1)},
		{ID: historyDigest(12), CreatedAt: "--", CreatedSince: "--", CreatedBy: "FROM zulu runtime-z:2", Size: "2048", LayerDigest: historyDigest(2)},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows: %+v; want %+v", rows, want)
	}
	rows, err = imageHistoryRows(img, historyDigest(99), historyOptions{human: true}, now)
	if err != nil || rows[0].CreatedSince != "About an hour ago" || rows[0].Size != "4.0 KiB" || rows[0].ID != oci.DigestHex(historyDigest(99))[:12] {
		t.Fatalf("human rows: %+v %v", rows, err)
	}
	for _, created := range []string{"", "invalid"} {
		img.Manifest.Annotations["org.opencontainers.image.created"] = created
		rows, err = imageHistoryRows(img, historyDigest(99), opts, now)
		if err != nil || rows[0].CreatedAt != "--" || rows[0].CreatedSince != "--" {
			t.Fatalf("optional date %q: %+v %v", created, rows, err)
		}
	}

	// Identical descriptors still represent two occurrences. Sorted aliases
	// are consumed in forward manifest order before reversing the output.
	runtime := img.Config.From["zulu"]
	runtime.Layer = img.Config.From["alpha"].Layer
	img.Config.From["zulu"] = runtime
	img.Manifest.Layers[0] = img.Manifest.Layers[1]
	rows, err = imageHistoryRows(img, historyDigest(99), opts, now)
	if err != nil || len(rows) != 3 || rows[1].ID != runtime.Image || rows[2].ID != img.Config.From["alpha"].Image || rows[1].Size != "1024" || rows[2].Size != "1024" {
		t.Fatalf("duplicate layers: %+v %v", rows, err)
	}
}

func TestImageHistoryCommitRows(t *testing.T) {
	img := historyFixture()
	img.Config.FormatVersion, img.Config.DataLayer = 4, historyDigest(4)
	app := oci.OwnLayer(&img.Manifest)
	img.Manifest.Layers = append(img.Manifest.Layers[:2], oci.Descriptor{MediaType: oci.MediaTypeLayerTarGzip, Digest: img.Config.DataLayer, Size: 512}, app)
	img.Manifest.Annotations["io.quasidock.commit.parent"] = historyDigest(98)
	img.Manifest.Annotations["io.quasidock.commit.message"] = "saved state"
	img.Manifest.Annotations["org.opencontainers.image.authors"] = "test author"
	if err := oci.ValidateLayers("fixture", &img.Manifest, &img.Config); err != nil {
		t.Fatal(err)
	}
	rows, err := imageHistoryRows(img, historyDigest(99), historyOptions{noTrunc: true}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 || rows[0].ID != historyDigest(99) || rows[0].CreatedBy != "COMMIT "+historyDigest(98) || rows[0].LayerDigest != img.Config.DataLayer || rows[0].Size != "512" || rows[0].Comment != "saved state" || rows[0].Author != "test author" || rows[0].CreatedAt == "--" {
		t.Fatalf("commit: %+v", rows)
	}
	if rows[1].ID != "<missing>" || rows[1].CreatedAt != "--" || rows[1].Author != "" || rows[1].Comment != "" || rows[1].LayerDigest != app.Digest {
		t.Fatalf("reused app: %+v", rows[1])
	}
	delete(img.Manifest.Annotations, "io.quasidock.commit.parent")
	rows, err = imageHistoryRows(img, historyDigest(99), historyOptions{}, time.Now())
	if err != nil || rows[0].CreatedBy != "DATA snapshot" {
		t.Fatalf("unannotated snapshot: %+v %v", rows, err)
	}
}

func TestHistoryRenderer(t *testing.T) {
	row := historyRow{ID: historyDigest(99), CreatedAt: "--", CreatedSince: "--", CreatedBy: "APP 雪\n\t\x1b[2J", Size: "42", Comment: "note\r\n雪\u2028", LayerDigest: historyDigest(1), Author: "a\t\u2029"}
	for _, format := range []string{"", "table", "table {{.CreatedBy}}\t{{.Comment}}\t{{.Author}}"} {
		renderer, err := newHistoryRenderer(historyOptions{format: format})
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := renderer.render(&out, []historyRow{row}); err != nil {
			t.Fatal(err)
		}
		text := out.String()
		if strings.Count(text, "\n") != 2 || strings.ContainsAny(text, "\r\x1b\u2028\u2029") || !strings.Contains(text, `APP 雪\n\t\x1b[2J`) || !strings.Contains(text, `note\r\n雪\u2028`) {
			t.Fatalf("unsafe table: %q", text)
		}
	}
	for _, format := range []string{"json", "{{json .}}"} {
		renderer, err := newHistoryRenderer(historyOptions{format: format})
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := renderer.render(&out, []historyRow{row, row}); err != nil {
			t.Fatal(err)
		}
		decoded := decodeHistory(t, out.Bytes())
		if !reflect.DeepEqual(decoded, []historyRow{row, row}) {
			t.Fatalf("JSON round trip: %+v", decoded)
		}
	}
	for _, format := range []string{"{{", "{{.Missing}}", "table {{.Missing}}"} {
		if _, err := newHistoryRenderer(historyOptions{format: format}); err == nil {
			t.Errorf("accepted template %q", format)
		}
	}
	renderer, err := newHistoryRenderer(historyOptions{format: "{{.ID}}:{{.LayerDigest}}"})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := renderer.render(&out, []historyRow{row}); err != nil || out.String() != row.ID+":"+row.LayerDigest+"\n" {
		t.Fatalf("template: %q %v", out.String(), err)
	}
	// A template may validate against an empty row but fail against real data.
	renderer, err = newHistoryRenderer(historyOptions{format: "{{if .ID}}{{index .Comment 100}}{{end}}"})
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := renderer.render(&out, []historyRow{{}, row}); err == nil || out.Len() != 0 {
		t.Fatalf("partial output after template error: %q %v", out.String(), err)
	}
	renderer, err = newHistoryRenderer(historyOptions{quiet: true, format: "{{.Missing}}"})
	if err != nil {
		t.Fatal(err)
	}
	if err := renderer.render(&out, []historyRow{row, {ID: "<missing>"}}); err != nil || out.String() != row.ID+"\n<missing>\n" {
		t.Fatalf("quiet: %q %v", out.String(), err)
	}
	writeErr := errors.New("closed output")
	if err := renderer.render(diffErrorWriter{writeErr}, []historyRow{row}); !errors.Is(err, writeErr) {
		t.Fatalf("writer error: %v", err)
	}
	if err := renderer.render(historyShortWriter{}, []historyRow{row}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write: %v", err)
	}
}

type historyShortWriter struct{}

func (historyShortWriter) Write(data []byte) (int, error) { return len(data) / 2, nil }

func TestHistoryUnicodeTruncation(t *testing.T) {
	img := historyFixture()
	img.Config.Version = strings.Repeat("雪", 50)
	rows, err := imageHistoryRows(img, historyDigest(99), historyOptions{}, time.Now())
	if err != nil || !utf8.ValidString(rows[0].CreatedBy) || utf8.RuneCountInString(rows[0].CreatedBy) != 45 || !strings.HasSuffix(rows[0].CreatedBy, "...") {
		t.Fatalf("truncation: %+v %v", rows, err)
	}
	rows, err = imageHistoryRows(img, historyDigest(99), historyOptions{noTrunc: true}, time.Now())
	if err != nil || rows[0].CreatedBy != "APP "+img.Config.ID+":"+img.Config.Version || rows[0].ID != historyDigest(99) {
		t.Fatalf("no truncation: %+v %v", rows, err)
	}
}

func registerHistoryImage(t *testing.T, ctx *context.Context, img *store.LoadedImage, name string) string {
	t.Helper()
	put := func(value any, mediaType string) oci.Descriptor {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		desc, err := ctx.Store.PutBlob(data, mediaType)
		if err != nil {
			t.Fatal(err)
		}
		return desc
	}
	mediaType := oci.MediaTypeConfig
	if img.Config.FormatVersion == 4 {
		mediaType = oci.MediaTypeConfigV4
	}
	img.Manifest.Config = put(img.Config, mediaType)
	desc := put(img.Manifest, oci.MediaTypeManifest)
	if err := ctx.Store.RegisterImage(store.Ref{ID: img.Config.ID, Version: img.Config.Version, Manifest: desc.Digest, Source: name}, name, true); err != nil {
		t.Fatal(err)
	}
	return desc.Digest
}

func decodeHistory(t *testing.T, data []byte) []historyRow {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	var rows []historyRow
	for {
		var row historyRow
		err := decoder.Decode(&row)
		if err == io.EOF {
			return rows
		}
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
}

func TestHistoryCLI(t *testing.T) {
	ctx, out := fixtureCLI(t)
	t.Setenv("QUASIDOCK_HOME", ctx.Store.Root)
	reg := testregistry.New()
	t.Cleanup(reg.Close)
	first, _ := fixtureImage(t, reg, "1", []byte("first"))
	second, _ := fixtureImage(t, reg, "2", []byte("second"))
	for _, version := range []string{"1", "2"} {
		if _, err := pullImage(ctx, reg.Host()+"/app:"+version, false); err != nil {
			t.Fatal(err)
		}
	}
	alias := "example.test/history:alias"
	if err := ctx.Store.TagImage("", first, alias); err != nil {
		t.Fatal(err)
	}
	requests := reg.Count("")
	reg.Fail(true)
	ctx.Env["QUASIDOCK_REGISTRY"] = reg.Host()
	for _, test := range []struct{ selector, digest string }{
		{first, first}, {reg.Host() + "/app:1", first}, {reg.Host() + "/app@" + first, first},
		{alias, first}, {"app:1", first}, {"test.cli.fixture", second},
	} {
		out.Reset()
		code, err := imageCommand(ctx, []string{"history", "--no-trunc", "--human=false", "--format=json", test.selector})
		if err != nil || code != 0 {
			t.Fatalf("%s: %d %v", test.selector, code, err)
		}
		rows := decodeHistory(t, out.Bytes())
		if len(rows) != 1 || rows[0].ID != test.digest || rows[0].CreatedAt != "--" || rows[0].CreatedBy == "" {
			t.Fatalf("%s: %+v", test.selector, rows)
		}
	}
	for _, command := range [][]string{{"history"}, {"image", "history"}} {
		out.Reset()
		code, err := Dispatch(ctx, append(append([]string(nil), command...), "-q", "--no-trunc", first))
		if err != nil || code != 0 || out.String() != first+"\n" {
			t.Fatalf("dispatch %v: %d %q %v", command, code, out.String(), err)
		}
		out.Reset()
		code, err = Dispatch(ctx, append(append([]string(nil), command...), "--help"))
		if err != nil || code != 0 || !strings.Contains(out.String(), historyUsage) {
			t.Fatalf("help %v: %d %q %v", command, code, out.String(), err)
		}
	}
	out.Reset()
	if code, err := imageCommand(ctx, []string{"history", "missing"}); code != 1 || !errors.Is(err, store.ErrImageNotFound) || out.Len() != 0 {
		t.Fatalf("missing: %d %q %v", code, out.String(), err)
	}
	if reg.Count("") != requests {
		t.Fatal("history contacted the registry")
	}
}

func TestHistoryMissingDependencies(t *testing.T) {
	ctx, out := fixtureCLI(t)
	img := historyFixture()
	// Neither runtime manifests nor layer blobs exist. The selected metadata is
	// sufficient for incomplete lazy images and for parents that were pruned.
	digest := registerHistoryImage(t, ctx, img, "example.test/history:1")
	if code, err := imageCommand(ctx, []string{"history", "--format=json", digest}); err != nil || code != 0 || len(decodeHistory(t, out.Bytes())) != 3 {
		t.Fatalf("metadata-only history: %d %q %v", code, out.String(), err)
	}
}

func TestHistoryReadErrors(t *testing.T) {
	for _, kind := range []string{"missing manifest", "corrupt manifest", "missing config", "corrupt config", "negative size", "invalid layer", "layer count"} {
		t.Run(kind, func(t *testing.T) {
			ctx, out := fixtureCLI(t)
			img := historyFixture()
			switch kind {
			case "negative size":
				img.Manifest.Layers[0].Size = -1
			case "invalid layer":
				img.Manifest.Layers[2].Digest = "invalid"
			case "layer count":
				img.Manifest.Layers = img.Manifest.Layers[:2]
			}
			digest := registerHistoryImage(t, ctx, img, "example.test/history:1")
			if strings.HasSuffix(kind, "manifest") || strings.HasSuffix(kind, "config") {
				blob := digest
				if strings.HasSuffix(kind, "config") {
					blob = img.Manifest.Config.Digest
				}
				path := ctx.Store.BlobPath(oci.DigestHex(blob))
				var err error
				if strings.HasPrefix(kind, "missing") {
					err = os.Remove(path)
				} else {
					err = os.WriteFile(path, []byte("corrupt"), 0o600)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if code, err := imageCommand(ctx, []string{"history", digest}); err == nil || code != 1 || out.Len() != 0 {
				t.Fatalf("%s: %d %q %v", kind, code, out.String(), err)
			}
		})
	}
}

func TestHistoryArchive(t *testing.T) {
	ctx, out := fixtureCLI(t)
	reg := testregistry.New()
	t.Cleanup(reg.Close)
	parent, _ := fixtureImage(t, reg, "1", []byte("fixture"))
	if _, err := pullImage(ctx, reg.Host()+"/app:1", false); err != nil {
		t.Fatal(err)
	}
	img, err := ctx.Store.ReadImage("fixture", parent)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := ctx.Store.WriteDataLayer(stdcontext.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	img.Config.FormatVersion, img.Config.DataLayer = 4, seed.Digest
	img.Manifest.Layers = []oci.Descriptor{seed, oci.OwnLayer(&img.Manifest)}
	img.Manifest.Annotations = map[string]string{"io.quasidock.commit.parent": parent, "io.quasidock.commit.message": "snapshot", "org.opencontainers.image.authors": "author", "org.opencontainers.image.created": "2020-01-02T03:04:05Z"}
	digest := registerHistoryImage(t, ctx, img, "example.test/history:saved")
	args := []string{"history", "--no-trunc", "--human=false", "--format=json", digest}
	out.Reset()
	if _, err := imageCommand(ctx, args); err != nil {
		t.Fatal(err)
	}
	before := bytes.Clone(out.Bytes())
	var archive bytes.Buffer
	if err := imagearchive.Save(ctx.Store, "", []string{digest}, &archive); err != nil {
		t.Fatal(err)
	}
	fresh, restored := fixtureCLI(t)
	if _, err := imagearchive.Load(fresh.Store, &archive, imagearchive.LoadOptions{}); err != nil {
		t.Fatal(err)
	}
	if fresh.Store.HasBlob(parent) {
		t.Fatal("archive unexpectedly included the commit parent")
	}
	if _, err := imageCommand(fresh, args); err != nil || !bytes.Equal(before, restored.Bytes()) {
		t.Fatalf("archive changed history: %q != %q (%v)", before, restored.Bytes(), err)
	}
}
