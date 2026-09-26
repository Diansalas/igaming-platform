//go:build !unix

package devfile

import "io/fs"

// The devfile backend refuses to construct outside Unix (security review
// §4.2): owner and permission checks need Unix file metadata.
const supported = false

const openNoFollow = 0

func platformOwner(fs.FileInfo) (uint32, bool) { return 0, false }

func platformEUID() uint32 { return 0 }
