package main

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"uuid"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/kataresponse"
	"go.kenn.io/roborev/internal/storage"
	roborevclient "go.kenn.io/roborev/pkg/client"
)

func kataCommentCmd() *cobra.Command {
	var body, key, replacement string
	targets := map[string]*string{}
	cmd := &cobra.Command{Use: "comment --<kind> <finding|comment> -m <text>", Short: "Respond to a published finding through Kata", Args: cobra.NoArgs}
	for _, kind := range []string{"reply", "confirm", "refute", "supersede"} {
		var target string
		targets[kind] = &target
		cmd.Flags().StringVar(&target, kind, "", kind+" a finding selector or comment reference")
	}
	cmd.MarkFlagsMutuallyExclusive("reply", "confirm", "refute", "supersede")
	cmd.Flags().StringVarP(&body, "body", "m", "", "response evidence")
	cmd.Flags().StringVar(&key, "idempotency-key", "", "opaque key for an identical response retry")
	cmd.Flags().StringVar(&replacement, "replacement", "", "replacement finding selector for supersede")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		request := kataresponse.Request{OperationID: key, Body: body, Replacement: replacement}
		for kind, target := range targets {
			if cmd.Flags().Changed(kind) {
				request.Kind, request.FindingID = kind, *target
			}
		}
		if cmd.Flags().Changed("idempotency-key") && key == "" {
			return usageErr(cmd, fmt.Errorf("--idempotency-key must not be empty"))
		}
		if request.OperationID == "" {
			request.OperationID = uuid.New().String() //nolint:forbidigo // Generated public retry key before any daemon contact.
		}
		if err := request.Validate(); err != nil {
			return usageErr(cmd, err)
		}
		if _, err := json.Marshal(request); err != nil {
			return usageErr(cmd, fmt.Errorf("response contains invalid text encoding"))
		}
		cmd.Printf("Idempotency key: %s\n", request.OperationID)
		return runKataResponse(cmd, request)
	}
	return cmd
}

// runKataResponse uses only the generated daemon API; the daemon owns Kata's
// credential. A failed enqueue acknowledgement leaves the printed ID reusable.
func runKataResponse(cmd *cobra.Command, request kataresponse.Request) error {
	if err := ensureDaemon(); err != nil {
		return fmt.Errorf("daemon unavailable: %w", err)
	}
	api := getDaemonEndpoint().APIClient(0)
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}
	response, err := api.KataResponseRaw(cmd.Context(), nil, roborevclient.WithBody(raw))
	if err != nil {
		return fmt.Errorf("daemon contact failed; retry with the same idempotency key: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusAccepted && strings.Contains(response.Header.Get("Content-Type"), "application/problem+json") {
		var problem struct {
			Detail string `json:"detail"`
		}
		if json.UnmarshalRead(response.Body, &problem) == nil && problem.Detail != "" {
			return fmt.Errorf("response operation: %s", problem.Detail)
		}
	}
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusMethodNotAllowed {
		return fmt.Errorf("daemon does not support typed responses; update and restart it")
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusAccepted {
		var problem struct {
			Detail string `json:"detail"`
		}
		if json.UnmarshalRead(response.Body, &problem) == nil && problem.Detail != "" {
			return fmt.Errorf("response operation: %s", problem.Detail)
		}
		return fmt.Errorf("daemon rejected response operation (HTTP %d); retry only with the same payload and idempotency key", response.StatusCode)
	}
	var status storage.KataResponseStatus
	if err := json.UnmarshalRead(response.Body, &status); err != nil {
		return fmt.Errorf("read operation acknowledgement; retry with the same idempotency key: %w", err)
	}
	cmd.Printf("%s: %s (%s)\n", status.OperationID, status.State, status.Kind)
	if status.Message != "" {
		cmd.Println(status.Message)
	}
	if status.ExistingComment != "" {
		cmd.Printf("Existing comment: %s\n", status.ExistingComment)
	}
	return nil
}
