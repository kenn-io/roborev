package streamfmt

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

type codexTrackerExpectation struct {
	name       string
	eventType  string
	item       codexItem
	wantCmd    string
	wantRender bool
}

func TestCodexCommandTracker(t *testing.T) {
	tests := []struct {
		name         string
		observations []codexTrackerExpectation
	}{
		{
			name: "StartedThenCompletedSameIDSuppressesCompletion",
			observations: []codexTrackerExpectation{
				{
					name:       "started",
					eventType:  "item.started",
					item:       codexItem{ID: "cmd_1", Command: "bash -lc ls"},
					wantCmd:    "bash -lc ls",
					wantRender: true,
				},
				{
					name:       "completed",
					eventType:  "item.completed",
					item:       codexItem{ID: "cmd_1", Command: "bash -lc ls"},
					wantRender: false,
				},
			},
		},
		{
			name: "CompletedWithoutStartedRenders",
			observations: []codexTrackerExpectation{
				{
					name:       "completed",
					eventType:  "item.completed",
					item:       codexItem{ID: "cmd_2", Command: "bash -lc pwd"},
					wantCmd:    "bash -lc pwd",
					wantRender: true,
				},
			},
		},
		{
			name: "MixedIDStartedWithoutIDCompletedWithIDSuppressesEcho",
			observations: []codexTrackerExpectation{
				{
					name:       "started without id",
					eventType:  "item.started",
					item:       codexItem{Command: "bash -lc ls"},
					wantCmd:    "bash -lc ls",
					wantRender: true,
				},
				{
					name:       "completed with id",
					eventType:  "item.completed",
					item:       codexItem{ID: "cmd_1", Command: "bash -lc ls"},
					wantRender: false,
				},
			},
		},
		{
			name: "MixedIDStartedWithIDCompletedWithoutIDSuppressesEcho",
			observations: []codexTrackerExpectation{
				{
					name:       "started with id",
					eventType:  "item.started",
					item:       codexItem{ID: "cmd_1", Command: "bash -lc ls"},
					wantCmd:    "bash -lc ls",
					wantRender: true,
				},
				{
					name:       "completed without id",
					eventType:  "item.completed",
					item:       codexItem{Command: "bash -lc ls"},
					wantRender: false,
				},
			},
		},
		{
			name: "CompletionWithoutCommandClearsStartedState",
			observations: []codexTrackerExpectation{
				{
					name:       "started",
					eventType:  "item.started",
					item:       codexItem{ID: "cmd_1", Command: "bash -lc ls"},
					wantCmd:    "bash -lc ls",
					wantRender: true,
				},
				{
					name:       "completed no command",
					eventType:  "item.completed",
					item:       codexItem{ID: "cmd_1"},
					wantRender: false,
				},
				{
					name:       "later completed",
					eventType:  "item.completed",
					item:       codexItem{ID: "cmd_2", Command: "bash -lc ls"},
					wantCmd:    "bash -lc ls",
					wantRender: true,
				},
			},
		},
		{
			name: "CommandFallbackDoesNotLeaveStaleIDState",
			observations: []codexTrackerExpectation{
				{
					name:       "started cmd 1",
					eventType:  "item.started",
					item:       codexItem{ID: "cmd_1", Command: "bash -lc ls"},
					wantCmd:    "bash -lc ls",
					wantRender: true,
				},
				{
					name:       "command-only completion",
					eventType:  "item.completed",
					item:       codexItem{Command: "bash -lc ls"},
					wantRender: false,
				},
				{
					name:       "started cmd 2",
					eventType:  "item.started",
					item:       codexItem{ID: "cmd_2", Command: "bash -lc ls"},
					wantCmd:    "bash -lc ls",
					wantRender: true,
				},
				{
					name:       "late completion for cmd 1",
					eventType:  "item.completed",
					item:       codexItem{ID: "cmd_1"},
					wantRender: false,
				},
				{
					name:       "completion for cmd 2",
					eventType:  "item.completed",
					item:       codexItem{Command: "bash -lc ls"},
					wantRender: false,
				},
			},
		},
		{
			name: "MultipleIDsPairInFIFOOrder",
			observations: []codexTrackerExpectation{
				{
					name:       "started A",
					eventType:  "item.started",
					item:       codexItem{ID: "cmd_A", Command: "bash -lc ls"},
					wantCmd:    "bash -lc ls",
					wantRender: true,
				},
				{
					name:       "started B",
					eventType:  "item.started",
					item:       codexItem{ID: "cmd_B", Command: "bash -lc ls"},
					wantCmd:    "bash -lc ls",
					wantRender: true,
				},
				{
					name:       "first command-only completion",
					eventType:  "item.completed",
					item:       codexItem{Command: "bash -lc ls"},
					wantRender: false,
				},
				{
					name:       "completion for B",
					eventType:  "item.completed",
					item:       codexItem{ID: "cmd_B"},
					wantRender: false,
				},
				{
					name:       "second command-only completion",
					eventType:  "item.completed",
					item:       codexItem{Command: "bash -lc ls"},
					wantCmd:    "bash -lc ls",
					wantRender: true,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tracker codexCommandTracker
			for _, obs := range tt.observations {
				t.Run(obs.name, func(t *testing.T) {
					gotCmd, gotRender := tracker.Observe(
						obs.eventType, obs.item.ID, obs.item.Command,
					)
					assert.Equal(t, obs.wantCmd, gotCmd)
					assert.Equal(t, obs.wantRender, gotRender)
				})
			}
		})
	}
}
