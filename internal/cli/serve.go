package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/config"
	"github.com/HarshitBadhwar8/leadscore/internal/receiver"
)

// runServe: serve [--every [interval]]. The first SIGTERM (or Ctrl-C) shuts
// down in order: the running run saves, the receiver stores what it accepted,
// then serve exits 0. A second one quits at once.
func runServe(inv *invocation) int {
	every, timer := inv.flags["every"]
	var interval time.Duration
	if timer && every != "" {
		d, err := config.ParseDuration(every)
		if err != nil {
			return inv.fail(err)
		}
		interval = d
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
	defer signal.Stop(sig)
	go func() {
		select {
		case <-sig:
			signal.Stop(sig)
			cancel()
		case <-ctx.Done():
		}
	}()
	err := receiver.Serve(ctx, receiver.ServeOptions{
		ConfigPath: inv.flags["config"], RubricPath: inv.flags["rubric"],
		Timer: timer, Every: interval, Out: inv.stdout, Log: inv.stderr,
	})
	if err != nil {
		return inv.fail(err)
	}
	return exitOK
}

// healthzTimeout bounds `leadscore healthz`, under the compose health check's
// own timeout.
const healthzTimeout = 8 * time.Second

// runHealthz calls the local /healthz on receiver.port and exits 0 on 200
// (the compose health check). When leadscore.yml cannot be read it still
// tries $PORT, else 8080, so a broken file shows as unhealthy, not as a
// second error.
func runHealthz(inv *invocation) int {
	port := config.DefaultReceiverPort
	if c, err := config.Load(inv.configOptions()); err == nil {
		port = c.Receiver.Port
	} else if p, perr := strconv.Atoi(os.Getenv("PORT")); perr == nil && p > 0 {
		port = p
	}
	client := &http.Client{Timeout: healthzTimeout}
	resp, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/healthz")
	if err != nil {
		return inv.fail(fmt.Errorf("calling /healthz on port %d: %w", port, err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := printable(strings.TrimSpace(string(body)))
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(inv.stderr, "leadscore healthz: %d %s\n", resp.StatusCode, msg)
		return exitFail
	}
	fmt.Fprintln(inv.stdout, msg)
	return exitOK
}
