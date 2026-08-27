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

package forge

import (
	"bytes"
	"fmt"
	"text/template"
)

// Workflow renders the file damga proposes to add to the tenant's repository.
//
// Written as text and not marshalled from a struct, which is the opposite of
// how this repository renders the manifests it owns. The difference is the
// reader: this file arrives as a pull request that a person has to understand
// well enough to approve, in a repository that is theirs and not ours. A
// machine-serialised blob with the keys in map order is a pull request that
// gets closed, and this whole design rests on it being merged. So the comments
// in the output are part of the deliverable, not decoration.
//
// It is also the reason the workflow is deliberately small. Every step it does
// not have is a step nobody has to review, and the only steps that earn their
// place are the ones that make the signature mean something.
//
// One trap for anything that reads this file back — and phase 2 has an open
// item that will: the trigger key is written bare, as `on:`, because that is
// what every GitHub workflow looks like and this one arrives in front of a
// reviewer. In YAML 1.1 `on` is a boolean, so a Go parser going through
// sigs.k8s.io/yaml sees the key "true" rather than "on". Measured, not guessed:
// YAMLToJSON turns `on:\n  push:` into {"true":{"push":...}}. GitHub's own
// parser is not affected and neither is the signature. Quoting the key would
// make the Go side simpler and the pull request stranger, and the pull request
// is the part that has to be merged.
func (c Connection) Workflow() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := workflowTemplate.Execute(&buf, c); err != nil {
		return nil, fmt.Errorf("forge: rendering workflow: %w", err)
	}
	return buf.Bytes(), nil
}

var workflowTemplate = template.Must(template.New("workflow").Parse(
	`# Added by damga. This is the only file damga asks to put in this repository.
#
# What it does: builds this repository into a container image, pushes it, and
# signs the digest it just pushed. The signature is made with this repository's
# own GitHub Actions identity — damga never signs anything and holds no key.
# Nothing here reports back to damga.
#
# What damga does with it: refuses to run an image in your cluster unless it
# carries a signature from exactly this file, on this branch. That check happens
# in your cluster at admission time, against the public transparency log.
#
# Editing this file changes the identity it signs with, and damga will stop
# accepting images built from it until the policy is updated to match. The name
# of the file and the branch below are both part of that identity.

name: Build and sign

on:
  push:
    branches: [{{ .Branch }}]

# id-token is what makes this keyless: GitHub mints a short-lived OIDC token,
# Fulcio exchanges it for a certificate naming this workflow, and the signature
# is anchored in a public log. No key is stored anywhere, so there is no key to
# leak and none to rotate.
permissions:
  contents: read
  id-token: write
  packages: write

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - uses: docker/login-action@v3
        with:
          registry: {{ .RegistryHost }}
          username: ${{"{{"}} github.actor {{"}}"}}
          password: ${{"{{"}} secrets.GITHUB_TOKEN {{"}}"}}

      # The digest is the output that matters. A tag can be moved to point at a
      # different image after it is signed; a digest cannot.
      - id: build
        uses: docker/build-push-action@v6
        with:
          context: .
          push: true
          tags: {{ .ImageRepository }}:${{"{{"}} github.sha {{"}}"}}

      - uses: sigstore/cosign-installer@v3

      # Signed by digest, never by tag, for the same reason the digest is the
      # output that matters above.
      - run: cosign sign --yes "{{ .ImageRepository }}@${{"{{"}} steps.build.outputs.digest {{"}}"}}"
`))

// RegistryHost is the host part of the image repository, which the login step
// needs on its own.
//
// A method on the connection rather than a field, because it is not an
// independent fact: a registry that disagrees with the repository being pushed
// to is a login to somewhere the build does not use, and that fails at push
// time with an error about permissions rather than about configuration.
func (c Connection) RegistryHost() string {
	if i := indexByte(c.ImageRepository, '/'); i > 0 {
		return c.ImageRepository[:i]
	}
	return c.ImageRepository
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
