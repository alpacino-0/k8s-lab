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

package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Format is the shape of the body one receiver expects.
type Format string

const (
	// FormatAuto reads the shape off the host. See detect.
	FormatAuto Format = "auto"
	// FormatSlack is {"text": …}, which Slack's incoming webhooks take.
	FormatSlack Format = "slack"
	// FormatDiscord is {"content": …}. The field name is the whole difference,
	// and sending Slack's to Discord is a 400 rather than a silent drop.
	FormatDiscord Format = "discord"
	// FormatWebhook is the event itself, plus the sentence the other two carry.
	// Anything that is not one of the two chat services gets this.
	FormatWebhook Format = "webhook"
)

// defaultTimeout bounds one delivery.
//
// Bounded and synchronous rather than queued, and the trade is worth writing
// down: the caller is the deploy observer, so a slow receiver holds one
// reconcile for up to this long. A queue would free it and would then own a
// buffer that drops on overflow, a worker to drain it and a shutdown that has
// to flush it — three things to get wrong for a message that is late rather
// than lost. Transitions worth notifying arrive once per deploy, not per
// second, so the bound is the cheaper answer.
const defaultTimeout = 5 * time.Second

// Webhook posts one event to one URL.
type Webhook struct {
	url    string
	format Format
	client *http.Client
}

// NewWebhook reads the URL out of a file and refuses everything it cannot send
// to.
//
// A file and not a flag value or an environment variable, which is the pattern
// Config.GitTokenFile already carries and for the same measured reasons: a flag
// is in the process table and in the shell history, an environment variable is
// in /proc/<pid>/environ, in a crash dump and in `kubectl describe pod`. A
// Slack or Discord webhook URL is a bearer credential — anyone holding it can
// post into that channel as you — so it is exactly the kind of string that must
// not be in argv.
//
// Every refusal here happens at startup rather than at the first failed deploy.
// A control plane that only discovers its webhook is unusable at the moment
// something has gone wrong is a control plane that tells you nothing at the one
// moment you needed it to.
func NewWebhook(urlFile string, format Format, timeout time.Duration) (*Webhook, error) {
	raw, err := os.ReadFile(urlFile)
	if err != nil {
		return nil, fmt.Errorf("reading the notification URL from %s: %w", urlFile, err)
	}
	// Trimmed, because a file written by an editor or by `echo` ends with a
	// newline, and a newline inside a URL is rejected by net/http with an error
	// that says nothing about which file it came from.
	target := strings.TrimSpace(string(raw))
	if target == "" {
		return nil, fmt.Errorf("the notification URL file %s is empty", urlFile)
	}

	parsed, err := url.Parse(target)
	if err != nil {
		// The URL itself is never quoted back. It is the credential, and an
		// error message is the one place a credential reliably ends up in a
		// log that outlives the process.
		return nil, fmt.Errorf("the notification URL in %s is not a URL", urlFile)
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		return nil, fmt.Errorf("the notification URL in %s has scheme %q; only http and https are sent to",
			urlFile, parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("the notification URL in %s has no host", urlFile)
	}

	if format == "" {
		format = FormatAuto
	}
	if format == FormatAuto {
		format = detect(parsed.Host)
	}
	switch format {
	case FormatSlack, FormatDiscord, FormatWebhook:
	default:
		return nil, fmt.Errorf("notification format %q is not one of slack, discord, webhook or auto", format)
	}

	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Webhook{
		url:    target,
		format: format,
		client: &http.Client{
			Timeout: timeout,
			// Not followed. A redirect on a webhook post means something other
			// than the receiver answered, and following it would replay a body
			// carrying an app name and a commit at whatever host it named.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// Format reports the shape this webhook sends, which is worth asking for after
// FormatAuto has resolved it — the startup log prints it so that a body being
// refused is traceable to a detection rather than a guess.
func (w *Webhook) Format() Format { return w.format }

// Host reports the receiver's host and nothing else of the URL. The path is
// where the secret is in both Slack's and Discord's webhooks, so this is the
// most that can be logged.
func (w *Webhook) Host() string {
	if parsed, err := url.Parse(w.url); err == nil {
		return parsed.Host
	}
	return "the configured host"
}

// detect reads the body shape off the host.
//
// Two hosts and a default. Guessing is worth it because the alternative is a
// second flag that has to agree with the URL, and the mistake that flag invites
// — a Slack URL declared as discord — fails with a 400 the operator has to
// connect back to a setting they set weeks ago. A wrong guess here fails the
// same way and is one line to override with -notify-format.
func detect(host string) Format {
	switch {
	case strings.HasSuffix(host, "hooks.slack.com"), strings.HasSuffix(host, "slack.com"):
		return FormatSlack
	case strings.HasSuffix(host, "discord.com"), strings.HasSuffix(host, "discordapp.com"):
		return FormatDiscord
	default:
		return FormatWebhook
	}
}

// Notify posts the event and reports which way it failed.
func (w *Webhook) Notify(ctx context.Context, e Event) error {
	body, err := json.Marshal(w.body(e))
	if err != nil {
		return fmt.Errorf("encoding the notification: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building the notification request for %s: %w", w.Host(), withoutURL(err))
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.client.Do(req)
	if err != nil {
		// The request never arrived: DNS, a refused connection, a timeout, TLS.
		// Named apart from a receiver that answered, because one of those is
		// fixed in the network and the other in the receiver — and "the
		// notification was not sent" is equally true of both, which is why this
		// message does not say that.
		return fmt.Errorf("%s never answered: %w", w.Host(), withoutURL(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The receiver's own words, bounded. Slack answers `invalid_payload`
		// and Discord answers a JSON error naming the field, and both of those
		// say more than the status does — a 400 alone reads as "damga sent
		// something wrong" without saying what, which is the sentence this
		// repository keeps paying a round for.
		said, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		detail := strings.TrimSpace(string(said))
		if detail == "" {
			detail = "and said nothing"
		}
		return fmt.Errorf("%s refused the %s body with %d: %s",
			w.Host(), w.format, resp.StatusCode, detail)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// withoutURL drops the URL out of a transport error.
//
// net/http wraps every failure in *url.Error, which formats as
// `Post "<the whole URL>": …` — and for Slack and Discord the whole URL is the
// credential, because both put the token in the path. So the error this package
// returns, which the store then writes to a log somebody else can read, carried
// the secret.
//
// Found by the test that puts a token in the path and asserts it never comes
// back out, on the first run of that test. The host is deliberately kept: it is
// not the secret, and it is the thing that says which receiver went quiet.
func withoutURL(err error) error {
	var wrapped *url.Error
	if errors.As(err, &wrapped) && wrapped.Err != nil {
		return wrapped.Err
	}
	return err
}

// body is what each receiver is handed.
func (w *Webhook) body(e Event) any {
	text := Sentence(e)
	switch w.format {
	case FormatSlack:
		return map[string]string{"text": text}
	case FormatDiscord:
		return map[string]string{"content": text}
	default:
		// The fields and the sentence. A receiver that routes on the app name
		// needs the fields; one that pastes into a channel needs the sentence;
		// and a receiver written against only one of them keeps working when
		// the other changes.
		return map[string]any{
			"text":   text,
			"source": "damga",
			"event": map[string]any{
				"tenant": e.Tenant, "app": e.App, "env": e.Env,
				"state": e.State, "seq": e.Seq,
				"image": e.Image, "commit": e.Commit, "actor": e.Actor,
				"reason": e.Reason, "at": e.At.UTC().Format(time.RFC3339),
			},
		}
	}
}

// Sentence is the one line every format carries.
//
// Exported so the test that asserts a receiver can read the app name out of any
// of the three bodies asserts it against the same string the product sends.
//
// It leads with the app and the environment because that is what somebody
// scanning a channel is looking for, and it carries the reason because "api/prod
// failed" is an interruption while "api/prod failed: the commit was never
// pushed" is an instruction.
func Sentence(e Event) string {
	var b strings.Builder
	b.WriteString("damga: ")
	b.WriteString(e.App)
	b.WriteString("/")
	b.WriteString(e.Env)
	fmt.Fprintf(&b, " deploy %d is %s", e.Seq, e.State)
	if e.Image != "" {
		b.WriteString(" — ")
		b.WriteString(e.Image)
	}
	if e.Reason != "" {
		b.WriteString(" (")
		b.WriteString(e.Reason)
		b.WriteString(")")
	}
	return b.String()
}
