//go:build !windows

package fs

import "os"

func publishNewFile(source, target string) error {
	return os.Link(source, target)
}
