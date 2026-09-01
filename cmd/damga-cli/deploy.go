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
	"io"

	"github.com/spf13/cobra"
)

// buildCmd asks for one commit to be turned into one image.
func (e *env) buildCmd() *cobra.Command {
	var req struct {
		Repo     string `json:"repo"`
		Revision string `json:"revision"`
		Path     string `json:"path,omitempty"`
		Builder  string `json:"builder,omitempty"`
		Image    string `json:"image,omitempty"`
	}
	cmd := &cobra.Command{
		Use:   "build <app>",
		Short: "Ask the cluster to turn one commit into one image",
		Long: `build creates a Build in the cluster. It is the one thing this platform does
that writes to the cluster rather than to git, because a build has to happen
before there is a digest to commit.

  damga-cli build api --repo https://github.com/acme/api \
      --revision 5f1e0c1d0f9b7a2c3d4e5f60718293a4b5c6d7e8

The revision is a full 40-character commit sha and never a branch: a record
that says "built main" cannot answer which main, which is the only question
anybody asks of a build afterwards. --image is where the result is pushed,
without a tag; left out, the platform's own registry is used.

An installation whose control plane holds no permission to create builds
answers 501 and says so, rather than reporting a build it did not start.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, sess, err := e.signedIn()
			if err != nil {
				return err
			}
			tenant, err := e.tenantOf(sess)
			if err != nil {
				return err
			}
			var raw json.RawMessage
			if err := c.do(cmd.Context(), call{
				route:  routeBuild,
				target: target{tenant: tenant, app: args[0]},
				body:   req,
				out:    &raw,
			}); err != nil {
				return err
			}
			return e.show(raw, renderBuild)
		},
	}
	f := cmd.Flags()
	f.StringVar(&req.Repo, "repo", "", "the repository to clone (https:// or git@)")
	f.StringVar(&req.Revision, "revision", "", "the full 40-character commit sha to build")
	f.StringVar(&req.Path, "path", "", "the directory inside the repository to build from")
	f.StringVar(&req.Builder, "builder", "", "detect, dockerfile or buildpack (default: detect)")
	f.StringVar(&req.Image, "image", "",
		"where to push, without a tag (default: this platform's registry)")
	return cmd
}

func renderBuild(w io.Writer, body []byte) error {
	var out struct {
		Build struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
			Image     string `json:"image"`
			Revision  string `json:"revision"`
			Builder   string `json:"builder"`
		} `json:"build"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return err
	}
	b := out.Build
	// Accepted, and said as such. The digest — the only part anybody wants —
	// arrives minutes later on the Build's own status, so a message here that
	// read like a finished build would be a lie with a happy ending.
	printf(w, "Build %s accepted in %s.\n", b.Name, b.Namespace)
	t := table(w)
	printf(t, "Image\t%s\n", b.Image)
	printf(t, "Revision\t%s\n", short(b.Revision))
	printf(t, "Builder\t%s\n", b.Builder)
	return t.Flush()
}

// deployCmd commits a new desired state for one app environment.
//
// Only --image is required. Everything else left out keeps whatever is
// committed, which is the server's read-modify-write against git and not a
// default this client invents — and it is why the optional fields are sent only
// when their flag was actually given. Without that distinction a command that
// does not mention replicas and a command asking for zero replicas are the same
// request, and one of those two meanings is "take the app down".
func (e *env) deployCmd() *cobra.Command {
	var (
		image    string
		note     string
		port     int32
		replicas int32
		domain   string
	)
	cmd := &cobra.Command{
		Use:   "deploy <app> <env>",
		Short: "Commit a new image for one app environment",
		Long: `deploy writes a commit to the tenant's state repository. It touches no
cluster: Argo CD applies what was committed and admission is the last gate, so
what comes back is a pending record rather than a success.

  damga-cli deploy api prod --image registry.example.com/acme/api@sha256:...

Fields left out keep whatever is already committed. A flag that is not given is
not sent, so --replicas 0 means zero replicas and no --replicas means "leave it
alone".`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, sess, err := e.signedIn()
			if err != nil {
				return err
			}
			tenant, err := e.tenantOf(sess)
			if err != nil {
				return err
			}

			req := map[string]any{"image": image}
			if note != "" {
				req["note"] = note
			}
			// Changed and not a zero check, which is the whole point: cobra
			// knows whether the flag appeared, and the zero value of two of
			// these three is a request somebody might genuinely mean.
			if cmd.Flags().Changed("port") {
				req["port"] = port
			}
			if cmd.Flags().Changed("replicas") {
				req["replicas"] = replicas
			}
			if cmd.Flags().Changed("domain") {
				req["domain"] = domain
			}

			var raw json.RawMessage
			if err := c.do(cmd.Context(), call{
				route:  routeDeploy,
				target: target{tenant: tenant, app: args[0], env: args[1]},
				body:   req,
				out:    &raw,
			}); err != nil {
				return err
			}
			return e.show(raw, func(w io.Writer, body []byte) error {
				printline(w, "Committed. Nothing has been applied yet — this is what was recorded:")
				printline(w)
				return renderRecord(w, body)
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&image, "image", "", "the image to run (required)")
	f.StringVar(&note, "note", "", "a note recorded with the deploy")
	f.Int32Var(&port, "port", 0, "the port the container listens on")
	f.Int32Var(&replicas, "replicas", 0, "how many replicas to run")
	f.StringVar(&domain, "domain", "", "the domain to serve it on")
	return cmd
}
