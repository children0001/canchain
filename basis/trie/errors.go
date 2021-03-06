//
// (at your option) any later version.
//
//

package trie

import (
	"fmt"

	"github.com/5uwifi/canchain/common"
)

type MissingNodeError struct {
	NodeHash common.Hash // hash of the missing node
	Path     []byte      // hex-encoded path to the missing node
}

func (err *MissingNodeError) Error() string {
	return fmt.Sprintf("missing trie node %x (path %x)", err.NodeHash, err.Path)
}
