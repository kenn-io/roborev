package main

import (
	"context"
	"crypto/rand"
	"errors"
	"io"

	"go.kenn.io/roborev/internal/daemon"
)

func interruptDaemonWithUpdateAPI(
	ctx context.Context, endpoint daemon.DaemonEndpoint,
) (retErr error) {
	session, err := prepareUpdateDaemon(ctx, endpoint, rand.Text(), policyInterrupt, io.Discard)
	if err != nil {
		return err
	}
	defer func() {
		if retErr == nil {
			return
		}
		_, releaseErr := session.release(context.WithoutCancel(ctx))
		retErr = errors.Join(retErr, releaseErr)
	}()

	if err := waitForPreparedDrain(ctx, session, io.Discard); err != nil {
		return err
	}
	return session.shutdown(ctx)
}
