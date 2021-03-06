//
// (at your option) any later version.
//
//

package params

import (
	"fmt"
)

const (
	VersionMajor = 0          // Major version component of the current release
	VersionMinor = 1          // Minor version component of the current release
	VersionPatch = 0         // Patch version component of the current release
	VersionMeta  = "unstable" // Version metadata to append to the version string
)

var Version = func() string {
	v := fmt.Sprintf("%d.%d.%d", VersionMajor, VersionMinor, VersionPatch)
	if VersionMeta != "" {
		v += "-" + VersionMeta
	}
	return v
}()

func VersionWithCommit(gitCommit string) string {
	vsn := Version
	if len(gitCommit) >= 8 {
		vsn += "-" + gitCommit[:8]
	}
	return vsn
}
