package metrics

// The database side of this module's numbers.
//
// One HTTP call can be several commands, and a slow command is the usual reason a
// call is slow — so the two are reported separately instead of guessing from the
// handler's latency. The monitor is attached once, in store.Open.

import (
	"context"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/event"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	// DBCommands counts database commands by name and outcome.
	DBCommands = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "provisioner_db_commands_total",
		Help: "MongoDB commands issued, by command and outcome.",
	}, []string{"command", "outcome"})

	// DBSeconds is how long one command took.
	DBSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "provisioner_db_command_seconds",
		Help:    "Time spent in one MongoDB command, by command.",
		Buckets: []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5},
	}, []string{"command"})
)

func init() { prometheus.MustRegister(DBCommands, DBSeconds) }

// maxTrackedCommands bounds the in-flight table. A terminal event always follows
// a start on the same connection, but a connection torn down mid-command might
// not — the cap keeps a pathological case from growing without bound.
const maxTrackedCommands = 4096

// Monitor returns the driver's command monitor. Pairing a start with its
// terminal event is by request id, which the driver makes unique per command.
func Monitor() *event.CommandMonitor {
	var (
		mu       sync.Mutex
		inFlight = map[int64]time.Time{}
	)
	return &event.CommandMonitor{
		Started: func(_ context.Context, e *event.CommandStartedEvent) {
			mu.Lock()
			if len(inFlight) >= maxTrackedCommands {
				for id := range inFlight {
					delete(inFlight, id)
					break
				}
			}
			inFlight[e.RequestID] = time.Now()
			mu.Unlock()
		},
		Succeeded: func(_ context.Context, e *event.CommandSucceededEvent) {
			finish(&mu, inFlight, e.RequestID, e.CommandName, "ok")
		},
		Failed: func(_ context.Context, e *event.CommandFailedEvent) {
			finish(&mu, inFlight, e.RequestID, e.CommandName, "error")
		},
	}
}

func finish(mu *sync.Mutex, inFlight map[int64]time.Time, id int64, command, outcome string) {
	mu.Lock()
	started, ok := inFlight[id]
	delete(inFlight, id)
	mu.Unlock()
	if !ok {
		// A command whose start was pruned: count it, but the latency is unknown.
		DBCommands.WithLabelValues(command, outcome).Inc()
		return
	}
	DBCommands.WithLabelValues(command, outcome).Inc()
	DBSeconds.WithLabelValues(command).Observe(time.Since(started).Seconds())
}
