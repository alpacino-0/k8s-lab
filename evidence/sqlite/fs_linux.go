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

// Magic numbers from statfs(2). NFS and the SMB/CIFS family are the ones that
// matter: they are what a homelab PVC is usually backed by, and they are
// exactly where SQLite's own documentation says not to put a database — POSIX
// advisory locking over them is unreliable, and the failure is silent
// corruption rather than an error.
const (
	nfsSuper  = 0x6969
	smbSuper  = 0x517B
	cifsMagic = 0xFF534D42
	smb2Magic = 0xFE534D42
)

func refuseNetworkFilesystem(path string) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(filepath.Dir(path), &st); err != nil {
		// Not being able to tell is not a reason to refuse. A wrong refusal
		// costs an install; a missed one costs a database months later.
		return nil //nolint:nilerr // see comment
	}
	switch int64(st.Type) {
	case nfsSuper, smbSuper, cifsMagic, smb2Magic:
		return fmt.Errorf(
			"sqlite: %s is on a network filesystem, where SQLite's locking is unreliable and "+
				"corruption is silent; use a local volume, or run against PostgreSQL", path)
	}
	return nil
}
