package attest

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// stateMeaning explains each state in `check` output.
var stateMeaning = map[State]string{
	StateRecording: "snoopy is genuinely recording the active session",
	StateLocked:    "the supervised session is locked (KProxy: restricted)",
	StateNoSession: "the supervised user is not active on the seat (KProxy: restricted)",
	StateDegraded:  "recorder is genuine but recording evidence is incomplete (KProxy: allowed briefly, then blocked)",
	StateAbsent:    "recorder missing or failed an integrity check (KProxy: blocked)",
}

// WriteText prints an evaluation for administrators.
func WriteText(w io.Writer, ev *Evaluation, deviceID string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "state:   %s\n", ev.State)
	fmt.Fprintf(&b, "         %s\n", stateMeaning[ev.State])
	reasons := "none"
	if len(ev.Reasons) > 0 {
		reasons = strings.Join(ev.Reasons, ", ")
	}
	fmt.Fprintf(&b, "reasons: %s\n", reasons)
	fmt.Fprintf(&b, "user:    %s (uid %d)\n", ev.User, ev.UID)
	if deviceID != "" {
		fmt.Fprintf(&b, "device:  %s\n", deviceID)
	}
	b.WriteString("\n")
	for _, c := range ev.Checks {
		fmt.Fprintf(&b, "[%-4s] %-10s %s\n", strings.ToUpper(string(c.Status)), c.Name, c.Detail)
		for _, n := range c.Notes {
			fmt.Fprintf(&b, "       %-10s - %s\n", "", n)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// WriteJSON prints an evaluation as indented JSON.
func WriteJSON(w io.Writer, ev *Evaluation) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(ev)
}
