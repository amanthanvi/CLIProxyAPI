//go:build !linux

package auth

import "errors"

func (s *FileAffinityStore) checkPath() error {
	return errors.New("affinity checkpoint persistence currently requires Linux private-file validation")
}

func (s *FileAffinityStore) read() ([]byte, error) { return nil, s.checkPath() }
func (s *FileAffinityStore) write([]byte) error    { return s.checkPath() }
