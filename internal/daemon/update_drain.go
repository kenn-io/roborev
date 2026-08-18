package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/roborev/internal/storage"
)

const (
	updateLeaseDuration   = 60 * time.Second
	updatePolicyWait      = "wait"
	updatePolicyInterrupt = "interrupt"
	updatePolicyAbort     = "abort"
)

var updateRecoveryRetryInterval = 25 * time.Millisecond

var (
	errReviewsRunning           = errors.New("reviews are running")
	errShutdownInProgress       = errors.New("daemon shutdown in progress")
	errUpdatePolicyConflict     = errors.New("update lease policy conflict")
	errUpdateRecoveryInProgress = errors.New("update interruption recovery in progress")
	errLeaseTokenMismatch       = errors.New("update lease token mismatch")
	errLeaseExpired             = errors.New("update lease expired")
)

type updateLeaseConflict struct {
	Remaining time.Duration
}

func (e *updateLeaseConflict) Error() string {
	return fmt.Sprintf("another update owns the drain for %s", e.Remaining)
}

type updateDrainLease struct {
	ownerID    string
	token      string
	policy     string
	expiresAt  time.Time
	targeted   []int64
	recovering bool
	timer      *time.Timer
}

type updateDrainCoordinator struct {
	server *Server
	now    func() time.Time
}

func (c *updateDrainCoordinator) prepare(ownerID, policy string) (UpdateDrainStatus, error) {
	ownerID = strings.TrimSpace(ownerID)
	policy = strings.TrimSpace(policy)
	if ownerID == "" {
		return UpdateDrainStatus{}, errors.New("owner ID is required")
	}
	if policy != updatePolicyWait && policy != updatePolicyInterrupt && policy != updatePolicyAbort {
		return UpdateDrainStatus{}, fmt.Errorf("invalid update policy %q", policy)
	}

	s := c.server
	s.shutdownDrainMu.Lock()
	defer s.shutdownDrainMu.Unlock()
	if s.shutdownDraining {
		return UpdateDrainStatus{}, errShutdownInProgress
	}
	now := c.now()
	if lease := s.updateDrain; lease != nil {
		if !lease.expiresAt.After(now) {
			if err := c.releaseExpiredLocked(lease); err != nil || s.updateDrain != nil {
				return UpdateDrainStatus{}, errUpdateRecoveryInProgress
			}
		} else {
			if lease.recovering {
				return UpdateDrainStatus{}, errUpdateRecoveryInProgress
			}
			if lease.ownerID == ownerID && lease.policy == policy {
				return c.snapshotLocked(lease)
			}
			if lease.ownerID == ownerID {
				return UpdateDrainStatus{}, errUpdatePolicyConflict
			}
			return UpdateDrainStatus{}, &updateLeaseConflict{Remaining: lease.expiresAt.Sub(now)}
		}
	}

	if policy == updatePolicyInterrupt {
		s.workerPool.attemptTransitionsMu.Lock()
		defer s.workerPool.attemptTransitionsMu.Unlock()
	}
	if err := s.db.SetShutdownDraining(true); err != nil {
		return UpdateDrainStatus{}, fmt.Errorf("block job claims for update: %w", err)
	}
	ids, err := s.db.ListRunningJobIDs()
	if err != nil {
		return UpdateDrainStatus{}, c.rollbackPreparationLocked(ownerID, policy, err)
	}
	if policy == updatePolicyAbort && len(ids) != 0 {
		return UpdateDrainStatus{}, c.rollbackPreparationLocked(ownerID, policy, errReviewsRunning)
	}

	lease := &updateDrainLease{
		ownerID:   ownerID,
		token:     storage.GenerateUUID(),
		policy:    policy,
		expiresAt: now.Add(updateLeaseDuration),
		targeted:  append([]int64(nil), ids...),
	}
	s.updateDrain = lease
	if policy == updatePolicyInterrupt {
		s.workerPool.interruptJobsForUpdateLocked(ids)
	}
	c.armExpiryLocked(lease, lease.expiresAt.Sub(c.now()))
	return c.snapshotLocked(lease)
}

func (c *updateDrainCoordinator) renew(token string) (UpdateDrainStatus, error) {
	s := c.server
	s.shutdownDrainMu.Lock()
	defer s.shutdownDrainMu.Unlock()
	if s.shutdownDraining {
		return UpdateDrainStatus{}, errShutdownInProgress
	}
	lease := s.updateDrain
	if lease == nil || lease.token != token {
		return UpdateDrainStatus{}, errLeaseTokenMismatch
	}
	if lease.recovering {
		return UpdateDrainStatus{}, errUpdateRecoveryInProgress
	}
	if !lease.expiresAt.After(c.now()) {
		if err := c.releaseExpiredLocked(lease); err != nil || s.updateDrain != nil {
			return UpdateDrainStatus{}, errUpdateRecoveryInProgress
		}
		return UpdateDrainStatus{}, errLeaseExpired
	}
	lease.expiresAt = c.now().Add(updateLeaseDuration)
	c.armExpiryLocked(lease, updateLeaseDuration)
	return c.snapshotLocked(lease)
}

func (c *updateDrainCoordinator) release(token string) (bool, error) {
	s := c.server
	s.shutdownDrainMu.Lock()
	defer s.shutdownDrainMu.Unlock()
	if s.shutdownDraining {
		return false, errShutdownInProgress
	}
	lease := s.updateDrain
	if lease == nil || lease.token != token {
		return false, errLeaseTokenMismatch
	}
	if lease.recovering {
		return false, errUpdateRecoveryInProgress
	}
	lease.expiresAt = c.now()
	if err := c.releaseExpiredLocked(lease); err != nil {
		return false, err
	}
	return s.updateDrain == nil, nil
}

func (c *updateDrainCoordinator) snapshotLocked(lease *updateDrainLease) (UpdateDrainStatus, error) {
	runningIDs, err := c.server.db.ListRunningJobIDs()
	if err != nil {
		return UpdateDrainStatus{}, err
	}
	targetedRunning, err := c.server.db.CountRunningJobsByID(lease.targeted)
	if err != nil {
		return UpdateDrainStatus{}, err
	}
	return UpdateDrainStatus{
		LeaseToken:          lease.token,
		Policy:              lease.policy,
		ExpiresAt:           lease.expiresAt,
		RunningJobs:         len(runningIDs),
		TargetedRunningJobs: targetedRunning,
		ActiveWorkers:       c.server.workerPool.ActiveWorkers(),
		Recovering:          lease.recovering,
	}, nil
}

func (c *updateDrainCoordinator) releaseExpiredLocked(lease *updateDrainLease) error {
	s := c.server
	if s.updateDrain != lease {
		return nil
	}
	if lease.policy == updatePolicyInterrupt && s.workerPool.ActiveWorkers() == 0 {
		if err := s.workerPool.RetryFailedUpdateRequeues(); err != nil {
			lease.recovering = true
			c.armRecoveryRetryLocked(lease)
			return err
		}
	}
	remaining, err := s.db.CountRunningJobsByID(lease.targeted)
	if err != nil {
		lease.recovering = true
		c.armRecoveryRetryLocked(lease)
		return err
	}
	if lease.policy == updatePolicyInterrupt &&
		(remaining != 0 || s.workerPool.ActiveWorkers() != 0) {
		lease.recovering = true
		c.armRecoveryRetryLocked(lease)
		return nil
	}
	// Remove attempt markers while the persisted gate is still closed. Opening
	// the gate first would let a worker reclaim a requeued target and observe a
	// stale interrupt marker in the narrow interval between these operations.
	s.workerPool.ClearUpdateInterruptTargets()
	if err := s.db.SetShutdownDraining(false); err != nil {
		lease.recovering = true
		c.armRecoveryRetryLocked(lease)
		return err
	}
	if lease.timer != nil {
		lease.timer.Stop()
	}
	s.updateDrain = nil
	return nil
}

func (c *updateDrainCoordinator) rollbackPreparationLocked(
	ownerID, policy string, cause error,
) error {
	if err := c.server.db.SetShutdownDraining(false); err == nil {
		return cause
	} else {
		lease := &updateDrainLease{
			ownerID:    ownerID,
			token:      storage.GenerateUUID(),
			policy:     policy,
			expiresAt:  c.now(),
			recovering: true,
		}
		c.server.updateDrain = lease
		c.armRecoveryRetryLocked(lease)
		return errors.Join(cause, fmt.Errorf("release update claim gate: %w", err))
	}
}

func (c *updateDrainCoordinator) armExpiryLocked(lease *updateDrainLease, delay time.Duration) {
	if lease.timer != nil {
		lease.timer.Stop()
	}
	if delay < 0 {
		delay = 0
	}
	lease.timer = time.AfterFunc(delay, func() {
		s := c.server
		s.shutdownDrainMu.Lock()
		defer s.shutdownDrainMu.Unlock()
		if s.shutdownDraining || s.updateDrain != lease {
			return
		}
		if err := c.releaseExpiredLocked(lease); err != nil {
			log.Printf("Release expired update drain failed; retrying: %v", err)
		}
	})
}

func (c *updateDrainCoordinator) armRecoveryRetryLocked(lease *updateDrainLease) {
	c.armExpiryLocked(lease, updateRecoveryRetryInterval)
}

func (s *Server) updateDrainStatus() (bool, string, time.Time) {
	s.shutdownDrainMu.Lock()
	defer s.shutdownDrainMu.Unlock()
	if s.updateDrain == nil {
		return false, "", time.Time{}
	}
	return true, s.updateDrain.policy, s.updateDrain.expiresAt
}

func (s *Server) humaPrepareUpdate(
	ctx context.Context, input *PrepareUpdateInput,
) (*PrepareUpdateOutput, error) {
	status, err := s.updateCoordinator.prepare(input.Body.OwnerID, input.Body.Policy)
	if err != nil {
		var leaseConflict *updateLeaseConflict
		switch {
		case errors.Is(err, errReviewsRunning):
			return nil, huma.Error409Conflict("reviews are running")
		case errors.Is(err, errUpdatePolicyConflict):
			return nil, huma.Error409Conflict("update owner already holds a lease with a different policy")
		case errors.Is(err, errUpdateRecoveryInProgress):
			return nil, huma.Error409Conflict("previous update interruption is still recovering")
		case errors.Is(err, errShutdownInProgress):
			return nil, huma.Error409Conflict("daemon shutdown in progress")
		case errors.As(err, &leaseConflict):
			remaining := max(time.Duration(0), leaseConflict.Remaining).Round(time.Second)
			return nil, huma.Error409Conflict(
				fmt.Sprintf("another update is in progress; lease expires in %s", remaining),
			)
		default:
			return nil, huma.Error500InternalServerError(fmt.Sprintf("prepare update: %v", err))
		}
	}
	return &PrepareUpdateOutput{Body: status}, nil
}

func (s *Server) humaRenewUpdate(
	ctx context.Context, input *RenewUpdateInput,
) (*RenewUpdateOutput, error) {
	status, err := s.updateCoordinator.renew(input.Body.LeaseToken)
	if err != nil {
		return nil, updateLeaseHTTPError("renew", err)
	}
	return &RenewUpdateOutput{Body: status}, nil
}

func (s *Server) humaReleaseUpdate(
	ctx context.Context, input *ReleaseUpdateInput,
) (*ReleaseUpdateOutput, error) {
	released, err := s.updateCoordinator.release(input.Body.LeaseToken)
	if err != nil {
		return nil, updateLeaseHTTPError("release", err)
	}
	resp := &ReleaseUpdateOutput{}
	resp.Body.Released = released
	return resp, nil
}

func updateLeaseHTTPError(op string, err error) error {
	switch {
	case errors.Is(err, errLeaseTokenMismatch), errors.Is(err, errLeaseExpired):
		return huma.Error409Conflict("update lease is not owned by this updater")
	case errors.Is(err, errUpdateRecoveryInProgress):
		return huma.Error409Conflict("update interruption is still recovering")
	case errors.Is(err, errShutdownInProgress):
		return huma.Error409Conflict("daemon shutdown owns the claim drain")
	default:
		return huma.Error500InternalServerError(fmt.Sprintf("%s update lease: %v", op, err))
	}
}
