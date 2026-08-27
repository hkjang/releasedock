//go:build !linux

package executor

import (
	"errors"
	"os"
)

func AcquireRunnerHostLock(string) (*os.File, error) {
	return nil, errors.New("Runner host locking requires Linux")
}
