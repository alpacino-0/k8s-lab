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

// appsCmd lists this tenant's apps, and carries create and delete under it.
func (e *env) appsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apps",
		Short: "List this tenant's apps",
		Long: `apps lists every app environment this tenant has, from both places that know
about one, and says which of them knew:

  deployed        placed, and something has been deployed to it
  never deployed  placed, and nothing has been deployed yet
  record removed  deployed to, and no longer placed — the objects and the
                  deploy history outlive the registration

The three are the server's own words and are printed as it sends them.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
				route: routeApps, target: target{tenant: tenant}, out: &raw,
			}); err != nil {
				return err
			}
			return e.show(raw, renderApps)
		},
	}
	cmd.AddCommand(e.appsCreateCmd(), e.appsDeleteCmd())
	return cmd
}

// appsCreateCmd registers where one app environment lives.
//
// Every flag is required and none is guessed, which is the server's rule and
// not this client's — it refuses a request that leaves one out. Repeating the
// requirement here would be a second copy of a decision that has already been
// written down once, so what this does instead is name the flags and let the
// refusal come back in the server's own words.
func (e *env) appsCreateCmd() *cobra.Command {
	var req struct {
		App       string `json:"app"`
		Env       string `json:"env"`
		RepoURL   string `json:"repoUrl"`
		Branch    string `json:"branch"`
		Path      string `json:"path"`
		Namespace string `json:"namespace"`
	}
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Register an app environment and where its manifests are written",
		Long: `create records where one app environment lives. It writes to the control
plane's database and to nothing else: no commit, no cluster. The first commit is
made by the first deploy.

  damga-cli apps create --app api --env prod \
      --repo https://github.com/acme/state --branch main \
      --path apps/api/prod --namespace acme-prod

The repository is the tenant's STATE repository — where damga commits manifests
— and not the repository a build clones.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
				route: routeCreateApp, target: target{tenant: tenant}, body: req, out: &raw,
			}); err != nil {
				return err
			}
			return e.show(raw, func(w io.Writer, body []byte) error {
				var out struct {
					App wireApp `json:"app"`
				}
				if err := json.Unmarshal(body, &out); err != nil {
					return err
				}
				printf(w, "Registered %s/%s in namespace %s.\n",
					out.App.App, out.App.Env, out.App.Namespace)
				printf(w, "Manifests will be written to %s %s under %s.\n",
					out.App.RepoURL, dash(out.App.Branch), dash(out.App.Path))
				return nil
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&req.App, "app", "", "the app's name (a DNS label)")
	f.StringVar(&req.Env, "env", "", "the environment's name (a DNS label)")
	f.StringVar(&req.RepoURL, "repo", "", "the state repository manifests are committed to")
	f.StringVar(&req.Branch, "branch", "", "the branch to commit on")
	f.StringVar(&req.Path, "path", "", "the directory inside the repository")
	f.StringVar(&req.Namespace, "namespace", "", "the Kubernetes namespace to deploy into")
	return cmd
}

// appsDeleteCmd removes the registration and not the app.
func (e *env) appsDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <app>",
		Short: "Forget where an app lives, keeping what is running and what happened",
		Long: `delete removes the registration for every environment of one app.

It deletes the record and not the app. The manifests stay committed, whatever is
running keeps running, the database keeps its data and its backups, and the
deploy history stays readable — the app still appears in ` + "`damga-cli apps`" + `, as
"record removed". What is gone is the platform's answer to where the next deploy
would go, so the next deploy refuses rather than guessing.

What was removed is printed, because after this call there are objects in a
namespace and files in a repository that nothing points at any more, and whoever
ran this is the last person in a position to be told where they are.`,
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
				route:  routeDeleteApp,
				target: target{tenant: tenant, app: args[0]},
				out:    &raw,
			}); err != nil {
				return err
			}
			return e.show(raw, renderRemoved)
		},
	}
}

func renderRemoved(w io.Writer, body []byte) error {
	var out struct {
		App     string `json:"app"`
		Removed []struct {
			Env       string `json:"env"`
			RepoURL   string `json:"repoUrl"`
			Path      string `json:"path"`
			Namespace string `json:"namespace"`
		} `json:"removed"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return err
	}
	printf(w, "Unregistered %s. Still running, and still committed:\n\n", out.App)
	t := table(w)
	printline(t, "ENV\tNAMESPACE\tREPOSITORY\tPATH")
	for _, r := range out.Removed {
		printf(t, "%s\t%s\t%s\t%s\n", r.Env, dash(r.Namespace), dash(r.RepoURL), dash(r.Path))
	}
	return t.Flush()
}
