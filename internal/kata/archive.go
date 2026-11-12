package kata

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// ResolveArchiveIssue compares identities returned by Kata, rather than
// inferring a project from the spelling of a possibly moved reference.
func (c *CLIClient) ResolveArchiveIssue(ctx context.Context, project, ref string) (ArchiveIssue, error) {
	out, err := c.run(ctx, c, []string{"projects", "show", project, "--json"}, nil)
	if err != nil {
		return ArchiveIssue{}, fmt.Errorf("resolve project: %w", err)
	}
	var p struct {
		Project struct {
			ID int64 `json:"id"`
		} `json:"project"`
	}
	if err := json.Unmarshal(out, &p); err != nil || p.Project.ID <= 0 {
		return ArchiveIssue{}, fmt.Errorf("resolve project: missing project identity")
	}
	out, err = c.run(ctx, c, []string{"show", ref, "--json", "--project", project}, nil)
	if err != nil {
		return ArchiveIssue{}, fmt.Errorf("resolve issue: %w", err)
	}
	var result struct {
		Issue struct {
			ProjectID int64  `json:"project_id"`
			UID       string `json:"uid"`
			ShortID   string `json:"short_id"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(out, &result); err != nil || result.Issue.ProjectID <= 0 || result.Issue.UID == "" || result.Issue.ShortID == "" {
		return ArchiveIssue{}, fmt.Errorf("resolve issue: Kata did not return complete identity; update Kata and retry")
	}
	if result.Issue.ProjectID != p.Project.ID {
		return ArchiveIssue{}, fmt.Errorf("issue %s moved outside project %s; update the commit reference", ref, project)
	}
	return ArchiveIssue{UID: result.Issue.UID, ShortID: result.Issue.ShortID}, nil
}

// supportsAttachmentReuse caches only successful help probes. Concurrent first
// probes may run independently so one caller's context never blocks another.
func (c *CLIClient) supportsAttachmentReuse(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if state := c.archiveReuse.Load(); state != 0 {
		return state == 2, nil
	}
	out, err := c.run(ctx, c, []string{"docbank", "attachments", "add", "--help"}, nil)
	if err != nil {
		return false, fmt.Errorf("probe attachment reuse: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	state := uint32(1)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "--reuse-existing" {
			state = 2
			break
		}
	}
	c.archiveReuse.Store(state)
	return state == 2, nil
}

// ArchiveRubric uses atomic reuse when available, or serializes the legacy
// list/add pair within this process.
func (c *CLIClient) ArchiveRubric(ctx context.Context, project, uid, name string, data []byte) error {
	reuse, err := c.supportsAttachmentReuse(ctx)
	if err != nil {
		return err
	}
	if !reuse {
		key := project + "#" + uid + "/" + name
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
	}
	if !reuse {
		issue, err := c.ResolveArchiveIssue(ctx, project, uid)
		if err != nil {
			return err
		}
		if issue.UID != uid {
			return fmt.Errorf("resolve issue: identity changed; retry archive")
		}
		out, err := c.run(ctx, c, []string{"docbank", "attachments", "ls", issue.ShortID, "--json", "--project", project}, nil)
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
	issue, err := c.ResolveArchiveIssue(ctx, project, uid)
	if err != nil {
		return err
	}
	if issue.UID != uid {
		return fmt.Errorf("resolve issue: identity changed; retry archive")
	}
	args := []string{"docbank", "attachments", "add", issue.ShortID, file, "--json", "--project", project}
	if reuse {
		args = append(args, "--reuse-existing")
	}
	out, err := c.run(ctx, c, args, nil)
	if err != nil {
		return fmt.Errorf("add attachment: %w", err)
	}
	var added struct {
		Results []struct {
			ID string `json:"attachment_id"`
		} `json:"results"`
	}
	if err := json.Unmarshal(out, &added); err != nil {
		return fmt.Errorf("parse add attachment: %w", err)
	}
	if len(added.Results) != 1 || added.Results[0].ID == "" {
		return fmt.Errorf("add attachment failed: %s", string(out))
	}
	return nil
}
