package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	xansi "github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"

	roborevclient "go.kenn.io/roborev/pkg/client"
	"go.kenn.io/roborev/pkg/client/generated"
)

const (
	searchDefaultLimit  = 20
	searchMaxLimit      = 100
	searchMaxQueryRunes = 2000
)

type searchOpts struct {
	repo        string
	branch      string
	since       string
	verdict     string
	open        bool
	closed      bool
	lexical     bool
	hybrid      bool
	semantic    bool
	limit       int
	jsonOutput  bool
	agentOutput bool
}

func searchCmd() *cobra.Command {
	var opts searchOpts
	cmd := &cobra.Command{
		Use:   "search <query>...",
		Short: "Search completed review history",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return searchUsageError(cmd, fmt.Errorf("search requires a query"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			query := strings.TrimSpace(strings.Join(args, " "))
			if err := validateSearchOpts(query, opts); err != nil {
				return searchUsageError(cmd, err)
			}
			return runSearch(cmd, query, opts)
		},
	}
	cmd.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return searchUsageError(cmd, err)
	})

	cmd.Flags().StringVar(&opts.repo, "repo", "", "scope to a repository path, name, or identity")
	cmd.Flags().StringVar(&opts.branch, "branch", "", "scope to an exact branch")
	cmd.Flags().StringVar(&opts.since, "since", "", "lower bound as a Go duration or RFC3339 timestamp")
	cmd.Flags().StringVar(&opts.verdict, "verdict", "", "filter by verdict (pass or fail)")
	cmd.Flags().BoolVar(&opts.open, "open", false, "search open reviews only")
	cmd.Flags().BoolVar(&opts.closed, "closed", false, "search closed reviews only")
	cmd.Flags().BoolVar(&opts.lexical, "lexical", false, "use exact keyword search")
	cmd.Flags().BoolVar(&opts.hybrid, "hybrid", false, "combine lexical and semantic search")
	cmd.Flags().BoolVar(&opts.semantic, "semantic", false, "use semantic search")
	cmd.Flags().IntVar(&opts.limit, "limit", searchDefaultLimit, "maximum results (1..100)")
	cmd.Flags().BoolVar(&opts.jsonOutput, "json", false, "emit the daemon response as JSON")
	cmd.Flags().BoolVar(&opts.agentOutput, "agent", false, "emit concise agent-readable text")
	return cmd
}

func searchUsageError(cmd *cobra.Command, err error) error {
	cmd.SilenceUsage = false
	return &exitError{code: 2, cause: err}
}

func validateSearchOpts(query string, opts searchOpts) error {
	if opts.agentOutput && opts.jsonOutput {
		return errors.New("--agent and --json are mutually exclusive")
	}
	if query == "" {
		return errors.New("query must not be blank")
	}
	if utf8.RuneCountInString(query) > searchMaxQueryRunes {
		return fmt.Errorf("query must be at most %d UTF-8 runes", searchMaxQueryRunes)
	}
	if opts.verdict != "" && opts.verdict != "pass" && opts.verdict != "fail" {
		return errors.New("--verdict must be pass or fail")
	}
	if err := validateSearchSince(opts.since); err != nil {
		return err
	}
	if opts.limit < 1 || opts.limit > searchMaxLimit {
		return fmt.Errorf("--limit must be between 1 and %d", searchMaxLimit)
	}
	if boolCount(opts.open, opts.closed) > 1 {
		return errors.New("--open and --closed are mutually exclusive")
	}
	if boolCount(opts.lexical, opts.hybrid, opts.semantic) > 1 {
		return errors.New("--lexical, --hybrid, and --semantic are mutually exclusive")
	}
	return nil
}

func validateSearchSince(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if duration, err := time.ParseDuration(raw); err == nil && duration > 0 {
		return nil
	}
	if _, err := time.Parse(time.RFC3339, raw); err == nil {
		return nil
	}
	return errors.New("--since must be a positive Go duration or RFC3339 timestamp")
}

func boolCount(values ...bool) int {
	count := 0
	for _, value := range values {
		if value {
			count++
		}
	}
	return count
}

func runSearch(cmd *cobra.Command, query string, opts searchOpts) error {
	if err := ensureDaemon(); err != nil {
		return fmt.Errorf("daemon not running: %w", err)
	}

	endpoint := getDaemonEndpoint()
	api, err := roborevclient.NewWithHTTPClient(
		endpoint.BaseURL(), endpoint.HTTPClient(30*time.Second),
	)
	if err != nil {
		return fmt.Errorf("create search client: %w", err)
	}
	params := searchRequestQuery(query, opts)
	response, err := api.SearchReviewsWithResponse(cmd.Context(), &generated.SearchReviewsRequestOptions{
		Query: &params,
	})
	if err != nil {
		if response != nil {
			return searchResponseError(response)
		}
		return errors.New("search reviews: request failed")
	}
	if response == nil || response.JSON200 == nil || len(response.Body) == 0 {
		return errors.New("search reviews: daemon returned an empty response")
	}

	if opts.jsonOutput {
		_, err := cmd.OutOrStdout().Write(response.Body)
		return err
	}
	if opts.agentOutput {
		printSearchAgentResults(cmd, *response.JSON200)
		return nil
	}
	printSearchResults(cmd, *response.JSON200)
	return nil
}

func searchResponseError(response *generated.SearchReviewsResp) error {
	status := response.StatusCode
	if status == 0 && response.HTTPResponse != nil {
		status = response.HTTPResponse.StatusCode
	}
	statusText := http.StatusText(status)
	if statusText == "" {
		statusText = "HTTP error"
	}
	message := fmt.Sprintf("search reviews: %s (status %d)", statusText, status)

	var problem generated.SearchReviewsErrorResponse
	if len(response.Body) == 0 || json.Unmarshal(response.Body, &problem) != nil {
		return errors.New(message)
	}
	parts := make([]string, 0, 2)
	if problem.Title != nil {
		if title := sanitizeSearchSingleLine(*problem.Title); title != "" {
			parts = append(parts, title)
		}
	}
	if problem.Detail != nil {
		if detail := sanitizeSearchSingleLine(*problem.Detail); detail != "" {
			parts = append(parts, detail)
		}
	}
	if len(parts) > 0 {
		message += ": " + strings.Join(parts, ": ")
	}
	return errors.New(message)
}

func searchRequestQuery(query string, opts searchOpts) generated.SearchReviewsQuery {
	mode := generated.Auto
	switch {
	case opts.lexical:
		mode = generated.Lexical
	case opts.hybrid:
		mode = generated.Hybrid
	case opts.semantic:
		mode = generated.Semantic
	}
	state := generated.All
	if opts.open {
		state = generated.Open
	} else if opts.closed {
		state = generated.Closed
	}

	params := generated.SearchReviewsQuery{
		Q: query, Mode: &mode, State: &state, Limit: &opts.limit,
	}
	if opts.repo != "" {
		params.Repo = &opts.repo
	}
	if opts.branch != "" {
		params.Branch = &opts.branch
	}
	if opts.since != "" {
		params.Since = &opts.since
	}
	if opts.verdict != "" {
		verdict := generated.SearchReviewsQueryVerdict(opts.verdict)
		params.Verdict = &verdict
	}
	return params
}

func printSearchAgentResults(cmd *cobra.Command, response generated.SearchResponse) {
	cmd.Printf("OK search count=%d query=%s mode=%s partial=%t bounded=%t",
		len(response.Hits), searchAgentValue(response.Query), searchAgentValue(response.Mode), response.Partial, response.Bounded)
	if response.Degraded {
		cmd.Printf(" degraded=%s", searchAgentValue(pointerText(response.DegradedReason, "reason unavailable")))
	}
	if response.Bounded {
		cmd.Printf(" bounded_reason=%s", searchAgentValue(pointerText(response.BoundedReason, "candidate limit reached")))
	}
	cmd.Println()
	for _, hit := range response.Hits {
		state := "open"
		if hit.Closed {
			state = "closed"
		}
		cmd.Printf("- job=%d review=%d score=%g state=%s matched=%s repo=%s ref=%s verdict=%s excerpt=%s\n",
			hit.JobID, hit.ReviewID, hit.Score, state, searchAgentValue(strings.Join(hit.MatchedIn, ",")),
			searchAgentValue(hit.RepoName), searchAgentValue(hit.GitRef),
			searchAgentValue(pointerText(hit.Verdict, "unknown")), searchAgentValue(hit.Excerpt))
	}
}

func searchAgentValue(value string) string {
	value = sanitizeSearchExcerpt(value)
	if value == "" || strings.IndexFunc(value, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || r == '"' || r == '\\'
	}) >= 0 {
		return strconv.Quote(value)
	}
	return value
}

func printSearchResults(cmd *cobra.Command, response generated.SearchResponse) {
	cmd.Printf("Mode: %s\n", sanitizeSearchSingleLine(response.Mode))
	if response.Degraded {
		cmd.Printf("Degraded: %s\n", sanitizeSearchSingleLine(pointerText(response.DegradedReason, "reason unavailable")))
	}
	if response.Bounded {
		cmd.Printf("Bounded: %s\n", sanitizeSearchSingleLine(pointerText(response.BoundedReason, "candidate limit reached")))
	}
	if response.Partial {
		cmd.Println("Partial: search coverage is incomplete")
	}
	cmd.Printf("Coverage: %s\n", formatSearchCoverage(response.Coverage))

	if len(response.Hits) == 0 {
		cmd.Println("No matching reviews.")
		return
	}
	cmd.Println()
	for index, hit := range response.Hits {
		if index > 0 {
			cmd.Println()
		}
		state := "open"
		if hit.Closed {
			state = "closed"
		}
		verdict := sanitizeSearchSingleLine(pointerText(hit.Verdict, "unknown"))
		matchedIn := make([]string, 0, len(hit.MatchedIn))
		for _, leg := range hit.MatchedIn {
			matchedIn = append(matchedIn, sanitizeSearchSingleLine(leg))
		}
		matched := strings.Join(matchedIn, ", ")
		if matched == "" {
			matched = "none"
		}
		cmd.Printf("Job %d / Review %d | %s | %s | %s/%s\n",
			hit.JobID, hit.ReviewID, sanitizeSearchSingleLine(hit.RepoName),
			sanitizeSearchSingleLine(hit.GitRef), verdict, state)
		cmd.Printf("Score: %g | Matched: %s\n", hit.Score, matched)
		cmd.Println(sanitizeSearchExcerpt(hit.Excerpt))
	}
}

func formatSearchCoverage(coverage generated.SearchCoverage) string {
	mirror := "mirror complete"
	if !coverage.MirrorComplete {
		if coverage.MirrorBacklog == nil {
			mirror = "mirror incomplete (backlog unknown)"
		} else {
			mirror = fmt.Sprintf("mirror incomplete (%d pending)", *coverage.MirrorBacklog)
		}
	}

	vectors := "vectors disabled"
	if coverage.EmbeddingsConfigured || coverage.VectorState != "disabled" {
		details := make([]string, 0, 2)
		if coverage.EmbeddingBacklog > 0 {
			details = append(details, fmt.Sprintf("%d pending", coverage.EmbeddingBacklog))
		}
		if coverage.Skipped > 0 {
			details = append(details, fmt.Sprintf("%d skipped", coverage.Skipped))
		}
		vectors = "vectors " + sanitizeSearchSingleLine(coverage.VectorState)
		if len(details) > 0 {
			vectors += " (" + strings.Join(details, ", ") + ")"
		}
	}
	return mirror + "; " + vectors
}

func sanitizeSearchSingleLine(value string) string {
	return sanitizeSearchText(value, false)
}

func sanitizeSearchExcerpt(value string) string {
	return sanitizeSearchText(value, true)
}

func sanitizeSearchText(value string, preserveLayout bool) string {
	value = xansi.Strip(value)
	return strings.Map(func(r rune) rune {
		if !unicode.IsControl(r) {
			return r
		}
		if preserveLayout && (r == '\n' || r == '\t') {
			return r
		}
		return -1
	}, value)
}

func pointerText(value *string, fallback string) string {
	if value == nil || *value == "" {
		return fallback
	}
	return *value
}
