package cliproxy

import (
	"errors"
	"os"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
)

const affinityCheckpointInterval = 30 * time.Second

// Configuration changes never import disk hints over newer live bindings.
// An invalid new path retains the last working destination.
func (s *Service) configureAffinityStateStore(cfg *config.Config, restore bool) {
	if s == nil || s.coreManager == nil || cfg == nil {
		return
	}
	path := strings.TrimSpace(cfg.Routing.SessionAffinityStatePath)
	if !cfg.Routing.SessionAffinity || cfg.Home.Enabled {
		path = ""
	}
	s.affinityMu.Lock()
	defer s.affinityMu.Unlock()
	if s.affinityStopping {
		return
	}
	if !restore && !s.affinityInitialized {
		return
	}
	if restore {
		s.affinityInitialized = true
	}
	if path == "" {
		s.affinityStore, s.affinityPath = nil, ""
		return
	}
	if path == s.affinityPath && s.affinityStore != nil {
		return
	}
	if !restore && s.affinityInitialized && !s.affinityDrainEnabled {
		log.Warn("enabling affinity checkpoints requires a service restart")
		return
	}
	store, err := coreauth.NewFileAffinityStore(path, cfg.AuthDir)
	if err != nil {
		log.Warn("affinity checkpoint configuration rejected; retaining previous persistence destination")
		return
	}
	if restore {
		snapshot, errLoad := store.Load()
		if errLoad == nil {
			if errRestore := s.coreManager.RestoreAffinity(snapshot, time.Now()); errRestore != nil {
				log.Warn("affinity checkpoint rejected; starting with cold routing hints")
			} else if snapshot.ClaudeDates != nil {
				if errDates := helps.RestoreClaudeSessionDates(*snapshot.ClaudeDates, time.Now()); errDates != nil {
					log.Warn("affinity checkpoint date pins rejected")
				}
			}
		} else if !errors.Is(errLoad, os.ErrNotExist) {
			log.Warn("affinity checkpoint unavailable or invalid; starting with cold routing hints")
		}
	}
	s.affinityStore, s.affinityPath = store, path
	s.coreManager.EnableAffinityDrainTracking()
	s.affinityDrainEnabled = true
	if s.affinityStop == nil && s.affinityInitialized {
		s.affinityStop, s.affinityDone = make(chan struct{}), make(chan struct{})
		go s.runAffinityCheckpoints(s.affinityStop, s.affinityDone)
	}
}

func (s *Service) runAffinityCheckpoints(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(affinityCheckpointInterval)
	defer ticker.Stop()
	s.runAffinityCheckpointTicks(stop, ticker.C)
}

func (s *Service) runAffinityCheckpointTicks(stop <-chan struct{}, ticks <-chan time.Time) {
	for {
		select {
		case <-stop:
			return
		case <-ticks:
			if err := s.checkpointAffinity(); err != nil {
				log.Warn("affinity checkpoint failed; previous checkpoint retained unless replacement already completed")
			}
		}
	}
}

func (s *Service) checkpointAffinity() error {
	s.affinityMu.Lock()
	defer s.affinityMu.Unlock()
	if s.affinityStore == nil || s.coreManager == nil {
		return nil
	}
	snapshot, err := s.coreManager.SnapshotAffinity(time.Now())
	if err != nil {
		return errors.New("affinity snapshot unavailable")
	}
	if snapshot.Truncated {
		log.Warn("affinity checkpoint reached capacity; oldest hints omitted")
	}
	dates, err := helps.SnapshotClaudeSessionDates(time.Now())
	if err != nil {
		return errors.New("affinity date snapshot unavailable")
	}
	snapshot.ClaudeDates = &dates
	if err := s.affinityStore.Save(snapshot); err != nil {
		return errors.New("affinity checkpoint write failed")
	}
	return nil
}

func (s *Service) stopAffinityCheckpoints() bool {
	s.affinityMu.Lock()
	s.affinityStopping = true
	stop, done, drain := s.affinityStop, s.affinityDone, s.affinityDrainEnabled
	if stop != nil {
		close(stop)
		s.affinityStop = nil
	}
	s.affinityMu.Unlock()
	if done != nil {
		<-done
	}
	return drain
}
