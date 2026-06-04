package main

import "go.kenn.io/roborev/internal/agenthook"

var postAgentHook = agenthook.Post

func defaultAgentHookDaemonAddress() string {
	return agenthook.DefaultDaemonAddress()
}
