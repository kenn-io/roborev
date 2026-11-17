package daemon

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/config"
)

type PostCommitScheduleRequest struct {
	RepoPath string `json:"repo_path" minLength:"1"`
	Branch   string `json:"branch" minLength:"1"`
	Head     string `json:"head" minLength:"1"`
}

type PostCommitScheduleInput struct {
	Body PostCommitScheduleRequest
}
type PostCommitScheduleOutput struct{}

type postCommitTimerKey struct {
	repository, branch string
}
type postCommitTimer struct {
	cancel context.CancelFunc
}

// Timers are daemon-owned. Git's existing batch checkpoints remain authoritative:
// a canceled or lost timer leaves the range pending for the next commit or push.
// A flush that returns an error did not queue its range, so its timer re-arms
// with twice the previous wait until a flush succeeds or a newer commit on the
// branch replaces the timer.
type postCommitScheduler struct {
	mu      sync.Mutex
	timers  map[postCommitTimerKey]*postCommitTimer
	stopped bool
	wg      sync.WaitGroup
	flush   func(context.Context, string, string, string) error
}

func newPostCommitScheduler(flush func(context.Context, string, string, string) error) *postCommitScheduler {
	return &postCommitScheduler{timers: make(map[postCommitTimerKey]*postCommitTimer), flush: flush}
}

func (s *postCommitScheduler) schedule(repository, root, branch, head string, quietPeriod time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return fmt.Errorf("post-commit scheduler is stopping")
	}
	key := postCommitTimerKey{repository, branch}
	if prior := s.timers[key]; prior != nil {
		prior.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	entry := &postCommitTimer{cancel: cancel}
	s.timers[key] = entry
	// Construct the timer before returning so its deadline starts at scheduling.
	timer := time.NewTimer(quietPeriod)
	s.wg.Go(func() {
		defer cancel()
		defer timer.Stop()
		defer func() {
			s.mu.Lock()
			if s.timers[key] == entry {
				delete(s.timers, key)
			}
			s.mu.Unlock()
		}()
		wait := quietPeriod
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			if ctx.Err() != nil {
				return
			}
			if err := s.flush(ctx, root, branch, head); err == nil || ctx.Err() != nil {
				return
			}
			wait *= 2
			log.Printf("Post-commit quiet flush for %s queued no review; retrying in %s", branch, wait)
			timer.Reset(wait)
		}
	})
	return nil
}

func (s *postCommitScheduler) stop() {
	s.mu.Lock()
	s.stopped = true
	for _, timer := range s.timers {
		timer.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Server) humaPostCommitSchedule(ctx context.Context, input *PostCommitScheduleInput) (*PostCommitScheduleOutput, error) {
	req := input.Body
	root, err := gitrepo.Root(ctx, req.RepoPath)
	if err != nil {
		return nil, huma.Error400BadRequest("resolve post-commit checkout", err)
	}
	repository, err := gitrepo.MainRoot(ctx, root)
	if err != nil {
		return nil, huma.Error400BadRequest("resolve post-commit repository", err)
	}
	period, err := config.ResolvePostCommitQuietPeriod(root)
	if err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}
	if period == 0 {
		return &PostCommitScheduleOutput{}, nil
	}
	if err := s.postCommitScheduler.schedule(repository, root, req.Branch, req.Head, period); err != nil {
		return nil, err
	}
	return &PostCommitScheduleOutput{}, nil
}

// flushPostCommitBatch runs the hook's flush for a quiet-period timer. The
// flush exits with an error when the daemon did not queue its range.
func (s *Server) flushPostCommitBatch(ctx context.Context, root, branch, head string) error {
	binary, err := os.Executable()
	if err != nil {
		log.Printf("Post-commit quiet flush: %v", err)
		return err
	}
	s.endpointMu.Lock()
	endpoint := s.endpoint
	s.endpointMu.Unlock()
	cmd := HiddenCommand(ctx, binary, "post-commit", "--repo", root,
		"--flush", "--flush-branch", branch, "--quiet-head", head, "--server", endpoint.ConfigAddr())
	output, err := cmd.CombinedOutput()
	if err != nil || len(output) > 0 {
		log.Printf("Post-commit quiet flush for %s: %v %s", branch, err, output)
	}
	return err
}
