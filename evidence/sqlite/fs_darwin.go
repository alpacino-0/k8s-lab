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

package sqlite

import (
	"fmt"
	"path/filepath"
	"syscall"
)

// Darwin reports the filesystem by name rather than by magic number.
func refuseNetworkFilesystem(path string) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(filepath.Dir(path), &st); err != nil {
		return nil //nolint:nilerr // not being able to tell is not a refusal
	}
	name := make([]byte, 0, len(st.Fstypename))
	for _, c := range st.Fstypename {
		if c == 0 {
			break
		}
		name = append(name, byte(c))
	}
	switch string(name) {
	case "nfs", "smbfs", "cifs", "afpfs", "webdav":
		return fmt.Errorf(
			"sqlite: %s is on a %s volume, where SQLite's locking is unreliable and corruption "+
				"is silent; use a local volume, or run against PostgreSQL", path, name)
	}
	return nil
}
