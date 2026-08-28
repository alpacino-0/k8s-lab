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

// Package github opens the signing-workflow pull request on github.com.
//
// Five calls against the REST API, and the interesting part of all of them is
// what happens the second time. This runs in a control plane: a request times
// out and is retried, an operator presses the button again, a pod restarts
// mid-flight. Every step here therefore treats "it is already like that" as
// success — because the alternative is a stranger's repository accumulating
// pull requests from us, which is the fastest way to have the integration
// turned off.
package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/damgahq/damga/forge"
)

// DefaultAPI is github.com's REST endpoint.
const DefaultAPI = "https://api.github.com"

// Client is a forge.Proposer for github.com.
type Client struct {
	// Token authorises the pull request. It needs contents:write and
	// pull_requests:write on the tenant's repository and nothing else.
	Token string

	// API defaults to DefaultAPI. Set by tests, and by nothing else — GitHub
	// Enterprise Server is a different question, because public Fulcio does
	// not accept its OIDC issuer and a pull request that lands there produces
	// signatures this platform cannot verify.
	API string

	// HTTP defaults to a client with a timeout. The zero http.Client has none,
	// and a control plane that blocks for ever on one tenant's forge is a
	// control plane one tenant can stop.
	HTTP *http.Client
}

var _ forge.Proposer = (*Client)(nil)

func (c *Client) api() string {
	if c.API == "" {
		return DefaultAPI
	}
	return strings.TrimSuffix(c.API, "/")
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Propose puts the workflow on a branch of its own and opens a pull request.
func (c *Client) Propose(ctx context.Context, conn forge.Connection) (forge.Proposed, error) {
	if err := conn.Validate(); err != nil {
		return forge.Proposed{}, err
	}
	if c.Token == "" {
		return forge.Proposed{}, fmt.Errorf(
			"%w: no token is configured for the tenant's forge", forge.ErrNotPermitted)
	}
	// Rendered here from the connection rather than accepted as an argument, so
	// the file proposed and the file the policy pins cannot be different.
	workflow, err := conn.Workflow()
	if err != nil {
		return forge.Proposed{}, err
	}

	// An open pull request from a previous attempt is the answer, and looking
	// first means a retry costs one call instead of four.
	if found, ok, err := c.openPull(ctx, conn); err != nil {
		return forge.Proposed{}, err
	} else if ok {
		found.Existing = true
		return found, nil
	}

	base, err := c.refSHA(ctx, conn, conn.Branch)
	if err != nil {
		return forge.Proposed{}, err
	}
	if err := c.ensureBranch(ctx, conn, base); err != nil {
		return forge.Proposed{}, err
	}
	if err := c.putFile(ctx, conn, workflow); err != nil {
		return forge.Proposed{}, err
	}
	return c.openPullRequest(ctx, conn)
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) (int, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.api()+path, rdr)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	// Pinned, because the unversioned API is free to change the shape of a
	// response under a running control plane.
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http().Do(req)
	if err != nil {
		return 0, fmt.Errorf("forge/github: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read before branching on the status, so an error carries what the forge
	// said. "403" on its own sends an operator to the wrong place; GitHub's
	// message names the missing scope.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized,
		resp.StatusCode == http.StatusForbidden,
		resp.StatusCode == http.StatusNotFound:
		// 404 belongs with the refusals and not with the failures. GitHub
		// answers 404 rather than 403 for a private repository the token
		// cannot see, so "no such repository" and "not yours" are the same
		// reply — and treating it as a transport failure would retry a
		// permission problem for ever.
		return resp.StatusCode, fmt.Errorf("%w: %s %s: %s",
			forge.ErrNotPermitted, method, path, message(raw))
	case resp.StatusCode >= 300:
		return resp.StatusCode, fmt.Errorf("forge/github: %s %s: %s: %s",
			method, path, resp.Status, message(raw))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("forge/github: decoding %s: %w", path, err)
		}
	}
	return resp.StatusCode, nil
}

// message pulls GitHub's own explanation out of an error body, falling back to
// the raw bytes. Truncated: a forge that answers with an HTML error page should
// not put a page in a log line.
func message(raw []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Message != "" {
		return e.Message
	}
	if len(raw) > 200 {
		return string(raw[:200]) + "…"
	}
	return string(raw)
}

func (c *Client) repoPath(conn forge.Connection) string {
	return "/repos/" + conn.Owner + "/" + conn.Repo
}

func (c *Client) refSHA(ctx context.Context, conn forge.Connection, branch string) (string, error) {
	var out struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if _, err := c.do(ctx, http.MethodGet,
		c.repoPath(conn)+"/git/ref/heads/"+url.PathEscape(branch), nil, &out); err != nil {
		return "", err
	}
	if out.Object.SHA == "" {
		return "", fmt.Errorf("forge/github: %s has no branch %q to open a pull request against",
			conn.Repo, branch)
	}
	return out.Object.SHA, nil
}

// ensureBranch creates the proposal branch, and treats "it exists" as done.
//
// It deliberately does not move an existing branch to the current base. A
// branch left from a previous attempt already carries the file, and resetting
// it would discard whatever the tenant may have pushed on top — a review
// comment acted on, a tweak to the image name. The pull request is theirs once
// it is open.
func (c *Client) ensureBranch(ctx context.Context, conn forge.Connection, baseSHA string) error {
	status, err := c.do(ctx, http.MethodPost, c.repoPath(conn)+"/git/refs", map[string]any{
		"ref": "refs/heads/" + forge.ProposalBranch,
		"sha": baseSHA,
	}, nil)
	if err == nil {
		return nil
	}
	// 422 is what GitHub answers for a reference that already exists.
	if status == http.StatusUnprocessableEntity {
		return nil
	}
	return err
}

// putFile writes the workflow onto the proposal branch, updating rather than
// failing when a previous attempt already put it there.
func (c *Client) putFile(ctx context.Context, conn forge.Connection, body []byte) error {
	path := c.repoPath(conn) + "/contents/" + conn.WorkflowPath

	// The blob SHA is required to replace a file and rejected when creating
	// one, so this asks first. A 404 here is the ordinary case — the file is
	// not there — and is not a refusal, which is why it is read from the
	// status rather than from the error.
	var existing struct {
		SHA     string `json:"sha"`
		Content string `json:"content"`
	}
	status, _ := c.do(ctx, http.MethodGet,
		path+"?ref="+url.QueryEscape(forge.ProposalBranch), nil, &existing)

	payload := map[string]any{
		"message": forge.ProposalTitle,
		"content": base64.StdEncoding.EncodeToString(body),
		"branch":  forge.ProposalBranch,
	}
	if status == http.StatusOK && existing.SHA != "" {
		if sameContent(existing.Content, body) {
			// Identical. Writing it again would be a commit that changes
			// nothing, on a branch somebody may be reviewing.
			return nil
		}
		payload["sha"] = existing.SHA
	}
	_, err := c.do(ctx, http.MethodPut, path, payload, nil)
	return err
}

// sameContent compares GitHub's base64 blob, which arrives wrapped in newlines,
// with what would be written.
func sameContent(encoded string, want []byte) bool {
	clean := strings.NewReplacer("\n", "", "\r", "").Replace(encoded)
	got, err := base64.StdEncoding.DecodeString(clean)
	return err == nil && bytes.Equal(got, want)
}

// openPull finds a pull request a previous attempt already opened.
func (c *Client) openPull(ctx context.Context, conn forge.Connection) (forge.Proposed, bool, error) {
	var out []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	q := "?state=open&head=" + url.QueryEscape(conn.Owner+":"+forge.ProposalBranch)
	if _, err := c.do(ctx, http.MethodGet, c.repoPath(conn)+"/pulls"+q, nil, &out); err != nil {
		return forge.Proposed{}, false, err
	}
	if len(out) == 0 {
		return forge.Proposed{}, false, nil
	}
	return forge.Proposed{
		URL: out[0].HTMLURL, Number: out[0].Number, Branch: forge.ProposalBranch,
	}, true, nil
}

func (c *Client) openPullRequest(ctx context.Context, conn forge.Connection) (forge.Proposed, error) {
	var out struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	_, err := c.do(ctx, http.MethodPost, c.repoPath(conn)+"/pulls", map[string]any{
		"title": forge.ProposalTitle,
		"body":  forge.ProposalBody(conn),
		"head":  forge.ProposalBranch,
		"base":  conn.Branch,
	}, &out)
	if err != nil {
		// A pull request created between the check at the top and here, by a
		// concurrent attempt. Answered by looking rather than by failing: the
		// caller asked for one to exist, and one does.
		if found, ok, findErr := c.openPull(ctx, conn); findErr == nil && ok {
			found.Existing = true
			return found, nil
		}
		return forge.Proposed{}, err
	}
	return forge.Proposed{URL: out.HTMLURL, Number: out.Number, Branch: forge.ProposalBranch}, nil
}
