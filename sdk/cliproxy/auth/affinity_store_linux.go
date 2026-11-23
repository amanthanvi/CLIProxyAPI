//go:build linux

package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func affinityPrivateStat(fd int, directory bool) error {
	var stat unix.Stat_t
	if unix.Fstat(fd, &stat) != nil {
		return errors.New("cannot inspect affinity state permissions")
	}
	if stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o077 != 0 {
		return errors.New("affinity state owner or permissions are not private")
	}
	if directory && stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("affinity state parent is not a directory")
	}
	if !directory && stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("affinity state is not a regular file")
	}
	if !directory && stat.Mode&0o777 != 0o600 {
		return errors.New("affinity state file permissions must be 0600")
	}
	return nil
}

// Pin each component with openat: a replaced directory or symlink cannot redirect IO.
func (s *FileAffinityStore) openDir() (int, error) {
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, errors.New("cannot open affinity state root")
	}
	parts := strings.Split(strings.TrimPrefix(filepath.Dir(s.path), "/"), "/")
	for index, part := range parts {
		if part == "" {
			continue
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		_ = unix.Close(fd)
		if err != nil {
			return -1, errors.New("affinity state directory unavailable or symlinked")
		}
		fd = next
		var stat unix.Stat_t
		if unix.Fstat(fd, &stat) != nil {
			_ = unix.Close(fd)
			return -1, errors.New("cannot inspect affinity state ancestor")
		}
		// Root-owned sticky /tmp is safe because the private child cannot be replaced by other users.
		stickyRoot := stat.Uid == 0 && stat.Mode&unix.S_ISVTX != 0
		if (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) || (stat.Mode&0o022 != 0 && !stickyRoot) {
			_ = unix.Close(fd)
			return -1, errors.New("affinity state ancestor is not trusted")
		}
		if index == len(parts)-1 {
			if err := affinityPrivateStat(fd, true); err != nil {
				_ = unix.Close(fd)
				return -1, err
			}
		}
	}
	if err := affinityPrivateStat(fd, true); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func (s *FileAffinityStore) checkFinal(dir int) error {
	fd, err := unix.Openat(dir, filepath.Base(s.path), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return errors.New("affinity state file unavailable or symlinked")
	}
	defer unix.Close(fd)
	return affinityPrivateStat(fd, false)
}

func (s *FileAffinityStore) checkPath() error {
	dir, err := s.openDir()
	if err != nil {
		return err
	}
	defer unix.Close(dir)
	return s.checkFinal(dir)
}

func (s *FileAffinityStore) read() ([]byte, error) {
	dir, err := s.openDir()
	if err != nil {
		return nil, err
	}
	defer unix.Close(dir)
	fd, err := unix.Openat(dir, filepath.Base(s.path), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, errors.New("cannot open affinity state file")
	}
	file := os.NewFile(uintptr(fd), "affinity-state")
	defer file.Close()
	if err := affinityPrivateStat(fd, false); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxAffinityStateBytes+1))
	if err != nil {
		return nil, errors.New("cannot read affinity state")
	}
	return data, nil
}

func (s *FileAffinityStore) write(data []byte) error {
	dir, err := s.openDir()
	if err != nil {
		return err
	}
	defer unix.Close(dir)
	if err := s.checkFinal(dir); err != nil {
		return err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return errors.New("cannot create affinity temporary name")
	}
	tmp := ".session-affinity-" + hex.EncodeToString(nonce[:]) + ".tmp"
	fd, err := unix.Openat(dir, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return errors.New("cannot create affinity temporary file")
	}
	defer unix.Unlinkat(dir, tmp, 0)
	if err := unix.Fchmod(fd, 0o600); err != nil {
		_ = unix.Close(fd)
		return errors.New("cannot secure affinity temporary file")
	}
	file := os.NewFile(uintptr(fd), "affinity-temporary")
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return errors.New("cannot write affinity temporary file")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return errors.New("cannot sync affinity temporary file")
	}
	if err := file.Close(); err != nil {
		return errors.New("cannot close affinity temporary file")
	}
	if err := s.checkFinal(dir); err != nil {
		return err
	}
	rename := s.rename
	if rename == nil {
		rename = func(dir int, source, target string) error { return unix.Renameat(dir, source, dir, target) }
	}
	if err := rename(dir, tmp, filepath.Base(s.path)); err != nil {
		return errors.New("cannot replace affinity state file")
	}
	if err := unix.Fsync(dir); err != nil {
		return errors.New("cannot sync affinity state directory")
	}
	return nil
}
