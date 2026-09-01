/*
Copyright 2026 Orhan Yavuz.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

// show turns one response into output.
//
// With --json the server's bytes reach stdout untouched — not re-encoded, not
// reordered — because the reason to ask for JSON is to feed it to something
// else, and a client that rewrites the answer on the way past is a second
// source of truth. The renderer still runs, against io.Discard, so that a
// command whose answer decides the exit code decides it identically in both
// modes; verify is that command and it is the whole point of the product.
func (e *env) show(raw []byte, render func(io.Writer, []byte) error) error {
	w := e.stdout
	if e.jsonOut {
		if _, err := e.stdout.Write(raw); err != nil {
			return err
		}
		if len(raw) == 0 || raw[len(raw)-1] != '\n' {
			printline(e.stdout)
		}
		w = io.Discard
	}
	return render(w, raw)
}

// table is the one table format, so every list in this CLI lines up the same
// way and a column added to one does not silently change another.
func table(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
}

// when renders one of the API's RFC 3339 timestamps.
//
// A bad or empty timestamp becomes a dash rather than an error. The API renders
// the zero time as an empty string on purpose — a client that prints
// "0001-01-01" has been handed a date it will try to reason about — and this is
// the other half of that decision.
func when(stamp string) string {
	if strings.TrimSpace(stamp) == "" {
		return "—"
	}
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return stamp
	}
	return t.Local().Format("2006-01-02 15:04:05 MST")
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// short is a commit sha at the length git prints.
func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return dash(sha)
}

func renderMe(w io.Writer, body []byte, current string) error {
	var me meResponse
	if err := json.Unmarshal(body, &me); err != nil {
		return err
	}
	printf(w, "%s <%s>\n", displayName(me), me.Account.Email)
	if len(me.Memberships) == 0 {
		printline(w, "\nThis account is not a member of any tenant.")
		return nil
	}
	printline(w)
	t := table(w)
	printline(t, "  \tTENANT\tSLUG\tNAME\tROLE")
	for _, m := range me.Memberships {
		mark := " "
		if m.TenantID == current {
			// Which one every command without --tenant will use. Marked rather
			// than described, because the id is the thing to copy and a
			// sentence underneath would separate it from its row.
			mark = "*"
		}
		printf(t, "%s \t%s\t%s\t%s\t%s\n", mark, m.TenantID, m.TenantSlug, m.TenantName, m.Role)
	}
	return t.Flush()
}

// wireApp mirrors the app list's JSON. Named fields rather than a map, so that
// a field the API renames fails to compile here instead of rendering blank.
type wireApp struct {
	App       string `json:"app"`
	Env       string `json:"env"`
	State     string `json:"state"`
	RepoURL   string `json:"repoUrl"`
	Branch    string `json:"branch"`
	Path      string `json:"path"`
	Namespace string `json:"namespace"`
}

func renderApps(w io.Writer, body []byte) error {
	var out struct {
		Apps []wireApp `json:"apps"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return err
	}
	if len(out.Apps) == 0 {
		printline(w, "No apps. Register one with `damga-cli apps create`.")
		return nil
	}
	t := table(w)
	printline(t, "APP\tENV\tSTATE\tNAMESPACE\tREPOSITORY\tBRANCH\tPATH")
	for _, a := range out.Apps {
		printf(t, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			a.App, a.Env, a.State, dash(a.Namespace), dash(a.RepoURL), dash(a.Branch), dash(a.Path))
	}
	return t.Flush()
}

// wireRecord is the part of one evidence record this CLI prints.
//
// A subset, and deliberately not the whole thing: --json hands back everything
// the server sent, so what belongs here is the answer to "what is running and
// did it work", not a transcription of the wire format.
type wireRecord struct {
	Seq   int64  `json:"seq"`
	ID    string `json:"id"`
	State string `json:"state"`
	Actor struct {
		DisplayName string `json:"displayName"`
		Email       string `json:"email"`
	} `json:"actor"`
	Source struct {
		RepoURL   string `json:"repoUrl"`
		Ref       string `json:"ref"`
		Path      string `json:"path"`
		CommitSHA string `json:"commitSha"`
	} `json:"source"`
	Image struct {
		RequestedRef   string `json:"requestedRef"`
		AdmittedDigest string `json:"admittedDigest"`
	} `json:"image"`
	Admission struct {
		Allowed bool   `json:"allowed"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"admission"`
	Note      string `json:"note"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
	Hash      string `json:"hash"`
}

func renderRecord(w io.Writer, body []byte) error {
	var r wireRecord
	if err := json.Unmarshal(body, &r); err != nil {
		return err
	}
	t := table(w)
	printf(t, "Deploy\t%d (%s)\n", r.Seq, r.ID)
	printf(t, "State\t%s\n", r.State)
	printf(t, "Image\t%s\n", dash(r.Image.RequestedRef))
	if r.Image.AdmittedDigest != "" {
		printf(t, "Digest\t%s\n", r.Image.AdmittedDigest)
	}
	printf(t, "Commit\t%s\n", short(r.Source.CommitSHA))
	printf(t, "Source\t%s\n", dash(r.Source.RepoURL))
	printf(t, "By\t%s\n", dash(r.Actor.DisplayName))
	printf(t, "Opened\t%s\n", when(r.CreatedAt))
	printf(t, "Updated\t%s\n", when(r.UpdatedAt))
	if r.Note != "" {
		printf(t, "Note\t%s\n", r.Note)
	}
	// Printed only when something actually said so. A record carrying no
	// reason, not allowed, and not rejected is one nothing has observed yet —
	// and printing "refused" for it would be this client reaching a conclusion
	// the API never gave it. The panel had exactly that bug, on every deploy
	// that had not finished syncing, for as long as the page existed.
	if r.Admission.Allowed || r.Admission.Reason != "" || r.State == "rejected" {
		verdict := "refused"
		if r.Admission.Allowed {
			verdict = "admitted"
		}
		printf(t, "Admission\t%s %s\n", verdict, strings.TrimSpace(r.Admission.Reason))
		if r.Admission.Message != "" {
			printf(t, "\t%s\n", r.Admission.Message)
		}
	}
	return t.Flush()
}

func renderHistory(w io.Writer, body []byte) error {
	var out struct {
		Records []wireRecord `json:"records"`
		Next    string       `json:"next"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return err
	}
	if len(out.Records) == 0 {
		// An empty page and not a failure: "what has happened here" has an
		// answer for an app that has never deployed, and it is nothing.
		printline(w, "Nothing has been deployed here yet.")
		return nil
	}
	t := table(w)
	printline(t, "SEQ\tSTATE\tIMAGE\tCOMMIT\tBY\tUPDATED")
	for _, r := range out.Records {
		printf(t, "%d\t%s\t%s\t%s\t%s\t%s\n",
			r.Seq, r.State, dash(r.Image.RequestedRef), short(r.Source.CommitSHA),
			dash(r.Actor.DisplayName), when(r.UpdatedAt))
	}
	if err := t.Flush(); err != nil {
		return err
	}
	if out.Next != "" {
		printf(w, "\nMore: --after %s\n", out.Next)
	}
	return nil
}

// renderVerify prints what the server concluded and returns errChainBroken when
// the chain does not hold.
//
// It recomputes nothing. /verify is an endpoint precisely so that the page, the
// CLI and a script cannot reach three conclusions about one deploy, and a
// second implementation here — even a correct one — would be a fourth.
func renderVerify(w io.Writer, body []byte) error {
	var p struct {
		Records   int64  `json:"records"`
		FromSeq   int64  `json:"fromSeq"`
		ToSeq     int64  `json:"toSeq"`
		Valid     bool   `json:"valid"`
		BrokenAt  int64  `json:"brokenAt"`
		RootHash  string `json:"rootHash"`
		CheckedAt string `json:"checkedAt"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return err
	}
	t := table(w)
	printf(t, "Chain\t%s\n", map[bool]string{true: "verified", false: "BROKEN"}[p.Valid])
	printf(t, "Records\t%d (seq %d–%d)\n", p.Records, p.FromSeq, p.ToSeq)
	printf(t, "Root\t%s\n", dash(p.RootHash))
	printf(t, "Checked\t%s\n", when(p.CheckedAt))
	if !p.Valid {
		printf(t, "Broken at\tseq %d\n", p.BrokenAt)
	}
	if err := t.Flush(); err != nil {
		return err
	}
	if !p.Valid {
		return fmt.Errorf("%w: the chain breaks at seq %d", errChainBroken, p.BrokenAt)
	}
	return nil
}

func renderRetention(w io.Writer, body []byte) error {
	var p struct {
		WindowSeconds int64  `json:"windowSeconds"`
		KeepCurrent   bool   `json:"keepCurrent"`
		Immutable     bool   `json:"immutable"`
		Anchor        string `json:"anchor"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return err
	}
	t := table(w)
	window := "kept for ever"
	if p.WindowSeconds > 0 {
		window = (time.Duration(p.WindowSeconds) * time.Second).String()
	}
	printf(t, "Window\t%s\n", window)
	printf(t, "Keeps current\t%t\n", p.KeepCurrent)
	// Reported as the store reports it, including the false this build always
	// answers: making the history unmodifiable is a deployment decision — a
	// database role without UPDATE — that the server can neither make nor
	// observe, so it claims nothing.
	printf(t, "Immutable\t%t\n", p.Immutable)
	printf(t, "Anchor\t%s\n", dash(p.Anchor))
	return t.Flush()
}

func renderBackup(w io.Writer, body []byte) error {
	var b struct {
		Database   string `json:"database"`
		State      string `json:"state"`
		Rehearsed  bool   `json:"rehearsed"`
		FinishedAt string `json:"finishedAt"`
		Rows       int64  `json:"rows"`
		SourceRows int64  `json:"sourceRows"`
		Tables     int32  `json:"tables"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return err
	}
	t := table(w)
	printf(t, "Database\t%s\n", dash(b.Database))
	printf(t, "State\t%s\n", dash(b.State))
	printf(t, "Finished\t%s\n", when(b.FinishedAt))
	if b.Rehearsed {
		// The count against its source and never alone. "1,284 rows came back"
		// and "1,284 came back out of 1,284" are different claims and only the
		// second one was measured.
		printf(t, "Restored\t%d of %d rows across %d tables\n", b.Rows, b.SourceRows, b.Tables)
	} else {
		printline(t, "Restored\tthe archive was written and not restored")
	}
	return t.Flush()
}

// printf and printline are how this program writes to a terminal.
//
// The write error is dropped on purpose, and this is the one place that says
// why rather than seven places repeating `_, _ =`. Every one of these goes to a
// terminal, to a tabwriter whose Flush is checked, or to a test buffer. The
// failure that actually happens in practice is `damga-cli history | head`
// closing the pipe, and that arrives as SIGPIPE and ends the process before any
// returned error could be looked at. Branching on each write would be a dozen
// paths for a value this program is never in a position to act on.
func printf(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}

func printline(w io.Writer, a ...any) {
	_, _ = fmt.Fprintln(w, a...)
}
