//go:build !darwin && !linux

package updater

import (
	"errors"
	"os"
)

func tryCacheLock(path string) (release func(), acquired bool, err error) {
	exclusivePath := path + ".exclusive"
	file, err := os.OpenFile(exclusivePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return func() {
		_ = file.Close()
		_ = os.Remove(exclusivePath)
	}, true, nil
}
