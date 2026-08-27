//go:build !linux

package executor

import (
	"errors"
	"net"
	"os"
)

func peerUID(*net.UnixConn) (uint32, error) {
	return 0, errors.New("isolated executor requires Linux SO_PEERCRED")
}

func ownerUID(os.FileInfo) (uint32, error) {
	return 0, errors.New("isolated executor requires Linux file ownership checks")
}

func ownerGID(os.FileInfo) (uint32, error) {
	return 0, errors.New("isolated executor requires Linux file group checks")
}
