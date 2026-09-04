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

// Package panel is the free front end: the assets directory, embedded whole,
// and the tests beside it that are not.
//
// It said "three files" until there were seven. The count was written when the
// page was index.html, app.js and style.css, and every view added since —
// metrics, logs, catalogue, exec — walked past it. Replaced with a description
// rather than a larger number, because the next view will walk past that one
// too: //go:embed takes the directory, so nothing here has to be counted.
//
// There is no build step, no framework and no CDN, and that is a decision
// rather than a stage this has not reached yet.
//
//   - No npm. A Go product whose own interface needs a second toolchain has
//     two dependency trees to audit, two lockfiles to keep current and two
//     things that can break a release — for a page whose job is to render one
//     record and a list. This will not grow one.
//   - Nothing loaded from a CDN. An install that silently stops working
//     without a route to the internet is not a self-hosted product, it is a
//     hosted one with extra steps. Everything the page needs is in the binary.
//   - There are tests, and they need nothing installed. app_test.js runs under
//     node --test, which is built in — so the argument against a build step
//     holds without extending to an argument against being checked. It exists
//     because the one place this page decides anything decided wrongly for as
//     long as the page had existed, and nothing could have caught it: every
//     deploy that had not finished syncing rendered as refused, under a banner
//     explaining a refusal that had not happened.
//   - The panel decides nothing. It does not compute whether a chain is valid,
//     whether an image was signed, or whether a policy passed; it shows what
//     the API said. The moment it computes any of that, the panel and the CLI
//     can disagree about the same deploy, and the one people believe is the
//     one with the nicer typography.
package panel

import (
	"embed"
	"io/fs"
)

//go:embed assets
var assets embed.FS

// FS returns the bundle, rooted so that index.html is at "/".
func FS() fs.FS {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		// Unreachable: the directory is embedded above and checked at compile
		// time. A panic here would mean the binary was built without it.
		panic("panel: " + err.Error())
	}
	return sub
}
