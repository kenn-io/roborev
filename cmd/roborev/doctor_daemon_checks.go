package main

import (
	"fmt"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/version"
)

func checkDoctorDaemon(env *doctorEnv) []doctorCheck {
	if !env.daemonUp() {
		c := doctorCheck{
			ID: "daemon.running", Category: "daemon", Status: doctorWarn,
			Summary: "daemon is not running; commits are not reviewed until it starts",
			Details: []string{env.pingErr.Error(), "checks that need the daemon were skipped"},
			Fix:     "roborev daemon start",
		}
		if daemon.IsDaemonAccessError(env.pingErr) {
			c.Summary = "cannot reach the daemon: access denied"
			c.Fix = "check auth_key and [daemon_tls] in the global config; if roborev runs in a sandbox, allow loopback or Unix socket access and retry"
		}
		return []doctorCheck{c}
	}

	out := []doctorCheck{{
		ID: "daemon.running", Category: "daemon", Status: doctorOK,
		Summary: fmt.Sprintf("daemon is running (pid %d)", env.ping.PID),
	}}
	if env.ping.Version != version.Version {
		out = append(out, doctorCheck{
			ID: "daemon.version", Category: "daemon", Status: doctorWarn,
			Summary: fmt.Sprintf("daemon runs %s but this CLI is %s", env.ping.Version, version.Version),
			Fix:     "roborev daemon restart",
		})
	}

	if status, err := env.daemon.Status(env.ctx); err != nil {
		out = append(out, doctorCheck{
			ID: "daemon.status", Category: "daemon", Status: doctorWarn,
			Summary: "daemon status is unavailable", Details: []string{err.Error()},
		})
	} else if status.QueuePaused {
		out = append(out, doctorCheck{
			ID: "daemon.queue_paused", Category: "daemon", Status: doctorWarn,
			Summary: fmt.Sprintf("review queue is paused; %d job(s) waiting", status.QueuedJobs),
			Fix:     "roborev unpause",
		})
	}

	health, err := env.daemon.Health(env.ctx)
	if err != nil {
		out = append(out, doctorCheck{
			ID: "daemon.health", Category: "daemon", Status: doctorWarn,
			Summary: "daemon health is unavailable", Details: []string{err.Error()},
		})
		return out
	}
	var unhealthy []string
	for _, comp := range health.Components {
		if !comp.Healthy {
			unhealthy = append(unhealthy, fmt.Sprintf("%s: %s", comp.Name, deref(comp.Message)))
		}
	}
	if len(unhealthy) > 0 {
		out = append(out, doctorCheck{
			ID: "daemon.health", Category: "daemon", Status: doctorWarn,
			Summary: "daemon reports unhealthy components", Details: unhealthy,
			Fix: "run 'roborev status' for detail; 'roborev daemon restart' clears stuck workers",
		})
	}
	if health.ErrorCount24H > 0 {
		c := doctorCheck{
			ID: "daemon.errors", Category: "daemon", Status: doctorInfo,
			Summary: fmt.Sprintf("daemon logged %s in the last 24 hours", plural(int(health.ErrorCount24H), "error")),
			Fix:     "run 'roborev log' or read errors.log in the roborev data directory",
		}
		for i, e := range health.RecentErrors {
			if i == 3 {
				break
			}
			c.Details = append(c.Details, fmt.Sprintf("%s: %s", e.Component, doctorFirstLine(e.Message)))
		}
		out = append(out, c)
	}
	return out
}
