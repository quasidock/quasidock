package updates

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zigsaw-io/zigsaw/internal/context"
	"github.com/zigsaw-io/zigsaw/internal/lifecycle"
	"github.com/zigsaw-io/zigsaw/internal/schedule"
	"github.com/zigsaw-io/zigsaw/internal/store"
	"github.com/zigsaw-io/zigsaw/internal/version"
)

const remindAfterSeconds = 3 * 24 * 60 * 60

type State struct {
	Checked      int64               `json:"checked"`
	Apps         []lifecycle.Waiting `json:"apps,omitempty"`
	Failed       []string            `json:"failed,omitempty"`
	Zigsaw       *string             `json:"zigsaw,omitempty"`
	ZigsawFailed bool                `json:"zigsaw_failed,omitempty"`
	Notified     []string            `json:"notified,omitempty"`
	NotifiedAt   int64               `json:"notified_at,omitempty"`
	Seen         []string            `json:"seen,omitempty"`
}

func (s State) Items() []string {
	var out []string
	for _, app := range s.Apps {
		out = append(out, fmt.Sprintf("%s@%s", app.ID, app.ToDigest))
	}
	if s.Zigsaw != nil {
		out = append(out, fmt.Sprintf("zigsaw@%s", *s.Zigsaw))
	}
	return out
}

func statePath(ctx *context.Context) string {
	return ctx.Store.Path("updates", "state.json")
}

func load(ctx *context.Context) State {
	bytes, err := os.ReadFile(statePath(ctx))
	if err != nil {
		return State{}
	}
	var s State
	if err := json.Unmarshal(bytes, &s); err != nil {
		return State{}
	}
	return s
}

func save(ctx *context.Context, s State) error {
	dest := statePath(ctx)
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	tmp := ctx.Store.TmpPath("updates")
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return store.RenameRetrying(tmp, dest, 2000)
}

func stillWaiting(ctx *context.Context, s State) State {
	var liveApps []lifecycle.Waiting
	for _, app := range s.Apps {
		ref, err := ctx.Store.ReadRef(app.ID)
		if err != nil || ref == nil {
			continue
		}
		if ref.Manifest != app.FromDigest {
			continue
		}
		if ctx.Store.IsHeld(app.ID) {
			continue
		}
		skip, _ := ctx.Store.ReadSkip(app.ID)
		if lifecycle.PassesOver(skip, app.ToDigest) != nil {
			continue
		}
		liveApps = append(liveApps, app)
	}
	live := s
	live.Apps = liveApps
	if s.Zigsaw != nil && !lifecycle.IsNewer(*s.Zigsaw) {
		live.Zigsaw = nil
	}
	return live
}

func contains(list []string, item string) bool {
	for _, x := range list {
		if x == item {
			return true
		}
	}
	return false
}

func Due(state State, items []string, now int64) bool {
	unseen := false
	isNew := false
	for _, item := range items {
		if !contains(state.Seen, item) {
			unseen = true
		}
		if !contains(state.Notified, item) {
			isNew = true
		}
	}
	if !unseen {
		return false
	}
	return isNew || now-state.NotifiedAt >= remindAfterSeconds
}

func Clip(text string, max int) string {
	if len(text) <= max {
		return text
	}
	end := max
	for end > 0 && (text[end]&0xc0 == 0x80) {
		end--
	}
	return text[:end]
}

func WriteNotification(w io.Writer, s State) {
	count := len(s.Apps)
	if s.Zigsaw != nil {
		count++
	}
	plural := "s"
	if count == 1 {
		plural = ""
	}
	fmt.Fprintf(w, "zigsaw: %d update%s waiting\n", count, plural)
	if len(s.Apps) > 0 {
		first := s.Apps[0]
		fmt.Fprintf(w, "%s %s to %s", Clip(first.ID, 80), Clip(first.From, 30), Clip(first.To, 30))
		if count > 1 {
			fmt.Fprintf(w, ", and %d more", count-1)
		}
	} else if s.Zigsaw != nil {
		fmt.Fprintf(w, "zigsaw %s is out", Clip(*s.Zigsaw, 30))
	}
	fmt.Fprintf(w, ". Click to see what's waiting; nothing gets installed.\n")
}

type CheckOptions struct {
	Notify bool
	Quiet  bool
}

func Check(ctx *context.Context, opts CheckOptions) (uint32, error) {
	before := load(ctx)
	state := before

	var apps []lifecycle.Waiting
	var failed []string

	refs, err := lifecycle.RefsOf(ctx, nil)
	if err != nil {
		return 1, err
	}
	for _, ref := range refs {
		found, err := lifecycle.NewImage(ctx, ref)
		if err != nil {
			context.Note("%s: %v", ref.ID, err)
			failed = append(failed, ref.ID)
			for _, app := range before.Apps {
				if app.ID == ref.ID {
					apps = append(apps, app)
				}
			}
			continue
		}
		if found != nil {
			apps = append(apps, *found)
		}
	}
	state.Apps = apps
	state.Failed = failed

	state.ZigsawFailed = false
	if lifecycle.Applies(ctx) {
		if v, err := lifecycle.Newer(ctx); err == nil && v != "" {
			state.Zigsaw = &v
		} else if err != nil {
			context.Note("zigsaw's latest version: %v", err)
			state.ZigsawFailed = true
		}
	} else {
		state.Zigsaw = nil
	}

	state.Checked = time.Now().Unix()
	state = stillWaiting(ctx, state)
	items := state.Items()

	if opts.Notify && Due(state, items, state.Checked) {
		state.Notified = items
		state.NotifiedAt = state.Checked
		var sb strings.Builder
		WriteNotification(&sb, state)
		fmt.Print(sb.String())
	}

	_ = save(ctx, state)

	if !opts.Notify && !opts.Quiet {
		isOn, _ := schedule.IsOn(ctx)
		writeList(ctx, state, isOn)
	}

	if len(failed) > 0 || state.ZigsawFailed {
		return 1, nil
	}
	return 0, nil
}

func Show(ctx *context.Context) error {
	state := stillWaiting(ctx, load(ctx))
	isOn, _ := schedule.IsOn(ctx)
	writeList(ctx, state, isOn)
	items := state.Items()
	if len(items) == 0 {
		return nil
	}
	looked := state
	looked.Seen = items
	return save(ctx, looked)
}

func FormatAgo(seconds int64) string {
	s := seconds
	if s < 0 {
		s = 0
	}
	if s < 60 {
		return "just now"
	} else if s < 60*60 {
		n := s / 60
		plural := "s"
		if n == 1 {
			plural = ""
		}
		return fmt.Sprintf("%d minute%s ago", n, plural)
	} else if s < 24*60*60 {
		n := s / 3600
		plural := "s"
		if n == 1 {
			plural = ""
		}
		return fmt.Sprintf("%d hour%s ago", n, plural)
	} else {
		n := s / 86400
		plural := "s"
		if n == 1 {
			plural = ""
		}
		return fmt.Sprintf("%d day%s ago", n, plural)
	}
}

func writeList(ctx *context.Context, state State, on bool) {
	var sb strings.Builder
	if state.Checked == 0 {
		sb.WriteString("Nothing has checked for updates yet. `zigsaw updates check` does now.\n")
	} else {
		ago := time.Now().Unix() - state.Checked
		fmt.Fprintf(&sb, "Last checked %s.\n", FormatAgo(ago))
		if len(state.Apps) == 0 && state.Zigsaw == nil {
			sb.WriteString("Nothing is waiting.\n")
		}
		if len(state.Apps) > 0 {
			sb.WriteString("\nWaiting to be installed, with `zigsaw update`:\n")
			for _, app := range state.Apps {
				sb.WriteString("  ")
				_ = app.Write(&sb)
			}
		}
		if state.Zigsaw != nil {
			fmt.Fprintf(&sb, "\nzigsaw %s is out; this is %s. `zigsaw upgrade` installs it.\n", *state.Zigsaw, version.Version)
		}
		if len(state.Failed) > 0 {
			fmt.Fprintf(&sb, "\nCouldn't check %s: `zigsaw update --check` says why.\n", strings.Join(state.Failed, ", "))
		}
		if state.ZigsawFailed {
			sb.WriteString("\nCouldn't find out zigsaw's latest version.\n")
		}
	}

	if on {
		sb.WriteString("\nA daily check is on; it installs nothing. `zigsaw updates off` stops it.\n")
	} else {
		sb.WriteString("\nNo daily check is on. `zigsaw updates on` starts one, which installs nothing.\n")
	}

	fmt.Print(sb.String())
}

func NoteWaiting(ctx *context.Context) error {
	state := stillWaiting(ctx, load(ctx))
	count := len(state.Apps)
	if state.Zigsaw != nil {
		count++
	}
	if count > 0 {
		plural := "s"
		pronoun := "them"
		if count == 1 {
			plural = ""
			pronoun = "it"
		}
		context.Note("note: %d update%s waiting: `zigsaw updates` lists %s", count, plural, pronoun)
	}
	return nil
}
