package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// The review store keeps every single review once, so panels and
// single-review arms can be assembled from saved reviews instead of run again.
// A review lives at
//
//	ROOT/HARNESS/MODEL/REASONING/PROMPT_VERSION/REVIEWER/PR/N/
//
// with review.json, review.md, and meta.json. N numbers repeat reviews of the
// same key. A stored review is reused only when its prompt hash matches the
// prompt the current code builds; the path keys are for people and grouping.

// storeKey is what produced a review.
type storeKey struct {
	Harness, Model, Reasoning, PromptVersion, Reviewer, PR string
}

func (k storeKey) dir(root string) string {
	return filepath.Join(root, k.Harness, k.Model, k.Reasoning, k.PromptVersion, k.Reviewer, k.PR)
}

// sampleMeta is meta.json: how one stored review was made.
type sampleMeta struct {
	Reviewer      string `json:"reviewer"`
	ReviewType    string `json:"review_type"`
	Instructions  string `json:"instructions"`
	Agent         string `json:"agent"`
	Provider      string `json:"provider,omitempty"`
	Model         string `json:"model"`
	Reasoning     string `json:"reasoning"`
	PromptVersion string `json:"prompt_version"`
	// PromptSHA256 hashes the complete prompt, with a prompt handed over as a
	// file read back in. Empty when it is unknown.
	PromptSHA256   string    `json:"prompt_sha256"`
	RoborevSHA     string    `json:"roborev_sha"`
	PR             string    `json:"pr"`
	SourceRun      string    `json:"source_run"`
	CreatedAt      time.Time `json:"created_at"`
	ElapsedSeconds float64   `json:"elapsed_seconds,omitempty"`
	ReviewSHA256   string    `json:"review_sha256"`
}

// reviewerName names a reviewer by what it adds to roborev's base prompt. A
// member with no instructions is the stock reviewer for its review type.
func reviewerName(member, reviewType, instructions string) string {
	if instructions != "" {
		return member
	}
	if reviewType == "" || reviewType == "default" {
		return "stock"
	}
	return reviewType
}

// currentDateLine is the date roborev puts in every prompt. It changes daily
// without changing what the reviewer is asked, so the hash masks it.
var currentDateLine = regexp.MustCompile(`(?m)^Current date: [0-9]{4}-[0-9]{2}-[0-9]{2} \(UTC\)$`)

// hashablePrompt is the prompt as hashed for reuse, with the current date
// masked. The instruction block is hashed separately, with a placeholder for
// the panel name.
func hashablePrompt(prompt string) string {
	return currentDateLine.ReplaceAllString(prompt, "Current date: * (UTC)")
}

func harnessName(agent, provider string) string {
	if provider == "" {
		return agent
	}
	return agent + "@" + provider
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// samples returns the key's sample directories in order, with their metadata.
func samples(root string, k storeKey) ([]string, []sampleMeta, error) {
	base := k.dir(root)
	entries, err := os.ReadDir(base)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var nums []int
	for _, e := range entries {
		if n, err := strconv.Atoi(e.Name()); err == nil && e.IsDir() {
			nums = append(nums, n)
		}
	}
	sort.Ints(nums)
	var dirs []string
	var metas []sampleMeta
	for _, n := range nums {
		dir := filepath.Join(base, strconv.Itoa(n))
		raw, err := os.ReadFile(filepath.Join(dir, "meta.json"))
		if errors.Is(err, fs.ErrNotExist) {
			continue // another writer has claimed it and not finished yet
		}
		if err != nil {
			return nil, nil, err
		}
		var m sampleMeta
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", dir, err)
		}
		dirs = append(dirs, dir)
		metas = append(metas, m)
	}
	return dirs, metas, nil
}

// findSample returns the first stored review of the key made from exactly
// this prompt, or "" when there is none.
func findSample(root string, k storeKey, promptHash string) (string, error) {
	if promptHash == "" {
		return "", nil
	}
	dirs, metas, err := samples(root, k)
	if err != nil {
		return "", err
	}
	for i, m := range metas {
		if m.PromptSHA256 == promptHash {
			return dirs[i], nil
		}
	}
	return "", nil
}

// putSample stores a review and returns its directory. Identical reviews are
// both kept: two reviews that find nothing can match byte for byte and are
// still two samples. A reviewer whose instructions differ from its stored
// reviews is refused, since its name would then stand for two reviewers.
func putSample(root string, k storeKey, meta sampleMeta, reviewJSON []byte, reviewMD string) (string, error) {
	meta.ReviewSHA256 = sha256Hex(reviewJSON)
	dirs, metas, err := samples(root, k)
	if err != nil {
		return "", err
	}
	for i, m := range metas {
		if m.Instructions != meta.Instructions {
			return "", fmt.Errorf("reviewer %s already has reviews with other instructions (%s); give the changed reviewer a new name", k.Reviewer, dirs[i])
		}
	}
	base := k.dir(root)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", err
	}
	// Mkdir claims a number atomically, so concurrent writers never share one.
	for n := 1; ; n++ {
		dir := filepath.Join(base, strconv.Itoa(n))
		err := os.Mkdir(dir, 0o755)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		encoded, err := json.MarshalIndent(meta, "", "  ")
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(dir, "review.json"), reviewJSON, 0o644); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(dir, "review.md"), []byte(reviewMD), 0o644); err != nil {
			return "", err
		}
		// meta.json last: readers treat a sample without it as unfinished.
		return dir, os.WriteFile(filepath.Join(dir, "meta.json"), append(encoded, '\n'), 0o644)
	}
}
