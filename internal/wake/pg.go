package wake

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/gorm"
)

// Channel is the Postgres NOTIFY channel signals travel on.
//
// One channel for every topic, with the topic as the payload, because a
// listener has to issue a LISTEN per channel and the set of topics is small
// enough that filtering in the process is cheaper than four subscriptions.
const Channel = "linq_stellar_wake"

// listenRetry is how long to wait before reconnecting a dropped listener. The
// cost of being disconnected is latency, not correctness — the polling loops
// carry on — so this is patient rather than aggressive.
const listenRetry = 5 * time.Second

// Notifier carries signals between processes over Postgres.
//
// The loops and the thing that most often makes work for them run as separate
// deployments: the API server provisions a deposit account and the worker is
// what watches it. An in-process channel cannot cross that gap, and adding a
// broker for four one-word messages would be a strange amount of machinery. A
// NOTIFY is one round-trip on a connection both processes already hold.
//
// Best-effort by design. A signal that does not arrive — no listener, a pooler
// in the way, a dropped connection — costs exactly the polling interval that
// existed before signals did.
type Notifier struct {
	DB  *gorm.DB
	Log *slog.Logger
}

// Notify publishes a topic to any listening process.
func (n Notifier) Notify(topic string) {
	if n.DB == nil {
		return
	}
	// Short timeout: this runs on the tail of a request or a settlement, and
	// telling someone else to look is never worth holding either up.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := n.DB.WithContext(ctx).
		Exec("SELECT pg_notify(?, ?)", Channel, topic).Error; err != nil {
		if n.Log != nil {
			n.Log.Debug("could not publish wake signal",
				"component", "wake", "topic", topic, "error", err)
		}
	}
}

// Listen delivers signals published by other processes into the bus, until the
// context is cancelled.
//
// It holds one connection open for the life of the process — LISTEN is a
// property of a session, so it cannot be borrowed from the pool per call. That
// also means it does not work through a connection pooler running in
// transaction mode, and nothing here can tell the difference between "nobody is
// signalling" and "the pooler is eating them". The visible symptom is the one
// this package exists to remove: work found by the polling sweep instead of on
// a signal. The start line below is what says which of those you are looking
// at.
func Listen(ctx context.Context, db *gorm.DB, bus *Bus, log *slog.Logger) {
	if db == nil || bus == nil {
		return
	}
	if log == nil {
		log = slog.Default()
	}

	sqlDB, err := db.DB()
	if err != nil {
		log.Warn("wake listener unavailable; loops will poll",
			"component", "wake", "error", err)
		return
	}

	for ctx.Err() == nil {
		if err := listenOnce(ctx, sqlDB, bus, log); err != nil && ctx.Err() == nil {
			log.Warn("wake listener dropped, reconnecting",
				"component", "wake", "retry_in", listenRetry, "error", err)
			select {
			case <-ctx.Done():
			case <-time.After(listenRetry):
			}
		}
	}
	log.Info("wake listener stopped", "component", "wake")
}

// listenOnce holds one session open and fans its notifications into the bus.
func listenOnce(ctx context.Context, sqlDB *sql.DB, bus *Bus, log *slog.Logger) error {
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("take a connection: %w", err)
	}
	defer conn.Close()

	// The whole listen loop runs inside Raw: database/sql only lends the
	// driver connection for the duration of this callback, and a LISTEN is
	// worth nothing once the session it was issued on has gone back to the
	// pool.
	return conn.Raw(func(driverConn any) error {
		pgConn, ok := driverConn.(*stdlib.Conn)
		if !ok {
			// Not Postgres — the test suite runs on SQLite, where there is
			// nothing to listen to and one process to signal.
			return backoffForever(ctx)
		}
		raw := pgConn.Conn()

		if _, err := raw.Exec(ctx, "LISTEN "+pgx.Identifier{Channel}.Sanitize()); err != nil {
			return fmt.Errorf("listen: %w", err)
		}
		log.Info("wake listener connected",
			"component", "wake", "channel", Channel)

		for {
			note, err := raw.WaitForNotification(ctx)
			if err != nil {
				if ctx.Err() != nil || errors.Is(err, context.Canceled) {
					return nil
				}
				return fmt.Errorf("wait: %w", err)
			}
			log.Debug("wake signal received",
				"component", "wake", "topic", note.Payload, "from_pid", note.PID)
			// Local only: this signal has already crossed the wire, and
			// bridging it again would publish it straight back.
			bus.SignalLocal(note.Payload)
		}
	})
}

// backoffForever parks until the context ends, for a database that cannot
// carry signals. Returning nil immediately would spin the reconnect loop.
func backoffForever(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
