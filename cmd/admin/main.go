package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"example.com/subscriptions/internal/config"
	"example.com/subscriptions/internal/hashring"
	"example.com/subscriptions/internal/store"
	"example.com/subscriptions/internal/topology"
	"example.com/subscriptions/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, "admin:", e)
		os.Exit(1)
	}
}
func run() error {
	path := flag.String("config", "configs/local.json", "current configuration")
	nextPath := flag.String("next", "", "next configuration for rebalance")
	command := flag.String("command", "plan", "migrate, init, plan, rebalance, acknowledge, gc-stale, cleanup")
	phase := flag.String("phase", "", "freeze, copy or activate")
	execute := flag.Bool("execute", false, "confirm a topology/data administration operation")
	flag.Parse()
	c, e := config.Load(*path)
	if e != nil {
		return e
	}
	ring, e := c.Ring()
	if e != nil {
		return e
	}
	var next config.Config
	var nextRing *hashring.Ring
	if *nextPath != "" {
		next, e = config.Load(*nextPath)
		if e != nil {
			return e
		}
		nextRing, e = next.Ring()
		if e != nil {
			return e
		}
	}
	if *command == "plan" {
		if nextRing == nil {
			return fmt.Errorf("-next is required")
		}
		moves, e := hashring.Plan(ring, nextRing)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"old": ring.Fingerprint, "next": nextRing.Fingerprint, "buckets": ring.Spec.Buckets, "moved": len(moves), "moves": moves})
	}
	if (*command == "rebalance" || *command == "init" || *command == "acknowledge" || *command == "gc-stale") && !*execute {
		return fmt.Errorf("review the plan/backup first, then pass -execute")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	s, e := store.Open(ctx, c, true)
	if e != nil {
		return e
	}
	defer s.Close()
	switch *command {
	case "migrate":
		for _, id := range ring.Spec.Shards {
			if e := migrations.Apply(ctx, s.Pools[id]); e != nil {
				return fmt.Errorf("shard %s: %w", id, e)
			}
			fmt.Println("migrated", id)
		}
		return nil
	case "init":
		return topology.Initialize(ctx, s)
	case "cleanup":
		return topology.Cleanup(ctx, s)
	case "acknowledge":
		return topology.Acknowledge(ctx, s)
	case "gc-stale":
		return topology.GCStale(ctx, s)
	case "rebalance":
		if nextRing == nil {
			return fmt.Errorf("-next is required")
		}
		// Preserve existing shard identities and endpoints for the duration of a move.
		for _, old := range c.Shards {
			for _, n := range next.Shards {
				if old.ID == n.ID {
					a, e := c.DSN(old, true)
					if e != nil {
						return e
					}
					b, e := next.DSN(n, true)
					if e != nil {
						return e
					}
					if a != b {
						return fmt.Errorf("existing shard %s changes endpoint; do endpoint failover separately", old.ID)
					}
				}
			}
		}
		newStore, e := store.Open(ctx, next, true)
		if e != nil {
			return e
		}
		defer newStore.Close()
		pools := make(map[string]*pgxpool.Pool)
		for id, p := range s.Pools {
			pools[id] = p
		}
		for id, p := range newStore.Pools {
			if pools[id] == nil {
				pools[id] = p
			}
		}
		return (topology.Rebalancer{Old: ring, Next: nextRing, Pools: pools}).Run(ctx, *phase)
	default:
		return fmt.Errorf("unknown command")
	}
}
