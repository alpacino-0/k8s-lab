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

// Package panel is the free front end: three files, embedded in the binary.
//
// There is no build step, no framework and no CDN, and that is a decision
// rather than a stage this has not reached yet.
//
//   - No npm. A Go product whose own interface needs a second toolchain has
//     two dependency trees to audit, two lockfiles to keep current and two
//     things that can break a release — for a page whose job is to render one
//     record and a list. The core will not grow one; a paid panel that wants
//     React supplies its own bundle through server.Options.Panel.
//   - Nothing loaded from a CDN. Air-gapped installation is a paid feature,
//     but a free install that silently stops working without a route to the
//     internet is not a free install, it is a hosted product with extra steps.
//     Everything the page needs is in the binary.
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
