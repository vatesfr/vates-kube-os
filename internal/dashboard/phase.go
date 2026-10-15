package dashboard

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// PhaseFile is where PID 1 publishes what the node is doing, as one line:
// "<text>|<unix start>". The console reads it and shows it as an activity line,
// so a node that is waiting reads as waiting instead of as a wall of errors.
//
// It is PID 1's to write and the console's to read. The kubelet cannot produce
// this: it only knows the API is not answering yet, not that waiting is what was
// asked of it.
const PhaseFile = "/run/vates/phase"

// SetPhase publishes what the node is doing, for the console's activity line.
//
// It is the writing half of PhaseFile and is best-effort: a console that misses a
// phase still shows the node. PID 1 is the only caller, because it is the only
// one that knows what it is waiting for rather than only that nothing has
// answered yet.
func SetPhase(text string) {
	if err := os.MkdirAll(filepath.Dir(PhaseFile), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(PhaseFile,
		[]byte(fmt.Sprintf("%s|%d\n", text, time.Now().Unix())), 0o644)
}

// Phase is what PID 1 last published, and since when.
type Phase struct {
	Since time.Time
	Text  string
}

// ReadPhase reads the phase file. Nothing to show -- no file, or a phase of
// "ready" -- is reported as false: an activity line is for a node still coming
// up, not for one that has arrived.
func ReadPhase() (Phase, bool) {
	b, err := os.ReadFile(PhaseFile)
	if err != nil {
		return Phase{}, false
	}
	return parsePhase(b)
}

// parsePhase is ReadPhase without the file, so the format can be tested.
func parsePhase(b []byte) (Phase, bool) {
	text, rest, _ := strings.Cut(strings.TrimSpace(string(b)), "|")
	if text == "" || text == "ready" {
		return Phase{}, false
	}
	secs, _ := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
	return Phase{Text: text, Since: time.Unix(secs, 0)}, true
}

// Elapsed is how long the phase has been going, at the width a footer shows. It
// is empty when the start is unknown, rather than "0s" -- a phase that has no
// start time has no duration, and a zero would be a reading.
func (p Phase) Elapsed(now time.Time) string {
	if p.Since.IsZero() {
		return ""
	}
	return now.Sub(p.Since).Round(time.Second).String()
}

// Line is the activity line, or "" when there is nothing to say.
func (p Phase) Line(now time.Time) string {
	if p.Text == "" {
		return ""
	}
	if e := p.Elapsed(now); e != "" {
		return p.Text + " (" + e + ")"
	}
	return p.Text
}
