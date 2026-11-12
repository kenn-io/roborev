package kata

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type archiveLock struct {
	token chan struct{}
	users int
}

var archiveLocks = struct {
	sync.Mutex
	locks map[string]*archiveLock
}{locks: make(map[string]*archiveLock)}

// ArchiveRubric serializes the list/add pair for an issue and exact file name.
func (c *CLIClient) ArchiveRubric(ctx context.Context, project, ref, name string, data []byte) error {
	key := project + "#" + ref + "/" + name
	archiveLocks.Lock()
	lock := archiveLocks.locks[key]
	if lock == nil {
		lock = &archiveLock{token: make(chan struct{}, 1)}
		lock.token <- struct{}{}
		archiveLocks.locks[key] = lock
	}
	lock.users++
	archiveLocks.Unlock()
	defer func() {
		archiveLocks.Lock()
		lock.users--
		if lock.users == 0 {
			delete(archiveLocks.locks, key)
		}
		archiveLocks.Unlock()
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-lock.token:
	}
	defer func() { lock.token <- struct{}{} }()
	out, err := c.run(ctx, c, []string{"docbank", "attachments", "ls", ref, "--json", "--project", project}, nil)
	if err != nil {
		return fmt.Errorf("list attachments: %w", err)
	}
	var listed struct {
		Attachments map[string]struct {
			Name string `json:"name"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(out, &listed); err != nil {
		return fmt.Errorf("parse attachments: %w", err)
	}
	if listed.Attachments == nil {
		return fmt.Errorf("parse attachments: missing attachments")
	}
	for _, item := range listed.Attachments {
		if item.Name == name {
			return nil
		}
	}
	dir, err := os.MkdirTemp("", "roborev-rubric-*")
	if err != nil {
		return fmt.Errorf("create rubric temporary directory: %w", err)
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, name)
	if err := os.WriteFile(file, data, 0o600); err != nil {
		return fmt.Errorf("write rubric: %w", err)
	}
	out, err = c.run(ctx, c, []string{"docbank", "attachments", "add", ref, file, "--json", "--project", project}, nil)
	if err != nil {
		return fmt.Errorf("add attachment: %w", err)
	}
	var added struct {
		Results []struct {
			ID    string `json:"attachment_id"`
			Error string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(out, &added); err != nil {
		return fmt.Errorf("parse add attachment: %w", err)
	}
	if len(added.Results) != 1 || added.Results[0].Error != "" || added.Results[0].ID == "" {
		return fmt.Errorf("add attachment failed: %s", string(out))
	}
	return nil
}
