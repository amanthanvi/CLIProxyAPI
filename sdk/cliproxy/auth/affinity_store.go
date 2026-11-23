package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const maxAffinityStateBytes = 16 << 20

// FileAffinityStore persists sensitive routing hints in an existing private directory.
// It does not create directories or write inside the credential store.
type FileAffinityStore struct {
	mu     sync.Mutex
	path   string
	rename func(int, string, string) error
}

func NewFileAffinityStore(path, authDir string) (*FileAffinityStore, error) {
	path = strings.TrimSpace(path)
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("affinity state path must be a clean absolute path")
	}
	if authDir != "" {
		authPath, err := filepath.Abs(authDir)
		if err != nil {
			return nil, errors.New("invalid auth directory")
		}
		authPath, err = filepath.EvalSymlinks(authPath)
		if err != nil {
			return nil, errors.New("cannot resolve auth directory for affinity isolation")
		}
		rel, err := filepath.Rel(authPath, path)
		if err != nil || rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
			return nil, errors.New("affinity state must be outside the auth directory")
		}
	}
	s := &FileAffinityStore{path: path}
	if err := s.checkPath(); err != nil {
		return nil, err
	}
	return s, nil
}

// Load rejects the whole checkpoint on corruption; callers may cold-route instead.
func (s *FileAffinityStore) Load() (AffinitySnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.read()
	if err != nil {
		return AffinitySnapshot{}, err
	}
	if len(data) > maxAffinityStateBytes {
		return AffinitySnapshot{}, errors.New("affinity state exceeds byte limit")
	}
	if err := rejectDuplicateAffinityJSON(data); err != nil {
		return AffinitySnapshot{}, err
	}
	var snapshot AffinitySnapshot
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return AffinitySnapshot{}, errors.New("invalid affinity state JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return AffinitySnapshot{}, errors.New("trailing affinity state data")
	}
	if snapshot.Version != AffinitySnapshotVersion {
		return AffinitySnapshot{}, errors.New("unsupported affinity state version")
	}
	return snapshot, nil
}

func rejectDuplicateAffinityJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var readValue func(int) error
	readValue = func(depth int) error {
		if depth > 32 {
			return errors.New("affinity JSON nesting limit exceeded")
		}
		token, err := decoder.Token()
		if err != nil {
			return errors.New("invalid affinity state JSON")
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			keys := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return errors.New("invalid affinity state JSON")
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("invalid affinity state JSON")
				}
				if _, exists := keys[key]; exists {
					return errors.New("duplicate affinity JSON field")
				}
				keys[key] = struct{}{}
				if err := readValue(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := readValue(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid affinity state JSON")
		}
		if _, err := decoder.Token(); err != nil {
			return errors.New("invalid affinity state JSON")
		}
		return nil
	}
	return readValue(0)
}

func (s *FileAffinityStore) Save(snapshot AffinitySnapshot) error {
	if snapshot.Version != AffinitySnapshotVersion {
		return errors.New("unsupported affinity state version")
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return errors.New("cannot encode affinity state")
	}
	if len(data)+1 > maxAffinityStateBytes {
		return errors.New("affinity state exceeds byte limit")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write(append(data, '\n'))
}
