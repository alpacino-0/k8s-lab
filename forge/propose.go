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
	"context"
	"errors"
	"fmt"
)

// ProposalBranch is the branch damga pushes the workflow to.
//
// Fixed, and that is what makes proposing twice safe: a second attempt finds
// the branch and the pull request it opened last time instead of opening a
// second one. A name with a timestamp or a random suffix in it would turn every
// retry — a timeout, a redeploy of the control plane, somebody pressing the
// button again — into another pull request in somebody else's repository.
const ProposalBranch = "damga/signing-workflow"

// Proposed is the pull request that carries the signing workflow.
type Proposed struct {
	URL    string
	Number int
	Branch string

	// Existing is true when this call found the pull request rather than
	// opening it. Reported rather than hidden because the panel should say
	// "your pull request is still open" and not claim to have made one.
	Existing bool
}

// Proposer opens the pull request that puts the signing workflow in a tenant's
// repository.
//
// It takes the connection and nothing else, deliberately. Handing it a file
// alongside would allow proposing a workflow that is not the one the policy
// pins, and the two agreeing is the property this whole package exists to hold
// — an implementation renders it from the connection with Workflow, the same
// call the policy is derived from.
//
// The interface is here rather than in the GitHub package because which forges
// can be reached is an open phase-2 question and the paid build reaching one
// this build cannot is exactly the seam that answers it.
type Proposer interface {
	Propose(ctx context.Context, c Connection) (Proposed, error)
}

// ErrNotPermitted is a forge that answered "no" rather than "broken": a token
// without the scope to open a pull request, a repository that has disabled
// them, an installation that was never granted this repository.
//
// Distinguished from a transport failure because the two need opposite
// responses. A refusal is something the tenant fixes by granting access; a
// failure is something that may work on the next attempt, and retrying a
// refusal forever is how a platform ends up rate-limited by a forge for asking
// the same forbidden question.
var ErrNotPermitted = errors.New("forge: not permitted to open a pull request")

// ProposalTitle is the pull request's subject.
const ProposalTitle = "Add a workflow that signs this repository's images"

// ProposalBody explains what merging does, to the person deciding whether to.
//
// Long for a generated pull request body, and that is the point: this arrives
// in a repository damga does not own, from an account its reviewer may not
// recognise, asking for permission to run on every push. A terse body reads
// like automation to be closed. The one thing it must land is that damga is
// asking to be *checked*, not to be trusted.
func ProposalBody(c Connection) string {
	return fmt.Sprintf(`This pull request adds one file: `+"`%s`"+`.

**What it does.** On every push to `+"`%s`"+`, it builds this repository into a
container image, pushes it to `+"`%s`"+`, and signs the digest it just pushed.
The signature is made with this repository's own GitHub Actions identity through
Sigstore — there is no key to store, leak or rotate, and the signature is
recorded in a public transparency log.

**What damga does with it.** It refuses to run an image in your cluster unless
that image carries a signature from exactly this file, on this branch. Nothing
else is accepted, including a workflow somebody adds later on another branch.

**What damga does not do.** It does not build your code, it does not sign
anything, and it holds no signing key. That is deliberate: a platform that
signed the images it also verifies would only be able to claim it trusts what it
produced. Verifying a signature it could not have made is a stronger claim, and
it is the reason this file has to live here rather than with us.

**Until you merge this**, the admission rule in your cluster records what it
would have rejected rather than rejecting it, so nothing breaks while this sits
open. It starts enforcing after the first image built by this workflow is seen.

**If you edit this file**, the identity it signs with changes and damga will
stop accepting images built from it until the rule is updated to match. The
file's name and the branch above are both part of that identity.

The identity this pins:

    %s
`, c.WorkflowPath, c.Branch, c.ImageRepository, c.Identity())
}
