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

// MergeState is what the tenant's own branch says about the workflow damga
// proposed.
type MergeState string

const (
	// MergeAbsent: the file is not on the branch. The pull request has not been
	// merged, or somebody removed the file afterwards. Those look identical
	// from here and both mean the same thing — nothing on that branch signs.
	MergeAbsent MergeState = "absent"

	// MergeMatches: the file is there and is byte-for-byte what damga renders.
	MergeMatches MergeState = "matches"

	// MergeDrifted: the file is there and has been edited.
	//
	// This is the one the identity cannot catch, and the reason this check
	// exists at all. The policy pins a workflow file at a ref, so a signature
	// made by an edited file at the same path on the same branch presents
	// exactly the accepted identity. The signature is genuine and the claim
	// behind it is somebody else's — an image built from a different
	// Dockerfile, a step that pulls its own base, a job that signs something it
	// did not build.
	MergeDrifted MergeState = "drifted"
)

// Merged is the answer, with enough detail to put on a page.
type Merged struct {
	State MergeState

	// Detail says what was found, in words a person can act on. Not a diff:
	// the file is the tenant's now, and quoting their edits back at them is
	// both rude and unnecessary — what they need to know is that damga stopped
	// vouching for what it signs.
	Detail string
}

// Proposer opens the pull request that puts the signing workflow in a tenant's
// repository, and reads back what became of it.
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

	// MergeStatus reads the workflow on the branch the identity names and says
	// whether it is the one damga wrote.
	//
	// On the same interface as Propose rather than its own, because they are
	// the same capability against the same repository with the same credential,
	// and a build that can do one and not the other would be a forge that
	// accepts writes and refuses reads.
	MergeStatus(ctx context.Context, c Connection) (Merged, error)
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

**If you rename or move this file, or change the branch above**, the identity it
signs with changes and damga stops accepting images built from it until the rule
is updated to match. Both are part of that identity.

**Editing what is inside it is different, and worth knowing.** The identity is
the file's path on this branch, so an edited file still signs as itself: the
signature stays genuine and damga keeps accepting it. What damga does instead is
notice — it reads this file back and says on your evidence page whether it is
still the one it wrote. It does not stop you editing it. It stops quietly
vouching for something it no longer recognises.

The identity this pins:

    %s
`, c.WorkflowPath, c.Branch, c.ImageRepository, c.Identity())
}
