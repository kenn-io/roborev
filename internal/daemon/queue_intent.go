package daemon

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
	"uuid"

	"github.com/gofrs/flock"
	"go.kenn.io/kit/atomicfile"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

const QueueIntentHeader = "X-Roborev-Queue-Intent"

var ErrQueueIntentSuperseded = errors.New("queue request superseded by a newer request")

type queueIntent struct {
	Token  string `json:"token"`
	Paused *bool  `json:"paused"`
}

func queueIntentPath() string { return filepath.Join(config.DataDir(), "queue-intent.json") }

func lockQueueIntent(ctx context.Context) (func(), error) {
	if err := os.MkdirAll(config.DataDir(), 0o700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	lock := flock.New(queueIntentPath() + ".lock")
	if ok, err := lock.TryLockContext(ctx, 10*time.Millisecond); err != nil || !ok {
		return nil, fmt.Errorf("lock queue request: %w", err)
	}
	return func() { _ = lock.Close() }, nil
}

func readQueueIntent() (queueIntent, error) {
	var request queueIntent
	data, err := os.ReadFile(queueIntentPath())
	if err == nil {
		err = json.Unmarshal(data, &request)
		if err == nil && (request.Token == "" || request.Paused == nil) {
			err = fmt.Errorf("incomplete queue request")
		}
	}
	return request, err
}

func writeQueueIntent(paused bool) (string, error) {
	request := queueIntent{Token: uuid.New().String(), Paused: &paused}
	data, err := json.Marshal(request)
	if err == nil {
		err = atomicfile.WriteFile(queueIntentPath(), data)
	}
	return request.Token, err
}

// PublishQueueIntent carries local CLI intent across recovery by another process.
func PublishQueueIntent(ctx context.Context, paused bool) (string, error) {
	release, err := lockQueueIntent(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	return writeQueueIntent(paused)
}

func updateQueueIntent(ctx context.Context, db *storage.DB, paused bool, token string) error {
	release, err := lockQueueIntent(ctx)
	if err != nil {
		return err
	}
	defer release()
	if token == "" {
		if _, err = writeQueueIntent(paused); err != nil {
			return err
		}
	} else {
		request, err := readQueueIntent()
		if errors.Is(err, os.ErrNotExist) {
			current, err := db.IsQueuePaused()
			if err != nil || current == paused {
				return err
			}
			return ErrQueueIntentSuperseded
		}
		if err != nil {
			return err
		}
		if request.Token != token || *request.Paused != paused {
			return ErrQueueIntentSuperseded
		}
	}
	if err := db.SetQueuePaused(paused); err != nil {
		return err
	}
	return os.Remove(queueIntentPath())
}

// consumeQueueIntent keeps publication serialized until the caller starts workers.
func consumeQueueIntent(ctx context.Context, db *storage.DB) (func(), error) {
	release, err := lockQueueIntent(ctx)
	if err != nil {
		return nil, err
	}
	request, err := readQueueIntent()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("Ignoring queue request: %v", err)
		}
		return release, nil
	}
	if err := db.SetQueuePaused(*request.Paused); err != nil {
		release()
		return nil, fmt.Errorf("persist queue request: %w", err)
	}
	if err := os.Remove(queueIntentPath()); err != nil {
		log.Printf("Remove applied queue request: %v", err)
	}
	return release, nil
}
